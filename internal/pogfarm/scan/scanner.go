package scan

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/queue"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// Defaults of the Scanner fields left zero.
const (
	DefaultScanInterval    = time.Minute
	DefaultScanWorkers     = 1
	DefaultScanAttempts    = 5
	DefaultRetryBackoff    = 10 * time.Minute
	DefaultMaxRetryBackoff = 6 * time.Hour
)

// maxScanError is how much of a failure's message is stored in
// vods.scan_error. A failed chat download can carry a long tool output.
const maxScanError = 2000

// releaseTimeout bounds the write that hands a VOD back when a scan is
// interrupted by shutdown, which runs after the scan's context has ended.
const releaseTimeout = 5 * time.Second

// Scanner turns recorded VODs into candidates and render jobs. For each due
// VOD it fetches the chat and the viewer clips through the VOD's source,
// runs DetectSpikes and BuildCandidates, and stores the result with
// QueueCandidates.
//
// A VOD is due when its scan status is new, or when it is failed and its
// retry time (next_scan_at) has come. A failed VOD without a retry time has
// been given up and is never scanned again by the loop.
//
// The slow work, downloading the chat and calling the platform, runs outside
// every write transaction, so the job API and the bot keep writing to the
// store while a scan runs.
type Scanner struct {
	// Store holds the VODs. Required.
	Store *store.Store
	// Queue is the job queue on Store that render jobs go to. Required.
	Queue *queue.Queue
	// Sources are the platforms the VODs are fetched from. Required.
	Sources *source.Registry

	// Spike and Candidates tune the detection. Both must be valid; start
	// from DefaultSpikeParams and DefaultCandidateParams.
	Spike      SpikeParams
	Candidates CandidateParams

	// MaxAttempts is how many scans of one VOD are started before a VOD
	// whose scans keep failing is given up. Zero means DefaultScanAttempts.
	MaxAttempts int
	// RetryBackoff is the wait before the first retry of a failed scan. It
	// doubles with every further failure, up to MaxRetryBackoff. Zero
	// means DefaultRetryBackoff and DefaultMaxRetryBackoff.
	RetryBackoff    time.Duration
	MaxRetryBackoff time.Duration

	// Interval is how long Run waits, when no VOD is due, before it looks
	// again. Zero means DefaultScanInterval.
	Interval time.Duration
	// Workers is how many VODs Run scans at the same time. Zero means
	// DefaultScanWorkers, one.
	Workers int

	// Logger receives what Run does. Nil discards it.
	Logger *slog.Logger
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// NewScanner returns a scanner configured from the scan section of pogfarm's
// configuration.
func NewScanner(st *store.Store, q *queue.Queue, sources *source.Registry, c config.Scan, logger *slog.Logger) *Scanner {
	return &Scanner{
		Store:           st,
		Queue:           q,
		Sources:         sources,
		Spike:           SpikeParams(c.Spike),
		Candidates:      CandidateParams(c.Candidates),
		MaxAttempts:     c.MaxAttempts,
		RetryBackoff:    c.RetryBackoff,
		MaxRetryBackoff: c.MaxRetryBackoff,
		Interval:        c.Interval,
		Workers:         c.Workers,
		Logger:          logger,
	}
}

// ScanResult says what one scan did.
type ScanResult struct {
	// VOD is the VOD as the scan left it in the store.
	VOD store.VOD
	// Spikes is the number of chat spikes found.
	Spikes int
	// Queued counts the candidates and jobs created.
	Queued QueueResult
	// Err is why the scan failed, nil when it succeeded. The VOD is then
	// failed, with a retry time unless it was given up.
	Err error
}

// GaveUp reports whether the scan failed and the VOD will not be retried.
func (r ScanResult) GaveUp() bool {
	return r.Err != nil && r.VOD.ScanStatus == store.ScanFailed && r.VOD.NextScanAt.IsZero()
}

func (s *Scanner) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scanner) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (s *Scanner) maxAttempts() int {
	if s.MaxAttempts > 0 {
		return s.MaxAttempts
	}
	return DefaultScanAttempts
}

// backoff is the wait after the given number of failed attempts, at least
// one.
func (s *Scanner) backoff(attempts int) time.Duration {
	base, ceiling := s.RetryBackoff, s.MaxRetryBackoff
	if base <= 0 {
		base, ceiling = DefaultRetryBackoff, DefaultMaxRetryBackoff
	}
	if ceiling < base {
		ceiling = base
	}
	d := base
	for i := 1; i < attempts && d < ceiling; i++ {
		d *= 2
	}
	return min(d, ceiling)
}

func (s *Scanner) check() error {
	if s.Store == nil || s.Queue == nil || s.Sources == nil {
		return errors.New("scan: scanner needs a store, a queue and sources")
	}
	return errors.Join(s.Spike.Validate(), s.Candidates.Validate())
}

// Recover hands back every VOD left in scanning, which happens when pogfarm
// stopped in the middle of a scan. Such a VOD becomes new again, so the next
// pass scans it from the start, unless it has used up its attempts, in which
// case it is failed and given up. Rescanning is safe because QueueCandidates
// stores nothing twice. It returns how many VODs it handed back.
//
// Recover must not run while a scan is in progress: Run calls it once, before
// its workers start.
func (s *Scanner) Recover(ctx context.Context) (int, error) {
	if s.Store == nil {
		return 0, errors.New("scan: scanner needs a store")
	}
	res, err := s.Store.Writer().ExecContext(ctx, `
		UPDATE vods SET
			scan_status  = CASE WHEN scan_attempts >= ? THEN ? ELSE ? END,
			scan_error   = CASE WHEN scan_attempts >= ? THEN 'scan interrupted by a restart, attempts used up' ELSE scan_error END,
			next_scan_at = NULL,
			updated_at   = ?
		WHERE scan_status = ?`,
		s.maxAttempts(), store.ScanFailed, store.ScanNew,
		s.maxAttempts(), store.FormatTime(s.now()), store.ScanScanning)
	if err != nil {
		return 0, fmt.Errorf("scan: recover interrupted scans: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("scan: recover interrupted scans: %w", err)
	}
	return int(n), nil
}

// ScanNext takes the oldest due VOD, marks it scanning, counts the attempt
// and scans it. scanned is false when no VOD was due.
//
// A failure of the scan itself (the platform, the chat download, the
// detection) does not make ScanNext fail: it is recorded on the VOD and
// returned in the result's Err. The VOD is then retried after a backoff, or
// given up once MaxAttempts scans have been started or at once when the
// platform no longer has the VOD (source.ErrVODNotFound).
//
// The returned error is the context's when ctx ends, which hands the VOD
// back as new without counting the attempt, and otherwise a failure of the
// store or a misconfigured scanner.
func (s *Scanner) ScanNext(ctx context.Context) (res ScanResult, scanned bool, err error) {
	if err := s.check(); err != nil {
		return res, false, err
	}
	if err := ctx.Err(); err != nil {
		return res, false, err
	}
	vod, ok, err := s.claim(ctx)
	if err != nil || !ok {
		return res, false, err
	}

	spikes, queued, scanErr := s.scan(ctx, vod)
	if ctx.Err() != nil {
		s.release(ctx, vod.ID)
		return res, false, ctx.Err()
	}

	if scanErr == nil {
		err = s.finish(ctx, vod.ID)
	} else {
		err = s.fail(ctx, vod, scanErr)
	}
	if err != nil {
		return res, true, err
	}
	res = ScanResult{Spikes: spikes, Queued: queued, Err: scanErr}
	res.VOD, err = store.GetVOD(ctx, s.Store.Reader(), vod.ID)
	return res, true, err
}

// claim picks the oldest due VOD and marks it scanning in one statement, so
// two workers never take the same VOD.
func (s *Scanner) claim(ctx context.Context) (store.VOD, bool, error) {
	now := store.FormatTime(s.now())
	var id int64
	err := s.Store.Writer().QueryRowContext(ctx, `
		UPDATE vods SET scan_status = ?, scan_attempts = scan_attempts + 1, next_scan_at = NULL, updated_at = ?
		WHERE id = (
			SELECT id FROM vods
			WHERE (scan_status = ? AND (next_scan_at IS NULL OR next_scan_at <= ?))
			   OR (scan_status = ? AND next_scan_at IS NOT NULL AND next_scan_at <= ?)
			ORDER BY id LIMIT 1)
		RETURNING id`,
		store.ScanScanning, now, store.ScanNew, now, store.ScanFailed, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return store.VOD{}, false, nil
	}
	if err != nil {
		return store.VOD{}, false, fmt.Errorf("scan: claim a vod: %w", err)
	}
	vod, err := store.GetVOD(ctx, s.Store.Reader(), id)
	if err != nil {
		return store.VOD{}, false, fmt.Errorf("scan: claim vod %d: %w", id, err)
	}
	return vod, true, nil
}

// scan does the work of one scan. Only QueueCandidates writes, in its own
// short transaction.
func (s *Scanner) scan(ctx context.Context, vod store.VOD) (int, QueueResult, error) {
	src, err := s.Sources.Get(vod.Platform)
	if err != nil {
		return 0, QueueResult{}, err
	}
	streamer, err := store.GetStreamer(ctx, s.Store.Reader(), vod.StreamerID)
	if err != nil {
		return 0, QueueResult{}, err
	}
	sv := source.VOD{
		Platform:   vod.Platform,
		ID:         vod.PlatformVODID,
		Streamer:   api.Streamer{Login: streamer.Login, DisplayName: streamer.DisplayName},
		StreamerID: streamer.PlatformUserID,
		Title:      vod.Title,
		StartedAt:  vod.StartedAt,
		Duration:   seconds(vod.DurationSeconds),
		Ended:      true,
	}

	chat, err := src.Chat(ctx, sv)
	if err != nil {
		return 0, QueueResult{}, fmt.Errorf("fetch chat: %w", err)
	}
	clips, err := source.ViewerClips(ctx, src, sv)
	if errors.Is(err, source.ErrViewerClipsUnsupported) {
		clips, err = nil, nil
	}
	if err != nil {
		return 0, QueueResult{}, fmt.Errorf("list viewer clips: %w", err)
	}

	spikes, err := DetectSpikes(chat, s.Spike)
	if err != nil {
		return 0, QueueResult{}, err
	}
	cands, err := BuildCandidates(spikes, clips, sv.Duration, s.Candidates)
	if err != nil {
		return len(spikes), QueueResult{}, err
	}
	queued, err := QueueCandidates(ctx, s.Store, s.Queue, vod, cands)
	if err != nil {
		return len(spikes), QueueResult{}, err
	}
	return len(spikes), queued, nil
}

// finish marks a VOD scanned.
func (s *Scanner) finish(ctx context.Context, id int64) error {
	_, err := s.Store.Writer().ExecContext(ctx, `
		UPDATE vods SET scan_status = ?, scan_error = '', next_scan_at = NULL, updated_at = ?
		WHERE id = ? AND scan_status = ?`,
		store.ScanScanned, store.FormatTime(s.now()), id, store.ScanScanning)
	if err != nil {
		return fmt.Errorf("scan: mark vod %d scanned: %w", id, err)
	}
	return nil
}

// fail marks a VOD failed and records why. It sets a retry time unless the
// VOD is given up.
func (s *Scanner) fail(ctx context.Context, vod store.VOD, scanErr error) error {
	now := s.now()
	var next any // NULL: given up
	if !errors.Is(scanErr, source.ErrVODNotFound) && vod.ScanAttempts < s.maxAttempts() {
		next = store.FormatTime(now.Add(s.backoff(vod.ScanAttempts)))
	}
	_, err := s.Store.Writer().ExecContext(ctx, `
		UPDATE vods SET scan_status = ?, scan_error = ?, next_scan_at = ?, updated_at = ?
		WHERE id = ? AND scan_status = ?`,
		store.ScanFailed, truncateError(scanErr.Error()), next, store.FormatTime(now), vod.ID, store.ScanScanning)
	if err != nil {
		return fmt.Errorf("scan: mark vod %d failed: %w", vod.ID, err)
	}
	return nil
}

// release hands back a VOD whose scan was interrupted by shutdown, without
// counting the attempt. ctx has ended, so the write gets a short context of
// its own. If the write fails, Recover hands the VOD back on the next start.
func (s *Scanner) release(ctx context.Context, id int64) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	_, err := s.Store.Writer().ExecContext(wctx, `
		UPDATE vods SET scan_status = ?, scan_attempts = MAX(scan_attempts - 1, 0), updated_at = ?
		WHERE id = ? AND scan_status = ?`,
		store.ScanNew, store.FormatTime(s.now()), id, store.ScanScanning)
	if err != nil {
		s.logger().Warn("could not hand back an interrupted scan; it is recovered on the next start",
			"vod_id", id, "error", err)
	}
}

// Run scans due VODs until ctx ends. It first calls Recover, then runs
// Workers loops that each take one due VOD after the other with ScanNext and
// wait Interval when none is due. A failed scan is logged and recorded on
// its VOD and never stops the loop; neither does a failure of the store,
// after which the loop waits Interval and tries again.
//
// Run returns an error only when the scanner is misconfigured or Recover
// fails, before any scan starts. Otherwise it returns nil once ctx has ended
// and every scan in progress has stopped.
func (s *Scanner) Run(ctx context.Context) error {
	if err := s.check(); err != nil {
		return err
	}
	log := s.logger()
	n, err := s.Recover(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	if n > 0 {
		log.Info("handed back scans interrupted by a restart", "vods", n)
	}

	interval := s.Interval
	if interval <= 0 {
		interval = DefaultScanInterval
	}
	workers := s.Workers
	if workers <= 0 {
		workers = DefaultScanWorkers
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() { s.work(ctx, interval) })
	}
	wg.Wait()
	return nil
}

// work is one worker of Run.
func (s *Scanner) work(ctx context.Context, interval time.Duration) {
	log := s.logger()
	for {
		res, scanned, err := s.ScanNext(ctx)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil:
			log.Error("scanner pass failed", "error", err)
		case scanned:
			s.logResult(res)
			continue
		}
		if !sleep(ctx, interval) {
			return
		}
	}
}

func (s *Scanner) logResult(res ScanResult) {
	v := res.VOD
	attrs := []any{"vod_id", v.ID, "platform", v.Platform, "platform_vod_id", v.PlatformVODID, "attempt", v.ScanAttempts}
	switch {
	case res.Err == nil:
		s.logger().Info("vod scanned", append(attrs,
			"spikes", res.Spikes, "candidates", res.Queued.Candidates, "jobs", res.Queued.Jobs)...)
	case res.GaveUp():
		s.logger().Warn("vod scan failed, giving up", append(attrs, "error", res.Err)...)
	default:
		s.logger().Warn("vod scan failed, will retry", append(attrs, "error", res.Err, "next_scan_at", v.NextScanAt)...)
	}
}

// sleep waits for d and reports whether ctx is still alive.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// seconds converts stored seconds to a duration, to the millisecond.
func seconds(s float64) time.Duration {
	return time.Duration(math.Round(s*1000)) * time.Millisecond
}

// truncateError cuts a failure's message to what vods.scan_error keeps.
func truncateError(msg string) string {
	if len(msg) <= maxScanError {
		return msg
	}
	return strings.ToValidUTF8(msg[:maxScanError], "") + " …"
}

package upload

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// DailyWindow is the window of the daily cap: a target gets at most its
// daily_cap posts in any DailyWindow, counted by posted_at. A rolling window
// is at least as strict as a calendar day in whatever time zone the
// platform resets its own limits.
const DailyWindow = 24 * time.Hour

// Options configure a Loop.
type Options struct {
	// Store is the database. Required.
	Store *store.Store
	// DataDir is the data directory clip file paths are relative to.
	// Required.
	DataDir string
	// Config is the pogfarm.upload section, already validated.
	Config config.Upload
	// Targets are the implementations available. Every enabled target in
	// Config must have one; the others are ignored.
	Targets []Target
	// Composer builds the text of a post. Default: DefaultComposer.
	Composer Composer
	// Notifier tells the owner about posts and failures. Default: nobody is
	// told (it is still logged).
	Notifier Notifier
	// Now is the clock. Default time.Now.
	Now func() time.Time
	// Logger receives what the loop did. Default: discarded.
	Logger *slog.Logger
}

// Loop posts approved clips to the enabled targets. It is safe for
// concurrent use, but there must be only one Loop per database: a row it
// finds in uploading is taken to be left over from a crash unless this
// Loop is uploading it right now.
//
// Each enabled target is worked on by at most one goroutine at a time, so
// a slow, stuck or rate-limited target holds up only itself. A turn of one
// target (see Pass) does, in order:
//
//  1. Rows of the target left in uploading by an earlier run are settled,
//     never retried blindly: when the target implements Checker it is asked
//     whether the post exists, and the row becomes done (with the post id)
//     or goes back to pending; otherwise the row gets attention_at and
//     attention_reason (migration 0002), stays in uploading, is never
//     touched again by the loop and the owner is told to look. The owner
//     settles it with ResolveAttention. Because it is not done, the clip's
//     file is kept by the cleanup and the review cannot be undone.
//  2. If the target has had fewer than daily_cap posts in the last
//     DailyWindow and its last post is at least min_spacing ago, the oldest
//     due pending row (next_attempt_at unset or passed) of an approved clip
//     is marked uploading, with attempts+1, in its own short write
//     transaction whose UPDATE checks the status again.
//  3. The target is called outside any transaction, with the per-attempt
//     timeout. Success marks the row done with posted_at and the post id
//     and tells the owner the link. A permanent error marks it failed and
//     tells the owner. Another error puts it back to pending with
//     next_attempt_at by exponential backoff, or marks it failed and tells
//     the owner once it has had max_attempts. An unconfirmed error or a
//     timeout is settled as in step 1.
//
// When ctx is cancelled during an upload the row is left in uploading, to
// be settled as in step 1 on the next start.
type Loop struct {
	st       *store.Store
	dataDir  string
	cfg      config.Upload
	composer Composer
	notifier Notifier
	now      func() time.Time
	logger   *slog.Logger
	targets  []*targetState

	mu sync.Mutex // guards targetState.busy
}

type targetState struct {
	name       string
	target     Target
	dailyCap   int
	minSpacing time.Duration
	busy       bool
}

// New returns a Loop. It fails when the options are incomplete or an
// enabled target has no implementation.
func New(opts Options) (*Loop, error) {
	if opts.Store == nil {
		return nil, errors.New("upload: no store")
	}
	if opts.DataDir == "" {
		return nil, errors.New("upload: no data directory")
	}
	c := opts.Config
	switch {
	case c.MaxAttempts < 1:
		return nil, fmt.Errorf("upload: max attempts %d is below 1", c.MaxAttempts)
	case c.RetryBackoff <= 0 || c.MaxRetryBackoff < c.RetryBackoff:
		return nil, fmt.Errorf("upload: retry backoff %s up to %s is not valid", c.RetryBackoff, c.MaxRetryBackoff)
	case c.PollInterval <= 0:
		return nil, fmt.Errorf("upload: poll interval %s is not positive", c.PollInterval)
	case c.AttemptTimeout <= 0:
		return nil, fmt.Errorf("upload: attempt timeout %s is not positive", c.AttemptTimeout)
	}
	impls := make(map[string]Target, len(opts.Targets))
	for _, t := range opts.Targets {
		if t == nil {
			return nil, errors.New("upload: nil target")
		}
		if _, dup := impls[t.Name()]; dup {
			return nil, fmt.Errorf("upload: two targets are named %q", t.Name())
		}
		impls[t.Name()] = t
	}
	l := &Loop{
		st:       opts.Store,
		dataDir:  opts.DataDir,
		cfg:      c,
		composer: opts.Composer,
		notifier: opts.Notifier,
		now:      opts.Now,
		logger:   opts.Logger,
	}
	for _, tc := range c.Targets {
		if !tc.On() {
			continue
		}
		impl, ok := impls[tc.Name]
		if !ok {
			return nil, fmt.Errorf("upload: target %q is enabled in the config, but pogfarm has no implementation of it", tc.Name)
		}
		if tc.DailyCap < 1 || tc.MinSpacing < 0 {
			return nil, fmt.Errorf("upload: target %q: daily cap %d or spacing %s is not valid", tc.Name, tc.DailyCap, tc.MinSpacing)
		}
		l.targets = append(l.targets, &targetState{
			name: tc.Name, target: impl, dailyCap: tc.DailyCap, minSpacing: tc.MinSpacing,
		})
	}
	if l.composer == nil {
		l.composer = DefaultComposer(opts.Store)
	}
	if l.now == nil {
		l.now = time.Now
	}
	if l.logger == nil {
		l.logger = slog.New(slog.DiscardHandler)
	}
	return l, nil
}

// Run works through the uploads until ctx is cancelled: a turn of every
// idle target at the start, every poll interval, and right after any turn
// that did something, so a backlog drains without waiting for the timer.
// Turns run in goroutines of their own; Run returns once ctx is cancelled
// and they have stopped. Run it in its own goroutine.
func (l *Loop) Run(ctx context.Context) {
	ticker := time.NewTicker(l.cfg.PollInterval)
	defer ticker.Stop()
	l.run(ctx, ticker.C)
}

// run is Run with the timer passed in.
func (l *Loop) run(ctx context.Context, tick <-chan time.Time) {
	var wg sync.WaitGroup
	defer wg.Wait()
	wake := make(chan struct{}, 1)
	report := func(ts *targetState, worked bool, err error) {
		if err != nil && ctx.Err() == nil {
			l.logger.Error("upload: turn failed, will retry", "target", ts.name, "err", err)
		}
		if worked {
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}
	for {
		if ctx.Err() != nil {
			return
		}
		l.dispatch(ctx, &wg, report)
		select {
		case <-ctx.Done():
			return
		case <-tick:
		case <-wake:
		}
	}
}

// Pass gives every enabled target that is not busy one turn (see Loop):
// settle its rows left in uploading, then upload at most one clip. The
// targets' turns run at the same time; Pass returns when all have ended,
// with their errors joined.
func (l *Loop) Pass(ctx context.Context) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	l.dispatch(ctx, &wg, func(_ *targetState, _ bool, err error) {
		if err != nil {
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}
	})
	wg.Wait()
	return errors.Join(errs...)
}

// dispatch starts a turn of every enabled target that is not busy and
// returns without waiting for them. report is called at the end of each.
func (l *Loop) dispatch(ctx context.Context, wg *sync.WaitGroup, report func(*targetState, bool, error)) {
	for _, ts := range l.targets {
		if !l.acquire(ts) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			worked, err := l.turn(ctx, ts)
			l.release(ts)
			report(ts, worked, err)
		}()
	}
}

func (l *Loop) acquire(ts *targetState) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ts.busy {
		return false
	}
	ts.busy = true
	return true
}

func (l *Loop) release(ts *targetState) {
	l.mu.Lock()
	ts.busy = false
	l.mu.Unlock()
}

// row is the part of an uploads row the loop carries around.
type row struct {
	id       int64
	clipID   int64
	attempts int // including the current one
}

// turn is one turn of one target; the caller holds the target. worked
// reports whether it changed anything.
func (l *Loop) turn(ctx context.Context, ts *targetState) (worked bool, err error) {
	settled, settleErr := l.settleLeftovers(ctx, ts)
	if ctx.Err() != nil {
		return settled, settleErr
	}
	now := l.now().UTC()
	ok, err := l.allowed(ctx, ts, now)
	if err != nil || !ok {
		return settled, errors.Join(settleErr, err)
	}
	r, ok, err := l.claim(ctx, ts, now)
	if err != nil || !ok {
		return settled, errors.Join(settleErr, err)
	}
	return true, errors.Join(settleErr, l.attempt(ctx, ts, r))
}

// allowed reports whether the daily cap and the spacing of the target let
// it post at now.
func (l *Loop) allowed(ctx context.Context, ts *targetState, now time.Time) (bool, error) {
	var (
		count int
		last  sql.NullString
	)
	err := l.st.Reader().QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM uploads WHERE target_platform = ? AND posted_at > ?),
			(SELECT max(posted_at) FROM uploads WHERE target_platform = ?)`,
		ts.name, store.FormatTime(now.Add(-DailyWindow)), ts.name).Scan(&count, &last)
	if err != nil {
		return false, fmt.Errorf("upload: %s: read recent posts: %w", ts.name, err)
	}
	if count >= ts.dailyCap {
		return false, nil
	}
	if last.Valid && ts.minSpacing > 0 {
		lastAt, err := store.ParseTime(last.String)
		if err != nil {
			return false, fmt.Errorf("upload: %s: %w", ts.name, err)
		}
		if now.Before(lastAt.Add(ts.minSpacing)) {
			return false, nil
		}
	}
	return true, nil
}

// claim marks the oldest due pending row of an approved clip uploading and
// returns it. ok is false when there is none.
func (l *Loop) claim(ctx context.Context, ts *targetState, now time.Time) (r row, ok bool, err error) {
	stamp := store.FormatTime(now)
	err = l.st.WriteTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT u.id, u.clip_id, u.attempts FROM uploads u JOIN clips c ON c.id = u.clip_id
			WHERE u.target_platform = ? AND u.status = 'pending'
				AND (u.next_attempt_at IS NULL OR u.next_attempt_at <= ?)
				AND c.review_status = 'approved' AND c.file_removed_at IS NULL
			ORDER BY u.id LIMIT 1`, ts.name, stamp).Scan(&r.id, &r.clipID, &r.attempts)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE uploads SET status = 'uploading', attempts = attempts + 1, updated_at = ?
			WHERE id = ? AND status = 'pending'`, stamp, r.id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		ok = n == 1
		r.attempts++
		return nil
	})
	if err != nil {
		return row{}, false, fmt.Errorf("upload: %s: claim: %w", ts.name, err)
	}
	return r, ok, nil
}

// attempt uploads the claimed row r and records the outcome.
func (l *Loop) attempt(ctx context.Context, ts *targetState, r row) error {
	// Bookkeeping after the target was called must land even when ctx is
	// cancelled meanwhile, or a made post would go unrecorded.
	wctx := context.WithoutCancel(ctx)

	post, err := l.post(ctx, r.clipID, ts.name)
	if err != nil {
		if ctx.Err() != nil {
			// Stopping before the target was called: hand the row back as
			// it was.
			return l.unclaim(wctx, ts, r)
		}
		if IsPermanent(err) {
			return l.giveUp(wctx, ts, r, err.Error())
		}
		return l.retry(wctx, ts, r, err.Error())
	}

	actx, cancel := context.WithTimeout(ctx, l.cfg.AttemptTimeout)
	externalID, url, err := ts.target.Upload(actx, post)
	timedOut := errors.Is(actx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	cancel()
	switch {
	case err == nil:
		return l.done(wctx, ts, r, externalID, url, "")
	case ctx.Err() != nil:
		l.logger.Warn("upload: stopped during an upload; the row is settled at the next start",
			"target", ts.name, "upload", r.id, "clip", r.clipID)
		return nil
	case timedOut:
		return l.unsure(wctx, ts, r, post, fmt.Sprintf("the upload ran out of time after %s", l.cfg.AttemptTimeout))
	case IsUnconfirmed(err):
		return l.unsure(wctx, ts, r, post, "the target could not confirm the upload: "+err.Error())
	case IsPermanent(err):
		return l.giveUp(wctx, ts, r, err.Error())
	default:
		return l.retry(wctx, ts, r, err.Error())
	}
}

// post builds the Post of a clip to one target.
func (l *Loop) post(ctx context.Context, clipID int64, target string) (Post, error) {
	clip, err := store.GetClip(ctx, l.st.Reader(), clipID)
	if err != nil {
		return Post{}, fmt.Errorf("load clip: %w", err)
	}
	rel := filepath.FromSlash(clip.FilePath)
	if !filepath.IsLocal(rel) {
		return Post{}, Permanent(fmt.Errorf("clip file path %q is not inside the data directory", clip.FilePath))
	}
	abs, err := filepath.Abs(filepath.Join(l.dataDir, rel))
	if err != nil {
		return Post{}, fmt.Errorf("clip file path: %w", err)
	}
	title, description, tags, err := l.composer(ctx, clip, target)
	if err != nil {
		return Post{}, fmt.Errorf("compose the post: %w", err)
	}
	return Post{ClipID: clip.ID, FilePath: abs, Title: title, Description: description, Tags: tags}, nil
}

// settleLeftovers settles the rows of the target left in uploading by an
// earlier run. The caller holds the target, so none of them is in flight.
func (l *Loop) settleLeftovers(ctx context.Context, ts *targetState) (bool, error) {
	rows, err := l.st.Reader().QueryContext(ctx, `
		SELECT id, clip_id, attempts FROM uploads
		WHERE target_platform = ? AND status = 'uploading' AND attention_at IS NULL
		ORDER BY id`, ts.name)
	if err != nil {
		return false, fmt.Errorf("upload: %s: find interrupted uploads: %w", ts.name, err)
	}
	var left []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.clipID, &r.attempts); err != nil {
			rows.Close()
			return false, fmt.Errorf("upload: %s: find interrupted uploads: %w", ts.name, err)
		}
		left = append(left, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("upload: %s: find interrupted uploads: %w", ts.name, err)
	}

	var errs []error
	for _, r := range left {
		if ctx.Err() != nil {
			break
		}
		const reason = "pogfarm stopped during the upload"
		post, err := l.post(ctx, r.clipID, ts.name)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if _, ok := ts.target.(Checker); ok {
				// Without the post the target cannot be asked.
				errs = append(errs, l.needsAttention(ctx, ts, r, reason+" (and the post could not be built to check: "+err.Error()+")"))
				continue
			}
		}
		errs = append(errs, l.unsure(ctx, ts, r, post, reason))
	}
	return len(left) > 0, errors.Join(errs...)
}

// unsure settles a row whose upload may or may not have been made.
func (l *Loop) unsure(ctx context.Context, ts *targetState, r row, post Post, reason string) error {
	checker, ok := ts.target.(Checker)
	if !ok {
		return l.needsAttention(ctx, ts, r, reason)
	}
	actx, cancel := context.WithTimeout(ctx, l.cfg.AttemptTimeout)
	externalID, url, found, err := checker.Find(actx, post)
	cancel()
	if err != nil {
		// Left in uploading: the next turn asks again.
		return fmt.Errorf("upload: %s: check upload %d of clip %d with the target: %w", ts.name, r.id, r.clipID, err)
	}
	if found {
		return l.done(ctx, ts, r, externalID, url, " (found on the target after: "+reason+")")
	}
	return l.retry(ctx, ts, r, reason+"; the target has no such post")
}

// done records the post of r.
func (l *Loop) done(ctx context.Context, ts *targetState, r row, externalID, url, note string) error {
	stamp := store.FormatTime(l.now())
	if err := l.settle(ctx, ts, r, "mark posted", `
		UPDATE uploads SET status = 'done', posted_at = ?, external_post_id = ?, last_error = '',
			next_attempt_at = NULL, updated_at = ?
		WHERE id = ? AND status = 'uploading' AND attention_at IS NULL`,
		stamp, externalID, stamp, r.id); err != nil {
		return err
	}
	link := url
	if link == "" {
		link = "post id " + externalID
	}
	l.logger.Info("upload: posted", "target", ts.name, "upload", r.id, "clip", r.clipID, "external_id", externalID, "url", url)
	l.notify(ctx, fmt.Sprintf("Clip %d is posted to %s: %s%s", r.clipID, ts.name, link, note))
	return nil
}

// retry puts r back to pending after a failure that may pass, or gives it
// up when it has had all its attempts.
func (l *Loop) retry(ctx context.Context, ts *targetState, r row, cause string) error {
	if r.attempts >= l.cfg.MaxAttempts {
		return l.giveUp(ctx, ts, r, fmt.Sprintf("%s (after %d attempts)", cause, r.attempts))
	}
	now := l.now()
	next := now.Add(l.backoff(r.attempts))
	if err := l.settle(ctx, ts, r, "schedule retry", `
		UPDATE uploads SET status = 'pending', last_error = ?, next_attempt_at = ?, updated_at = ?
		WHERE id = ? AND status = 'uploading' AND attention_at IS NULL`,
		cause, store.FormatTime(next), store.FormatTime(now), r.id); err != nil {
		return err
	}
	l.logger.Warn("upload: failed, will retry", "target", ts.name, "upload", r.id, "clip", r.clipID,
		"attempts", r.attempts, "next_attempt_at", next, "err", cause)
	return nil
}

// backoff is the wait after the given number of attempts: RetryBackoff
// doubled for every attempt after the first, up to MaxRetryBackoff.
func (l *Loop) backoff(attempts int) time.Duration {
	d := l.cfg.RetryBackoff
	for i := 1; i < attempts && d < l.cfg.MaxRetryBackoff; i++ {
		d *= 2
	}
	return min(d, l.cfg.MaxRetryBackoff)
}

// giveUp marks r failed for good and tells the owner.
func (l *Loop) giveUp(ctx context.Context, ts *targetState, r row, cause string) error {
	stamp := store.FormatTime(l.now())
	if err := l.settle(ctx, ts, r, "mark failed", `
		UPDATE uploads SET status = 'failed', last_error = ?, next_attempt_at = NULL, updated_at = ?
		WHERE id = ? AND status = 'uploading' AND attention_at IS NULL`,
		cause, stamp, r.id); err != nil {
		return err
	}
	l.logger.Error("upload: failed for good", "target", ts.name, "upload", r.id, "clip", r.clipID, "err", cause)
	l.notify(ctx, fmt.Sprintf("Clip %d could not be posted to %s and is given up: %s", r.clipID, ts.name, cause))
	return nil
}

// needsAttention holds r for the owner and tells them.
func (l *Loop) needsAttention(ctx context.Context, ts *targetState, r row, reason string) error {
	stamp := store.FormatTime(l.now())
	if err := l.settle(ctx, ts, r, "hold for a manual look", `
		UPDATE uploads SET attention_at = ?, attention_reason = ?, last_error = ?, updated_at = ?
		WHERE id = ? AND status = 'uploading' AND attention_at IS NULL`,
		stamp, reason, reason, stamp, r.id); err != nil {
		return err
	}
	l.logger.Warn("upload: may or may not be posted, held for a manual look",
		"target", ts.name, "upload", r.id, "clip", r.clipID, "reason", reason)
	l.notify(ctx, fmt.Sprintf(
		"Clip %d may or may not be posted to %s (%s). It will not be retried by itself: check %s and settle upload %d by hand.",
		r.clipID, ts.name, reason, ts.name, r.id))
	return nil
}

// unclaim hands r back as it was before the claim: pending, without the
// attempt.
func (l *Loop) unclaim(ctx context.Context, ts *targetState, r row) error {
	return l.settle(ctx, ts, r, "hand back", `
		UPDATE uploads SET status = 'pending', attempts = max(attempts - 1, 0), updated_at = ?
		WHERE id = ? AND status = 'uploading' AND attention_at IS NULL`,
		store.FormatTime(l.now()), r.id)
}

// settle runs one conditional UPDATE of r in its own short transaction. A
// row that is no longer in the expected state is an error: nothing else
// should change a row this loop holds.
func (l *Loop) settle(ctx context.Context, ts *targetState, r row, what, query string, args ...any) error {
	res, err := l.st.Writer().ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("upload: %s: %s upload %d: %w", ts.name, what, r.id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("upload: %s: %s upload %d: %w", ts.name, what, r.id, err)
	}
	if n == 0 {
		return fmt.Errorf("upload: %s: %s upload %d: the row is no longer uploading", ts.name, what, r.id)
	}
	return nil
}

func (l *Loop) notify(ctx context.Context, text string) {
	if l.notifier == nil {
		return
	}
	if err := l.notifier.Notify(ctx, text); err != nil {
		l.logger.Error("upload: could not tell the owner", "err", err, "text", text)
	}
}

// ResolveAttention settles an upload held for a manual look (see Loop),
// after the owner checked the target by hand. With posted it becomes done
// at now with the given post id; without, it goes back to pending and is
// uploaded again. It fails, wrapping store.ErrNotFound, when the upload is
// not held.
func ResolveAttention(ctx context.Context, st *store.Store, uploadID int64, posted bool, externalID string, now time.Time) error {
	stamp := store.FormatTime(now)
	query := `
		UPDATE uploads SET status = 'pending', next_attempt_at = NULL, attention_at = NULL,
			attention_reason = '', last_error = '', updated_at = ?
		WHERE id = ? AND status = 'uploading' AND attention_at IS NOT NULL`
	args := []any{stamp, uploadID}
	if posted {
		query = `
			UPDATE uploads SET status = 'done', posted_at = ?, external_post_id = ?, next_attempt_at = NULL,
				attention_at = NULL, attention_reason = '', last_error = '', updated_at = ?
			WHERE id = ? AND status = 'uploading' AND attention_at IS NOT NULL`
		args = []any{stamp, externalID, stamp, uploadID}
	}
	res, err := st.Writer().ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("upload: resolve upload %d: %w", uploadID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("upload: resolve upload %d: %w", uploadID, err)
	} else if n == 0 {
		return fmt.Errorf("upload: resolve upload %d: not held for a manual look: %w", uploadID, store.ErrNotFound)
	}
	return nil
}

// DefaultComposer is the composer used until the real one is wired in: the
// title is the streamer's display name and the date of the VOD, with no
// description and no tags.
func DefaultComposer(st *store.Store) Composer {
	return func(ctx context.Context, clip store.Clip, _ string) (string, string, []string, error) {
		var name, started string
		err := st.Reader().QueryRowContext(ctx, `
			SELECT s.display_name, v.started_at
			FROM jobs j
			JOIN candidates c ON c.id = j.candidate_id
			JOIN vods v ON v.id = c.vod_id
			JOIN streamers s ON s.id = v.streamer_id
			WHERE j.id = ?`, clip.JobID).Scan(&name, &started)
		if err != nil {
			return "", "", nil, fmt.Errorf("upload: compose clip %d: %w", clip.ID, err)
		}
		t, err := store.ParseTime(started)
		if err != nil {
			return "", "", nil, fmt.Errorf("upload: compose clip %d: %w", clip.ID, err)
		}
		return strings.TrimSpace(name + " " + t.Format("2006-01-02")), "", nil, nil
	}
}

package scan

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/queue"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source/sourcetest"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// No test here sleeps or waits on the wall clock. The scanner's clock is
// scanClock, moved by hand, and the loops are observed through their logs.

var scanT0 = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

type scanClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *scanClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *scanClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// scanSource is a fake source whose Chat can fail or block.
type scanSource struct {
	*sourcetest.FakeWithClips

	mu       sync.Mutex
	chatErrs []error // returned by the next Chat calls, one each
	// onChat, when set, runs at the start of every Chat call; an error
	// it returns is Chat's.
	onChat func(ctx context.Context) error
}

func (s *scanSource) Chat(ctx context.Context, vod source.VOD) ([]source.ChatMessage, error) {
	s.mu.Lock()
	hook := s.onChat
	var err error
	if len(s.chatErrs) > 0 {
		err, s.chatErrs = s.chatErrs[0], s.chatErrs[1:]
	}
	s.mu.Unlock()
	if hook != nil {
		if err := hook(ctx); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	return s.FakeWithClips.Chat(ctx, vod)
}

func (s *scanSource) failChat(errs ...error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chatErrs = append(s.chatErrs, errs...)
}

func (s *scanSource) chatCalls() int {
	n := 0
	for _, c := range s.Calls() {
		if c.Method == "Chat" {
			n++
		}
	}
	return n
}

type scanFixture struct {
	t        *testing.T
	store    *store.Store
	queue    *queue.Queue
	src      *scanSource
	clock    *scanClock
	scanner  *Scanner
	streamer store.Streamer
}

func newScanFixture(t *testing.T) *scanFixture {
	t.Helper()
	st := discoverStore(t)
	clock := &scanClock{now: scanT0}
	q, err := queue.New(st, queue.Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	src := &scanSource{FakeWithClips: sourcetest.NewWithClips(discoverPlatform)}
	reg, err := source.NewRegistry(src)
	if err != nil {
		t.Fatal(err)
	}
	streamer, err := store.InsertStreamer(context.Background(), st.Writer(), store.Streamer{
		Platform: discoverPlatform, PlatformUserID: "id-streamer", Login: "streamer", DisplayName: "Streamer", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &scanFixture{
		t: t, store: st, queue: q, src: src, clock: clock, streamer: streamer,
		scanner: &Scanner{
			Store:           st,
			Queue:           q,
			Sources:         reg,
			Spike:           DefaultSpikeParams(),
			Candidates:      DefaultCandidateParams(),
			MaxAttempts:     3,
			RetryBackoff:    time.Minute,
			MaxRetryBackoff: 3 * time.Minute,
			Now:             clock.Now,
		},
	}
}

// scanVODLength is the length of every test VOD.
const scanVODLength = 30 * time.Minute

// addVOD records a VOD with the given status and attempts, and gives the
// fake source a chat with one burst at 10:00 and a viewer clip at 20:00, so
// a scan yields two candidates.
func (f *scanFixture) addVOD(id string, status store.ScanStatus, attempts int) store.VOD {
	f.t.Helper()
	started := scanT0.Add(-2 * time.Hour)
	v, err := store.InsertVOD(context.Background(), f.store.Writer(), store.VOD{
		Platform: discoverPlatform, PlatformVODID: id, StreamerID: f.streamer.ID, Title: "stream " + id,
		DurationSeconds: scanVODLength.Seconds(), StartedAt: started, EndedAt: started.Add(scanVODLength),
		ScanStatus: status, ScanAttempts: attempts,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.src.AddVOD(source.VOD{
		ID: id, Streamer: api.Streamer{Login: "streamer", DisplayName: "Streamer"}, StreamerID: "id-streamer",
		StartedAt: started, Duration: scanVODLength, Ended: true,
	})
	f.src.SetChat(id, scanChat())
	f.src.SetViewerClips(id, []source.ViewerClip{{Offset: 20 * time.Minute, Duration: 30 * time.Second, Views: 50, Title: "wow"}})
	return v
}

// scanChat is one message every 10 seconds for the whole VOD, and 40
// messages within the window at 10:00.
func scanChat() []source.ChatMessage {
	var chat []source.ChatMessage
	for off := time.Duration(0); off < scanVODLength; off += 10 * time.Second {
		chat = append(chat, source.ChatMessage{Offset: off, User: "regular", Text: "hi"})
	}
	for i := range 40 {
		chat = append(chat, source.ChatMessage{
			Offset: 10*time.Minute + time.Duration(i)*100*time.Millisecond, User: fmt.Sprintf("u%d", i), Text: "LUL",
		})
	}
	return chat
}

func (f *scanFixture) next() (ScanResult, bool) {
	f.t.Helper()
	res, scanned, err := f.scanner.ScanNext(context.Background())
	if err != nil {
		f.t.Fatalf("ScanNext: %v", err)
	}
	return res, scanned
}

func (f *scanFixture) vod(id int64) store.VOD {
	f.t.Helper()
	v, err := store.GetVOD(context.Background(), f.store.Reader(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

func (f *scanFixture) count(table string) int {
	f.t.Helper()
	var n int
	if err := f.store.Reader().QueryRowContext(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// wantNothingDue fails the test if a pass finds a VOD to scan.
func (f *scanFixture) wantNothingDue() {
	f.t.Helper()
	if res, scanned := f.next(); scanned {
		f.t.Fatalf("a pass scanned vod %d, want nothing due", res.VOD.ID)
	}
}

func TestScannerScansNewVOD(t *testing.T) {
	f := newScanFixture(t)
	v := f.addVOD("v1", store.ScanNew, 0)

	res, scanned := f.next()
	if !scanned || res.Err != nil {
		t.Fatalf("scanned %v, err %v", scanned, res.Err)
	}
	if res.VOD.ID != v.ID || res.VOD.ScanStatus != store.ScanScanned || res.VOD.ScanAttempts != 1 ||
		res.VOD.ScanError != "" || !res.VOD.NextScanAt.IsZero() {
		t.Errorf("vod after the scan: %+v", res.VOD)
	}
	if res.Spikes != 1 || res.Queued != (QueueResult{Candidates: 2, Jobs: 2}) {
		t.Errorf("spikes %d, queued %+v; want 1 spike, 2 candidates and 2 jobs", res.Spikes, res.Queued)
	}
	if got := f.count("candidates"); got != 2 {
		t.Errorf("%d candidates stored, want 2", got)
	}

	// The jobs are claimable and carry the VOD.
	job, err := f.queue.Claim(context.Background(), "clipper-1")
	if err != nil {
		t.Fatal(err)
	}
	if job.VODID != "v1" || job.Platform != discoverPlatform || job.Streamer.Login != "streamer" {
		t.Errorf("claimed job: %+v", job)
	}

	// A second pass does nothing new.
	f.wantNothingDue()
	if f.count("candidates") != 2 || f.count("jobs") != 2 || f.src.chatCalls() != 1 {
		t.Errorf("second pass changed something: %d candidates, %d jobs, %d chat fetches",
			f.count("candidates"), f.count("jobs"), f.src.chatCalls())
	}
}

func TestScannerSourceWithoutViewerClips(t *testing.T) {
	f := newScanFixture(t)
	plain := sourcetest.New("plain")
	reg, err := source.NewRegistry(plain)
	if err != nil {
		t.Fatal(err)
	}
	f.scanner.Sources = reg
	streamer, err := store.InsertStreamer(context.Background(), f.store.Writer(), store.Streamer{
		Platform: "plain", PlatformUserID: "p1", Login: "p", DisplayName: "P", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertVOD(context.Background(), f.store.Writer(), store.VOD{
		Platform: "plain", PlatformVODID: "pv", StreamerID: streamer.ID, DurationSeconds: scanVODLength.Seconds(),
		StartedAt: scanT0, EndedAt: scanT0.Add(scanVODLength),
	}); err != nil {
		t.Fatal(err)
	}
	plain.AddVOD(source.VOD{ID: "pv", Streamer: api.Streamer{Login: "p"}, StartedAt: scanT0, Duration: scanVODLength, Ended: true})
	plain.SetChat("pv", scanChat())

	res, scanned := f.next()
	if !scanned || res.Err != nil || res.VOD.ScanStatus != store.ScanScanned {
		t.Fatalf("scanned %v, result %+v", scanned, res)
	}
	if res.Queued != (QueueResult{Candidates: 1, Jobs: 1}) {
		t.Errorf("queued %+v, want the chat spike alone", res.Queued)
	}
}

func TestScannerRetriesWithBackoffThenGivesUp(t *testing.T) {
	f := newScanFixture(t)
	v := f.addVOD("v1", store.ScanNew, 0)
	boom := errors.New("chat download broke")
	f.src.failChat(boom, boom, boom, boom)

	// Attempt n fails and is retried after 1m, 2m; the third is the last.
	for attempt, wait := range []time.Duration{time.Minute, 2 * time.Minute} {
		attempt++
		res, scanned := f.next()
		if !scanned || !errors.Is(res.Err, boom) || res.GaveUp() {
			t.Fatalf("attempt %d: scanned %v, err %v, gave up %v", attempt, scanned, res.Err, res.GaveUp())
		}
		got := f.vod(v.ID)
		if got.ScanStatus != store.ScanFailed || got.ScanAttempts != attempt ||
			!strings.Contains(got.ScanError, "chat download broke") || !got.NextScanAt.Equal(f.clock.Now().Add(wait)) {
			t.Fatalf("attempt %d: vod %+v, want failed with retry at now+%s", attempt, got, wait)
		}

		f.clock.Advance(wait - time.Millisecond)
		f.wantNothingDue()
		f.clock.Advance(time.Millisecond)
	}

	res, scanned := f.next()
	if !scanned || !res.GaveUp() {
		t.Fatalf("third attempt: scanned %v, result %+v; want given up", scanned, res)
	}
	got := f.vod(v.ID)
	if got.ScanStatus != store.ScanFailed || got.ScanAttempts != 3 || !got.NextScanAt.IsZero() ||
		!strings.Contains(got.ScanError, "chat download broke") {
		t.Errorf("given-up vod: %+v", got)
	}

	f.clock.Advance(24 * time.Hour)
	f.wantNothingDue()
	if f.count("jobs") != 0 {
		t.Errorf("a failed scan queued jobs")
	}
}

func TestScannerRetrySucceeds(t *testing.T) {
	f := newScanFixture(t)
	v := f.addVOD("v1", store.ScanNew, 0)
	f.src.failChat(errors.New("flaky"))

	if res, _ := f.next(); res.Err == nil {
		t.Fatal("first attempt did not fail")
	}
	f.clock.Advance(time.Minute)
	res, scanned := f.next()
	if !scanned || res.Err != nil {
		t.Fatalf("retry: scanned %v, err %v", scanned, res.Err)
	}
	got := f.vod(v.ID)
	if got.ScanStatus != store.ScanScanned || got.ScanAttempts != 2 || got.ScanError != "" || !got.NextScanAt.IsZero() {
		t.Errorf("vod after a successful retry: %+v", got)
	}
}

func TestScannerVODNotFoundGivesUpAtOnce(t *testing.T) {
	f := newScanFixture(t)
	v := f.addVOD("v1", store.ScanNew, 0)
	f.src.RemoveVOD("v1")
	f.scanner.MaxAttempts = 10

	res, scanned := f.next()
	if !scanned || !errors.Is(res.Err, source.ErrVODNotFound) || !res.GaveUp() {
		t.Fatalf("scanned %v, err %v, gave up %v", scanned, res.Err, res.GaveUp())
	}
	if got := f.vod(v.ID); got.ScanStatus != store.ScanFailed || got.ScanAttempts != 1 || !got.NextScanAt.IsZero() {
		t.Errorf("vod: %+v", got)
	}
	f.clock.Advance(24 * time.Hour)
	f.wantNothingDue()
}

func TestScannerOneFailureDoesNotStopTheOthers(t *testing.T) {
	f := newScanFixture(t)
	bad := f.addVOD("v1", store.ScanNew, 0)
	good := f.addVOD("v2", store.ScanNew, 0)
	f.src.failChat(errors.New("only the first"))

	if res, _ := f.next(); res.VOD.ID != bad.ID || res.Err == nil {
		t.Fatalf("first pass: %+v", res)
	}
	if res, _ := f.next(); res.VOD.ID != good.ID || res.Err != nil {
		t.Fatalf("second pass: %+v", res)
	}
}

func TestScannerBackoffIsCapped(t *testing.T) {
	s := &Scanner{RetryBackoff: time.Minute, MaxRetryBackoff: 5 * time.Minute}
	for attempts, want := range map[int]time.Duration{1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 4: 5 * time.Minute, 60: 5 * time.Minute} {
		if got := s.backoff(attempts); got != want {
			t.Errorf("backoff(%d) = %s, want %s", attempts, got, want)
		}
	}
	if got := (&Scanner{}).backoff(1); got != DefaultRetryBackoff {
		t.Errorf("default backoff = %s", got)
	}
}

func TestScannerRecoversStuckScans(t *testing.T) {
	f := newScanFixture(t)
	stuck := f.addVOD("v1", store.ScanScanning, 1)
	worn := f.addVOD("v2", store.ScanScanning, 3) // MaxAttempts is 3
	done := f.addVOD("v3", store.ScanScanned, 1)

	// While a VOD is scanning, a pass leaves it alone.
	f.wantNothingDue()

	n, err := f.scanner.Recover(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("Recover: %d, %v; want 2", n, err)
	}
	if got := f.vod(stuck.ID); got.ScanStatus != store.ScanNew || got.ScanAttempts != 1 {
		t.Errorf("stuck vod after recovery: %+v", got)
	}
	if got := f.vod(worn.ID); got.ScanStatus != store.ScanFailed || !got.NextScanAt.IsZero() ||
		!strings.Contains(got.ScanError, "interrupted") {
		t.Errorf("vod out of attempts after recovery: %+v", got)
	}
	if got := f.vod(done.ID); got.ScanStatus != store.ScanScanned {
		t.Errorf("scanned vod touched: %+v", got)
	}

	res, scanned := f.next()
	if !scanned || res.VOD.ID != stuck.ID || res.Err != nil || res.VOD.ScanAttempts != 2 {
		t.Fatalf("resumed scan: scanned %v, %+v", scanned, res)
	}
	f.wantNothingDue()
}

// While a scan waits for its chat, the store takes writes: a job is
// enqueued and claimed, as the job API would.
func TestScannerSlowScanDoesNotBlockTheQueue(t *testing.T) {
	f := newScanFixture(t)
	f.addVOD("v1", store.ScanNew, 0)
	other := f.addVOD("v2", store.ScanScanned, 1)
	cand, err := store.InsertCandidate(context.Background(), f.store.Writer(), store.Candidate{
		Platform: discoverPlatform, VODID: other.ID, StartSeconds: 0, EndSeconds: 30, Score: 1, Signal: store.SignalChatSpike,
	})
	if err != nil {
		t.Fatal(err)
	}

	started, release := make(chan struct{}), make(chan struct{})
	f.src.onChat = func(context.Context) error {
		close(started)
		<-release
		return nil
	}
	type outcome struct {
		res     ScanResult
		scanned bool
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		res, scanned, err := f.scanner.ScanNext(context.Background())
		done <- outcome{res, scanned, err}
	}()

	<-started
	ctx := context.Background()
	if _, _, err := f.queue.Enqueue(ctx, cand.ID); err != nil {
		t.Fatalf("enqueue during a scan: %v", err)
	}
	job, err := f.queue.Claim(ctx, "clipper-1")
	if err != nil || job.VODID != "v2" {
		t.Fatalf("claim during a scan: %+v, %v", job, err)
	}
	close(release)

	out := <-done
	if out.err != nil || !out.scanned || out.res.Err != nil || out.res.VOD.ScanStatus != store.ScanScanned {
		t.Fatalf("slow scan: %+v", out)
	}
}

func TestScannerCancelledScanHandsTheVODBack(t *testing.T) {
	f := newScanFixture(t)
	v := f.addVOD("v1", store.ScanNew, 0)
	ctx, cancel := context.WithCancel(context.Background())
	f.src.onChat = func(ctx context.Context) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}

	_, scanned, err := f.scanner.ScanNext(ctx)
	if !errors.Is(err, context.Canceled) || scanned {
		t.Fatalf("ScanNext: scanned %v, err %v; want context.Canceled", scanned, err)
	}
	if got := f.vod(v.ID); got.ScanStatus != store.ScanNew || got.ScanAttempts != 0 || got.ScanError != "" {
		t.Errorf("vod after an interrupted scan: %+v", got)
	}
}

func TestScannerRejectsBadSetup(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	broken := *f.scanner
	broken.Queue = nil
	if err := broken.Run(ctx); err == nil {
		t.Error("Run without a queue: no error")
	}
	if _, _, err := broken.ScanNext(ctx); err == nil {
		t.Error("ScanNext without a queue: no error")
	}

	broken = *f.scanner
	broken.Spike.Window = 0
	if err := broken.Run(ctx); err == nil || !strings.Contains(err.Error(), "window") {
		t.Errorf("Run with invalid spike parameters: err = %v", err)
	}
}

// logSink is a slog handler that hands every record's message to a channel.
type logSink struct{ msgs chan string }

func newLogSink() *logSink { return &logSink{msgs: make(chan string, 1000)} }

func (s *logSink) Enabled(context.Context, slog.Level) bool { return true }
func (s *logSink) Handle(_ context.Context, r slog.Record) error {
	s.msgs <- r.Message
	return nil
}
func (s *logSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *logSink) WithGroup(string) slog.Handler      { return s }

// await reads messages until n of them equal want. If they never come, the
// test runs into go test's timeout.
func (s *logSink) await(t *testing.T, want string, n int) {
	t.Helper()
	for n > 0 {
		msg := <-s.msgs
		if msg == want {
			n--
		}
	}
}

// Run recovers the stuck scan, scans every due VOD once with two workers,
// waits for more and stops on cancel.
func TestScannerRunScansAndStopsOnCancel(t *testing.T) {
	f := newScanFixture(t)
	vods := []store.VOD{
		f.addVOD("v1", store.ScanScanning, 1),
		f.addVOD("v2", store.ScanNew, 0),
		f.addVOD("v3", store.ScanNew, 0),
	}
	sink := newLogSink()
	f.scanner.Logger = slog.New(sink)
	f.scanner.Workers = 2
	f.scanner.Interval = time.Hour // the test ends long before a second look

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.scanner.Run(ctx) }()

	sink.await(t, "vod scanned", len(vods))
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, v := range vods {
		got := f.vod(v.ID)
		if got.ScanStatus != store.ScanScanned || got.ScanAttempts != v.ScanAttempts+1 {
			t.Errorf("vod %d: %+v", i, got)
		}
	}
	if got, want := f.count("jobs"), 2*len(vods); got != want {
		t.Errorf("%d jobs, want %d", got, want)
	}
}

func TestScannerRunStopsOnCancelWhileIdle(t *testing.T) {
	f := newScanFixture(t)
	f.scanner.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.scanner.Run(ctx); err != nil {
		t.Errorf("Run with an ended context: %v", err)
	}

	// A worker that is waiting for its next look stops too.
	sink := newLogSink()
	f.scanner.Logger = slog.New(sink)
	f.addVOD("v1", store.ScanNew, 0)
	ctx, cancel = context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.scanner.Run(ctx) }()
	sink.await(t, "vod scanned", 1)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

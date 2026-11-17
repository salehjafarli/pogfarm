package scan

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source/sourcetest"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// discoverPlatform is the platform name the tests use. Discovery treats it as
// an opaque name.
const discoverPlatform api.Platform = "testplat"

// discoverNow is the clock of every test pass.
var discoverNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// discoverSource is a fake source whose listing can fail for single logins.
type discoverSource struct {
	*sourcetest.Fake

	mu   sync.Mutex
	errs map[string]error
	// onList, when set, runs at the start of every listing.
	onList func(login string)
}

func (s *discoverSource) RecentVODs(ctx context.Context, login string) ([]source.VOD, error) {
	s.mu.Lock()
	err, hook := s.errs[login], s.onList
	s.mu.Unlock()
	if hook != nil {
		hook(login)
	}
	if err != nil {
		return nil, err
	}
	return s.Fake.RecentVODs(ctx, login)
}

func (s *discoverSource) failFor(login string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errs == nil {
		s.errs = make(map[string]error)
	}
	if err == nil {
		delete(s.errs, login)
		return
	}
	s.errs[login] = err
}

// listed returns the logins the fake itself was asked to list, in order.
func (s *discoverSource) listed() []string {
	var logins []string
	for _, c := range s.Calls() {
		if c.Method == "RecentVODs" {
			logins = append(logins, c.Arg)
		}
	}
	return logins
}

func discoverStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// discoverSetup returns a discoverer over an empty store and an empty fake
// source, with the given logins on the allowlist, all enabled.
func discoverSetup(t *testing.T, logins ...string) (*Discoverer, *discoverSource, *store.Store) {
	t.Helper()
	src := &discoverSource{Fake: sourcetest.New(discoverPlatform)}
	reg, err := source.NewRegistry(src)
	if err != nil {
		t.Fatal(err)
	}
	st := discoverStore(t)
	d := &Discoverer{
		Store:     st,
		Sources:   reg,
		MaxVODAge: 72 * time.Hour,
		Now:       func() time.Time { return discoverNow },
	}
	for _, login := range logins {
		d.Allowlist = append(d.Allowlist, discoverEntry(login, true))
	}
	return d, src, st
}

func discoverEntry(login string, enabled bool) config.Streamer {
	return config.Streamer{Platform: discoverPlatform, Login: login, Enabled: &enabled}
}

// discoverVOD is an ended two-hour VOD of the streamer that started the given
// time before discoverNow. The streamer's permanent id is "id-<login>".
func discoverVOD(login, id string, ago time.Duration) source.VOD {
	return source.VOD{
		ID:         id,
		Streamer:   api.Streamer{Login: login, DisplayName: "Display " + login},
		StreamerID: "id-" + login,
		Title:      "title of " + id,
		StartedAt:  discoverNow.Add(-ago),
		Duration:   2 * time.Hour,
		Ended:      true,
	}
}

func discoverPass(t *testing.T, d *Discoverer) DiscoverResult {
	t.Helper()
	res, err := d.Discover(context.Background())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	return res
}

// discoverVODIDs returns the platform ids of all stored VODs, in row order.
func discoverVODIDs(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.Reader().QueryContext(context.Background(), `SELECT platform_vod_id FROM vods ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func discoverStreamerRow(t *testing.T, st *store.Store, login string) store.Streamer {
	t.Helper()
	s, err := store.GetStreamerByPlatformID(context.Background(), st.Reader(), discoverPlatform, "id-"+login)
	if err != nil {
		t.Fatalf("streamer %s: %v", login, err)
	}
	return s
}

func discoverStreamerCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.Reader().QueryRowContext(context.Background(), `SELECT count(*) FROM streamers`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func discoverRecordedIDs(res DiscoverResult) []string {
	ids := []string{}
	for _, v := range res.Recorded {
		ids = append(ids, v.PlatformVODID)
	}
	return ids
}

func TestDiscoverRecordsNewVODOnce(t *testing.T) {
	d, src, st := discoverSetup(t, "alice")
	vod := discoverVOD("alice", "v1", 5*time.Hour)
	src.AddVOD(vod)

	res := discoverPass(t, d)
	if got := discoverRecordedIDs(res); !slices.Equal(got, []string{"v1"}) {
		t.Fatalf("first pass recorded %v, want [v1]", got)
	}
	if res.Known != 0 || res.Live != 0 || res.TooOld != 0 || res.Disabled != 0 || len(res.Failures) != 0 {
		t.Errorf("first pass: unexpected result %+v", res)
	}

	streamer := discoverStreamerRow(t, st, "alice")
	if streamer.Platform != discoverPlatform || streamer.Login != "alice" ||
		streamer.DisplayName != "Display alice" || !streamer.Enabled {
		t.Errorf("stored streamer: %+v", streamer)
	}
	row, err := store.GetVODByPlatformID(context.Background(), st.Reader(), discoverPlatform, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != res.Recorded[0].ID || row.StreamerID != streamer.ID || row.Title != "title of v1" ||
		row.DurationSeconds != 7200 || !row.StartedAt.Equal(vod.StartedAt) ||
		!row.EndedAt.Equal(vod.StartedAt.Add(2*time.Hour)) ||
		row.ScanStatus != store.ScanNew || row.ScanAttempts != 0 {
		t.Errorf("stored vod: %+v", row)
	}

	// The second pass sees the same VOD and records nothing.
	res = discoverPass(t, d)
	if len(res.Recorded) != 0 || res.Known != 1 {
		t.Errorf("second pass: recorded %v, known %d; want nothing new and 1 known", discoverRecordedIDs(res), res.Known)
	}
	if got := discoverVODIDs(t, st); !slices.Equal(got, []string{"v1"}) {
		t.Errorf("stored vods = %v, want [v1]", got)
	}
	if n := discoverStreamerCount(t, st); n != 1 {
		t.Errorf("stored streamers = %d, want 1", n)
	}
	after, err := store.GetVODByPlatformID(context.Background(), st.Reader(), discoverPlatform, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if after != row {
		t.Errorf("the second pass changed the stored vod:\n%+v\nwas\n%+v", after, row)
	}
}

func TestDiscoverDoesNotTouchAVODTheScannerHasTaken(t *testing.T) {
	d, src, st := discoverSetup(t, "alice")
	src.AddVOD(discoverVOD("alice", "v1", 5*time.Hour))
	res := discoverPass(t, d)

	row := res.Recorded[0]
	row.ScanStatus = store.ScanScanned
	row.ScanAttempts = 1
	if err := store.UpdateVOD(context.Background(), st.Writer(), row); err != nil {
		t.Fatal(err)
	}

	res = discoverPass(t, d)
	if len(res.Recorded) != 0 || res.Known != 1 {
		t.Errorf("unexpected result %+v", res)
	}
	got, err := store.GetVOD(context.Background(), st.Reader(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ScanStatus != store.ScanScanned || got.ScanAttempts != 1 {
		t.Errorf("scan state was reset: %+v", got)
	}
}

func TestDiscoverRecordsOldestFirst(t *testing.T) {
	d, src, st := discoverSetup(t, "alice")
	src.AddVOD(discoverVOD("alice", "middle", 20*time.Hour))
	src.AddVOD(discoverVOD("alice", "newest", 5*time.Hour))
	src.AddVOD(discoverVOD("alice", "oldest", 40*time.Hour))

	res := discoverPass(t, d)
	want := []string{"oldest", "middle", "newest"}
	if got := discoverRecordedIDs(res); !slices.Equal(got, want) {
		t.Errorf("recorded %v, want %v", got, want)
	}
	if got := discoverVODIDs(t, st); !slices.Equal(got, want) {
		t.Errorf("stored %v, want %v", got, want)
	}
}

func TestDiscoverSkipsLiveVODUntilItEnds(t *testing.T) {
	d, src, st := discoverSetup(t, "alice")
	src.AddVOD(discoverVOD("alice", "done", 30*time.Hour))
	live := discoverVOD("alice", "live", 3*time.Hour)
	live.Ended = false
	live.Duration = 3 * time.Hour
	src.AddVOD(live)

	res := discoverPass(t, d)
	if got := discoverRecordedIDs(res); !slices.Equal(got, []string{"done"}) || res.Live != 1 {
		t.Fatalf("while live: recorded %v, live %d; want [done] and 1", got, res.Live)
	}
	// Still live on the next pass: still not recorded.
	res = discoverPass(t, d)
	if len(res.Recorded) != 0 || res.Live != 1 || res.Known != 1 {
		t.Fatalf("still live: %+v", res)
	}
	if got := discoverVODIDs(t, st); !slices.Equal(got, []string{"done"}) {
		t.Fatalf("stored %v, want [done]", got)
	}

	// The stream ends an hour later; the VOD has its final length.
	live.Ended = true
	live.Duration = 4 * time.Hour
	src.AddVOD(live)
	res = discoverPass(t, d)
	if got := discoverRecordedIDs(res); !slices.Equal(got, []string{"live"}) || res.Live != 0 {
		t.Fatalf("after the stream: recorded %v, live %d; want [live] and 0", got, res.Live)
	}
	if got := res.Recorded[0]; got.DurationSeconds != 4*3600 || got.ScanStatus != store.ScanNew ||
		!got.EndedAt.Equal(live.StartedAt.Add(4*time.Hour)) {
		t.Errorf("recorded vod: %+v", got)
	}

	res = discoverPass(t, d)
	if len(res.Recorded) != 0 || res.Known != 2 {
		t.Errorf("pass after recording: %+v", res)
	}
}

func TestDiscoverStoresStreamerWhoIsOnlyLive(t *testing.T) {
	d, src, st := discoverSetup(t, "alice")
	live := discoverVOD("alice", "live", time.Hour)
	live.Ended = false
	src.AddVOD(live)

	res := discoverPass(t, d)
	if len(res.Recorded) != 0 || res.Live != 1 {
		t.Fatalf("unexpected result %+v", res)
	}
	if s := discoverStreamerRow(t, st, "alice"); !s.Enabled {
		t.Errorf("streamer should be stored and enabled: %+v", s)
	}
}

func TestDiscoverSkipsOldVODs(t *testing.T) {
	d, src, st := discoverSetup(t, "alice")
	src.AddVOD(discoverVOD("alice", "ancient", 30*24*time.Hour))
	src.AddVOD(discoverVOD("alice", "just-over", 72*time.Hour+time.Second))
	src.AddVOD(discoverVOD("alice", "at-limit", 72*time.Hour))
	src.AddVOD(discoverVOD("alice", "fresh", time.Hour))

	res := discoverPass(t, d)
	if got := discoverRecordedIDs(res); !slices.Equal(got, []string{"at-limit", "fresh"}) || res.TooOld != 2 {
		t.Fatalf("recorded %v, too old %d; want [at-limit fresh] and 2", got, res.TooOld)
	}

	// Without a limit the old ones are recorded too.
	d.MaxVODAge = 0
	res = discoverPass(t, d)
	if got := discoverRecordedIDs(res); !slices.Equal(got, []string{"ancient", "just-over"}) || res.TooOld != 0 || res.Known != 2 {
		t.Errorf("no limit: recorded %v, result %+v", got, res)
	}
	if got := discoverVODIDs(t, st); len(got) != 4 {
		t.Errorf("stored %v, want 4 vods", got)
	}
}

func TestDiscoverSkipsDisabledAndUnlistedStreamers(t *testing.T) {
	d, src, st := discoverSetup(t, "alice")
	d.Allowlist = append(d.Allowlist, discoverEntry("bob", false))
	src.AddVOD(discoverVOD("alice", "a1", time.Hour))
	src.AddVOD(discoverVOD("bob", "b1", time.Hour))     // switched off
	src.AddVOD(discoverVOD("mallory", "m1", time.Hour)) // not on the allowlist

	res := discoverPass(t, d)
	if got := discoverRecordedIDs(res); !slices.Equal(got, []string{"a1"}) || len(res.Failures) != 0 {
		t.Fatalf("recorded %v, failures %v; want [a1] and none", got, res.Failures)
	}
	if got := src.listed(); !slices.Equal(got, []string{"alice"}) {
		t.Errorf("listed %v; only alice may be asked for", got)
	}
	if n := discoverStreamerCount(t, st); n != 1 {
		t.Errorf("stored streamers = %d, want 1", n)
	}
}

func TestDiscoverEmptyAllowlist(t *testing.T) {
	d, src, st := discoverSetup(t)
	src.AddVOD(discoverVOD("alice", "a1", time.Hour))

	res := discoverPass(t, d)
	if len(res.Recorded) != 0 || len(src.Calls()) != 0 || len(discoverVODIDs(t, st)) != 0 {
		t.Errorf("an empty allowlist must do nothing: %+v, calls %v", res, src.Calls())
	}
}

func TestDiscoverReportsUnknownLoginAndGoesOn(t *testing.T) {
	d, src, _ := discoverSetup(t, "alice", "ghost", "carol")
	src.AddVOD(discoverVOD("alice", "a1", time.Hour))
	src.AddVOD(discoverVOD("carol", "c1", time.Hour))
	src.failFor("ghost", source.ErrStreamerNotFound)

	res := discoverPass(t, d)
	if got := discoverRecordedIDs(res); !slices.Equal(got, []string{"a1", "c1"}) {
		t.Errorf("recorded %v, want [a1 c1]", got)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("failures = %v, want one", res.Failures)
	}
	f := res.Failures[0]
	if f.Platform != discoverPlatform || f.Login != "ghost" || !errors.Is(f.Err, source.ErrStreamerNotFound) {
		t.Errorf("failure = %+v", f)
	}
	if !errors.Is(f, source.ErrStreamerNotFound) || f.Error() != "discover testplat/ghost: source: streamer not found" {
		t.Errorf("failure as an error: %v", f)
	}
}

func TestDiscoverSourceErrorForOneStreamerDoesNotStopTheOthers(t *testing.T) {
	d, src, st := discoverSetup(t, "alice", "bob", "carol")
	for _, login := range []string{"alice", "bob", "carol"} {
		src.AddVOD(discoverVOD(login, login+"-1", time.Hour))
	}
	boom := errors.New("api: status 500")
	src.failFor("alice", boom)

	res := discoverPass(t, d)
	if got := discoverRecordedIDs(res); !slices.Equal(got, []string{"bob-1", "carol-1"}) {
		t.Errorf("recorded %v, want [bob-1 carol-1]", got)
	}
	if len(res.Failures) != 1 || res.Failures[0].Login != "alice" || !errors.Is(res.Failures[0].Err, boom) {
		t.Errorf("failures = %v", res.Failures)
	}

	// Once the source works again the missed VOD is recorded.
	src.failFor("alice", nil)
	res = discoverPass(t, d)
	if got := discoverRecordedIDs(res); !slices.Equal(got, []string{"alice-1"}) || len(res.Failures) != 0 || res.Known != 2 {
		t.Errorf("after recovery: recorded %v, result %+v", got, res)
	}
	if got := discoverVODIDs(t, st); len(got) != 3 {
		t.Errorf("stored %v, want 3 vods", got)
	}
}

func TestDiscoverUnknownPlatformIsAFailure(t *testing.T) {
	d, src, _ := discoverSetup(t, "alice")
	on := true
	d.Allowlist = slices.Insert(d.Allowlist, 0, config.Streamer{Platform: "elsewhere", Login: "zed", Enabled: &on})
	src.AddVOD(discoverVOD("alice", "a1", time.Hour))

	res := discoverPass(t, d)
	if got := discoverRecordedIDs(res); !slices.Equal(got, []string{"a1"}) {
		t.Errorf("recorded %v, want [a1]", got)
	}
	if len(res.Failures) != 1 || res.Failures[0].Platform != "elsewhere" ||
		!errors.Is(res.Failures[0].Err, source.ErrUnknownPlatform) {
		t.Errorf("failures = %v", res.Failures)
	}
}

func TestDiscoverRejectsUnusableListings(t *testing.T) {
	tests := []struct {
		name   string
		change func(v *source.VOD)
	}{
		{"no vod id", func(v *source.VOD) { v.ID = "" }},
		{"no streamer id", func(v *source.VOD) { v.StreamerID = "" }},
		{"another streamer's vod", func(v *source.VOD) { v.StreamerID = "id-somebody-else" }},
		{"no start time", func(v *source.VOD) { v.StartedAt = time.Time{} }},
		{"negative duration", func(v *source.VOD) { v.Duration = -time.Second }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, src, st := discoverSetup(t, "alice", "bob")
			src.AddVOD(discoverVOD("alice", "good", time.Hour))
			bad := discoverVOD("alice", "bad", 10*time.Hour)
			tt.change(&bad)
			src.AddVOD(bad)
			src.AddVOD(discoverVOD("bob", "b1", time.Hour))

			res := discoverPass(t, d)
			if len(res.Failures) != 1 || res.Failures[0].Login != "alice" {
				t.Fatalf("failures = %v, want one for alice", res.Failures)
			}
			// Nothing of the broken listing is recorded; bob is unaffected.
			if got := discoverVODIDs(t, st); !slices.Equal(got, []string{"b1"}) {
				t.Errorf("stored %v, want [b1]", got)
			}
		})
	}
}

func TestDiscoverSyncsAllowlistIntoStore(t *testing.T) {
	d, src, st := discoverSetup(t, "alice", "bob")
	src.AddVOD(discoverVOD("alice", "a1", 10*time.Hour))
	src.AddVOD(discoverVOD("bob", "b1", 10*time.Hour))
	discoverPass(t, d)
	if !discoverStreamerRow(t, st, "alice").Enabled || !discoverStreamerRow(t, st, "bob").Enabled {
		t.Fatal("both streamers should be stored enabled")
	}

	// bob is switched off in the config: his row is switched off, and his
	// new VOD is not recorded.
	d.Allowlist = []config.Streamer{discoverEntry("alice", true), discoverEntry("bob", false)}
	src.AddVOD(discoverVOD("bob", "b2", time.Hour))
	res := discoverPass(t, d)
	if res.Disabled != 1 || len(res.Recorded) != 0 {
		t.Errorf("after switching bob off: %+v", res)
	}
	if discoverStreamerRow(t, st, "bob").Enabled || !discoverStreamerRow(t, st, "alice").Enabled {
		t.Error("bob should be disabled and alice enabled")
	}
	if res = discoverPass(t, d); res.Disabled != 0 {
		t.Errorf("nothing left to switch off, got %d", res.Disabled)
	}

	// alice is removed from the config altogether.
	d.Allowlist = []config.Streamer{discoverEntry("bob", false)}
	res = discoverPass(t, d)
	if res.Disabled != 1 || discoverStreamerRow(t, st, "alice").Enabled {
		t.Errorf("after removing alice: %+v", res)
	}
	if got := src.listed(); slices.Contains(got[2:], "bob") || len(got) != 4 {
		t.Errorf("listed %v; bob may only be asked for in the first pass", got)
	}

	// bob is switched on again: the row is enabled again and b2 recorded.
	d.Allowlist = []config.Streamer{discoverEntry("bob", true)}
	res = discoverPass(t, d)
	if got := discoverRecordedIDs(res); !slices.Equal(got, []string{"b2"}) || res.Known != 1 {
		t.Errorf("after switching bob on: recorded %v, result %+v", got, res)
	}
	if !discoverStreamerRow(t, st, "bob").Enabled {
		t.Error("bob should be enabled again")
	}
	if n := discoverStreamerCount(t, st); n != 2 {
		t.Errorf("stored streamers = %d, want 2", n)
	}
}

func TestDiscoverUpdatesRenamedStreamer(t *testing.T) {
	d, src, st := discoverSetup(t, "alice")
	src.AddVOD(discoverVOD("alice", "a1", 10*time.Hour))
	discoverPass(t, d)
	before := discoverStreamerRow(t, st, "alice")

	// The account is renamed on the platform and in the config. Its
	// permanent id stays.
	src.RemoveVOD("a1")
	renamed := discoverVOD("alicia", "a2", time.Hour)
	renamed.StreamerID = "id-alice"
	renamed.Streamer.DisplayName = "Alicia"
	src.AddVOD(renamed)
	d.Allowlist = []config.Streamer{discoverEntry("alicia", true)}

	res := discoverPass(t, d)
	if got := discoverRecordedIDs(res); !slices.Equal(got, []string{"a2"}) {
		t.Fatalf("recorded %v, want [a2]", got)
	}
	after := discoverStreamerRow(t, st, "alice")
	if after.ID != before.ID || after.Login != "alicia" || after.DisplayName != "Alicia" || !after.Enabled {
		t.Errorf("streamer row after the rename: %+v", after)
	}
	if res.Recorded[0].StreamerID != before.ID {
		t.Errorf("the new vod should belong to the same streamer row")
	}
	if n := discoverStreamerCount(t, st); n != 1 {
		t.Errorf("stored streamers = %d, want 1", n)
	}
}

func TestDiscoverCancelledBeforeStart(t *testing.T) {
	d, src, st := discoverSetup(t, "alice")
	src.AddVOD(discoverVOD("alice", "a1", time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := d.Discover(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if len(res.Recorded) != 0 || len(src.Calls()) != 0 || len(discoverVODIDs(t, st)) != 0 {
		t.Errorf("a cancelled pass must do nothing: %+v", res)
	}
}

func TestDiscoverCancelledMidPass(t *testing.T) {
	d, src, st := discoverSetup(t, "alice", "bob", "carol")
	for _, login := range []string{"alice", "bob", "carol"} {
		src.AddVOD(discoverVOD(login, login+"-1", time.Hour))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src.onList = func(login string) {
		if login == "bob" {
			cancel()
		}
	}

	res, err := d.Discover(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	// alice was done before the cancellation; bob's listing was cut short
	// and is not reported as a failure of bob; carol was never asked for.
	if got := discoverRecordedIDs(res); !slices.Equal(got, []string{"alice-1"}) || len(res.Failures) != 0 {
		t.Errorf("recorded %v, failures %v; want [alice-1] and none", got, res.Failures)
	}
	if got := src.listed(); !slices.Equal(got, []string{"alice", "bob"}) {
		t.Errorf("listed %v, want [alice bob]", got)
	}
	if got := discoverVODIDs(t, st); !slices.Equal(got, []string{"alice-1"}) {
		t.Errorf("stored %v, want [alice-1]", got)
	}

	// A later pass picks up where this one stopped.
	src.onList = nil
	res = discoverPass(t, d)
	if got := discoverRecordedIDs(res); !slices.Equal(got, []string{"bob-1", "carol-1"}) {
		t.Errorf("next pass recorded %v, want [bob-1 carol-1]", got)
	}
}

func TestDiscoverStoreFailureEndsThePass(t *testing.T) {
	d, src, st := discoverSetup(t, "alice")
	src.AddVOD(discoverVOD("alice", "a1", time.Hour))
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	res, err := d.Discover(context.Background())
	if err == nil {
		t.Fatal("want an error from a closed store")
	}
	if len(res.Recorded) != 0 || len(res.Failures) != 0 {
		t.Errorf("unexpected result %+v", res)
	}
}

func TestDiscoverNeedsStoreAndSources(t *testing.T) {
	if _, err := (&Discoverer{}).Discover(context.Background()); err == nil {
		t.Error("want an error without a store and sources")
	}
}

// Two discoverers over one store, as after a restart that overlaps the old
// process, record every VOD exactly once between them.
func TestDiscoverConcurrentPassesRecordEachVODOnce(t *testing.T) {
	d, src, st := discoverSetup(t, "alice", "bob")
	const perStreamer = 25
	for _, login := range []string{"alice", "bob"} {
		for i := range perStreamer {
			src.AddVOD(discoverVOD(login, login+"-"+string(rune('a'+i)), time.Duration(i+1)*time.Hour))
		}
	}

	const passes = 4
	results := make([]DiscoverResult, passes)
	errs := make([]error, passes)
	var wg sync.WaitGroup
	for i := range passes {
		other := *d
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = other.Discover(context.Background())
		}()
	}
	wg.Wait()

	recorded, known := 0, 0
	seen := make(map[string]int)
	for i, res := range results {
		if errs[i] != nil {
			t.Fatalf("pass %d: %v", i, errs[i])
		}
		if len(res.Failures) != 0 {
			t.Errorf("pass %d: failures %v", i, res.Failures)
		}
		recorded += len(res.Recorded)
		known += res.Known
		for _, v := range res.Recorded {
			seen[v.PlatformVODID]++
		}
	}
	if recorded != 2*perStreamer || known != (passes-1)*2*perStreamer {
		t.Errorf("recorded %d and known %d; want %d and %d", recorded, known, 2*perStreamer, (passes-1)*2*perStreamer)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("vod %s was recorded %d times", id, n)
		}
	}
	if got := discoverVODIDs(t, st); len(got) != 2*perStreamer {
		t.Errorf("stored %d vods, want %d", len(got), 2*perStreamer)
	}
	if n := discoverStreamerCount(t, st); n != 2 {
		t.Errorf("stored streamers = %d, want 2", n)
	}
}

package scan

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/queue"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// candPlatform is not Twitch, so the tests see that the VOD's platform is
// carried rather than assumed.
const candPlatform api.Platform = "testplatform"

var candT0 = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// candFixture is a fresh database with one streamer and two VODs, and a
// queue on it.
type candFixture struct {
	t     *testing.T
	store *store.Store
	queue *queue.Queue
	vod   store.VOD
	other store.VOD
}

func newCandFixture(t *testing.T) *candFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	q, err := queue.New(st, queue.Options{Now: func() time.Time { return candT0 }})
	if err != nil {
		t.Fatal(err)
	}
	streamer, err := store.InsertStreamer(ctx, st.Writer(), store.Streamer{
		Platform:       candPlatform,
		PlatformUserID: "42",
		Login:          "streamer",
		DisplayName:    "Streamer",
		Enabled:        true,
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &candFixture{t: t, store: st, queue: q}
	for i, id := range []string{"vod-1", "vod-2"} {
		v, err := store.InsertVOD(ctx, st.Writer(), store.VOD{
			Platform:        candPlatform,
			PlatformVODID:   id,
			StreamerID:      streamer.ID,
			Title:           "a stream",
			DurationSeconds: 3600,
			StartedAt:       candT0.Add(-5 * time.Hour),
			EndedAt:         candT0.Add(-4 * time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			f.vod = v
		} else {
			f.other = v
		}
	}
	return f
}

// build returns the candidates of four separate moments, of which the top
// keep are kept.
func (f *candFixture) build(keep int) []Candidate {
	f.t.Helper()
	cands, err := BuildCandidates(
		[]Spike{candSpike(100*candS, 4), candSpike(500*candS, 6), candSpike(900*candS, 2)},
		[]source.ViewerClip{candClip(95*candS, 20*candS, 10), candClip(2000*candS, 30*candS, 0)},
		time.Hour,
		candParams(func(p *CandidateParams) { p.MaxPerVOD = keep }),
	)
	if err != nil {
		f.t.Fatal(err)
	}
	if len(cands) != 4 {
		f.t.Fatalf("built %d candidates, want 4: %+v", len(cands), cands)
	}
	return cands
}

func (f *candFixture) run(vod store.VOD, cands []Candidate) QueueResult {
	f.t.Helper()
	res, err := QueueCandidates(context.Background(), f.store, f.queue, vod, cands)
	if err != nil {
		f.t.Fatalf("QueueCandidates: %v", err)
	}
	return res
}

// candRow is a stored candidate with its job, if any.
type candRow struct {
	cand store.Candidate
	job  *store.Job
}

// rows returns the stored candidates of a VOD, ordered by start.
func (f *candFixture) rows(vod store.VOD) []candRow {
	f.t.Helper()
	ctx := context.Background()
	rs, err := f.store.Reader().QueryContext(ctx,
		`SELECT id FROM candidates WHERE vod_id = ? ORDER BY start_seconds`, vod.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	var ids []int64
	for rs.Next() {
		var id int64
		if err := rs.Scan(&id); err != nil {
			f.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rs.Close(); err != nil {
		f.t.Fatal(err)
	}
	var out []candRow
	for _, id := range ids {
		c, err := store.GetCandidate(ctx, f.store.Reader(), id)
		if err != nil {
			f.t.Fatal(err)
		}
		r := candRow{cand: c}
		if j, err := store.GetJobByCandidate(ctx, f.store.Reader(), id); err == nil {
			r.job = &j
		}
		out = append(out, r)
	}
	return out
}

func (f *candFixture) count(table string) int {
	f.t.Helper()
	var n int
	if err := f.store.Reader().QueryRowContext(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestQueueCandidates(t *testing.T) {
	f := newCandFixture(t)
	cands := f.build(2)

	if got := f.run(f.vod, cands); got != (QueueResult{Candidates: 4, Jobs: 2}) {
		t.Fatalf("first run created %+v, want 4 candidates and 2 jobs", got)
	}

	rows := f.rows(f.vod)
	if len(rows) != 4 {
		t.Fatalf("stored %d candidates, want 4", len(rows))
	}
	byStart := map[float64]Candidate{}
	for _, c := range cands {
		byStart[c.Start.Seconds()] = c
	}
	for _, r := range rows {
		want, ok := byStart[r.cand.StartSeconds]
		if !ok {
			t.Fatalf("stored candidate %+v was not built", r.cand)
		}
		if r.cand.EndSeconds != want.End.Seconds() || r.cand.Score != want.Score || r.cand.Signal != want.Signal {
			t.Errorf("stored %+v, built %+v", r.cand, want)
		}
		if r.cand.Platform != candPlatform || r.cand.VODID != f.vod.ID {
			t.Errorf("candidate %d has platform %q and vod %d, want %q and %d",
				r.cand.ID, r.cand.Platform, r.cand.VODID, candPlatform, f.vod.ID)
		}
		if want.Keep {
			if r.cand.Status != store.CandidateQueued {
				t.Errorf("kept candidate %d has status %q, want queued", r.cand.ID, r.cand.Status)
			}
			if r.job == nil {
				t.Errorf("kept candidate %d has no job", r.cand.ID)
				continue
			}
			if r.job.Status != store.JobQueued || r.job.Platform != candPlatform {
				t.Errorf("job of candidate %d: status %q platform %q, want queued and %q",
					r.cand.ID, r.job.Status, r.job.Platform, candPlatform)
			}
		} else {
			if r.cand.Status != store.CandidateSkipped {
				t.Errorf("candidate %d below the limit has status %q, want skipped", r.cand.ID, r.cand.Status)
			}
			if r.job != nil {
				t.Errorf("candidate %d below the limit has job %d", r.cand.ID, r.job.ID)
			}
		}
	}
	// The moment both signals confirm ranks first and is kept.
	if cands[0].Signal != store.SignalBoth || !cands[0].Keep {
		t.Errorf("top candidate %+v, want a kept one with both signals", cands[0])
	}

	// A second run, with the candidates built again, creates nothing.
	if got := f.run(f.vod, f.build(2)); got != (QueueResult{}) {
		t.Errorf("second run created %+v, want nothing", got)
	}
	if n := f.count("candidates"); n != 4 {
		t.Errorf("%d candidates after the second run, want 4", n)
	}
	if n := f.count("jobs"); n != 2 {
		t.Errorf("%d jobs after the second run, want 2", n)
	}

	// The same windows on another VOD are that VOD's own candidates.
	if got := f.run(f.other, f.build(2)); got != (QueueResult{Candidates: 4, Jobs: 2}) {
		t.Errorf("run on another VOD created %+v, want 4 candidates and 2 jobs", got)
	}
}

func TestQueueCandidatesRaisedLimit(t *testing.T) {
	f := newCandFixture(t)
	f.run(f.vod, f.build(1))

	// With a higher limit the stored, skipped candidates now kept get their
	// jobs, and nothing is stored twice.
	if got := f.run(f.vod, f.build(3)); got != (QueueResult{Jobs: 2}) {
		t.Fatalf("run with a higher limit created %+v, want 2 jobs and no candidates", got)
	}
	queued := 0
	for _, r := range f.rows(f.vod) {
		if r.job != nil {
			queued++
			if r.cand.Status != store.CandidateQueued {
				t.Errorf("candidate %d has a job but status %q", r.cand.ID, r.cand.Status)
			}
		}
	}
	if queued != 3 {
		t.Errorf("%d candidates have a job, want 3", queued)
	}
	if n := f.count("jobs"); n != 3 {
		t.Errorf("%d jobs, want 3", n)
	}
}

func TestQueueCandidatesNothing(t *testing.T) {
	f := newCandFixture(t)
	if got := f.run(f.vod, nil); got != (QueueResult{}) {
		t.Errorf("no candidates created %+v", got)
	}
	if n := f.count("candidates"); n != 0 {
		t.Errorf("%d candidates stored, want 0", n)
	}
}

func TestQueueCandidatesRejectsUnstoredVOD(t *testing.T) {
	f := newCandFixture(t)
	if _, err := QueueCandidates(context.Background(), f.store, f.queue, store.VOD{}, f.build(2)); err == nil {
		t.Error("QueueCandidates with a VOD that has no id: nil error, want one")
	}
	if n := f.count("candidates"); n != 0 {
		t.Errorf("%d candidates stored, want 0", n)
	}
}

func TestQueueCandidatesRollsBackOnError(t *testing.T) {
	f := newCandFixture(t)
	vod := f.vod
	vod.Platform = "elsewhere" // the composite foreign key refuses this
	if _, err := QueueCandidates(context.Background(), f.store, f.queue, vod, f.build(2)); err == nil {
		t.Fatal("QueueCandidates with the wrong platform: nil error, want one")
	}
	if n := f.count("candidates"); n != 0 {
		t.Errorf("%d candidates stored after a failed run, want 0", n)
	}
	if n := f.count("jobs"); n != 0 {
		t.Errorf("%d jobs stored after a failed run, want 0", n)
	}
}

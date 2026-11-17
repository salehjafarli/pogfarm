package queue

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// clippers lists the clippers and must succeed.
func (f *fixture) clippers() []Clipper {
	f.t.Helper()
	got, err := f.queue.Clippers(context.Background())
	if err != nil {
		f.t.Fatalf("Clippers: %v", err)
	}
	return got
}

func TestClippersEmpty(t *testing.T) {
	f := newFixture(t, Options{})
	if got := f.clippers(); len(got) != 0 {
		t.Errorf("Clippers on an empty queue = %+v", got)
	}
	// A queued job has no clipper yet.
	f.enqueue()
	if got := f.clippers(); len(got) != 0 {
		t.Errorf("Clippers with only a queued job = %+v", got)
	}
}

func TestClippers(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{})
	for range 4 {
		f.enqueue()
	}

	_, a1 := f.claim("node-a") // t0
	f.clock.Advance(10 * time.Second)
	_, b1 := f.claim("node-b") // t0+10s
	f.clock.Advance(10 * time.Second)
	if _, err := f.queue.Heartbeat(ctx, a1, "node-a"); err != nil { // t0+20s
		t.Fatal(err)
	}
	f.clock.Advance(10 * time.Second)
	_, c1 := f.claim("node-c") // t0+30s

	want := []Clipper{
		{Name: "node-c", LastSeen: t0ms.Add(30 * time.Second), JobID: c1},
		{Name: "node-a", LastSeen: t0ms.Add(20 * time.Second), JobID: a1},
		{Name: "node-b", LastSeen: t0ms.Add(10 * time.Second), JobID: b1},
	}
	if got := f.clippers(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Clippers =\n%+v\nwant\n%+v", got, want)
	}

	// node-b gives its job up: still seen, holding nothing. Failing is not
	// a sign of life the queue records.
	f.clock.Advance(10 * time.Second)
	if _, err := f.queue.Fail(ctx, b1, "node-b", "VOD deleted", false); err != nil {
		t.Fatal(err)
	}
	want[2].JobID = 0
	if got := f.clippers(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after node-b failed its job: Clippers =\n%+v\nwant\n%+v", got, want)
	}

	// node-a restarts and claims again while its old lease runs: it holds
	// two jobs and is shown with the newer one.
	f.clock.Advance(10 * time.Second)
	_, a2 := f.claim("node-a") // t0+50s
	want = []Clipper{
		{Name: "node-a", LastSeen: t0ms.Add(50 * time.Second), JobID: a2},
		{Name: "node-c", LastSeen: t0ms.Add(30 * time.Second), JobID: c1},
		{Name: "node-b", LastSeen: t0ms.Add(10 * time.Second), JobID: 0},
	}
	if got := f.clippers(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after node-a claimed again: Clippers =\n%+v\nwant\n%+v", got, want)
	}

	// node-c goes quiet and the reaper takes its job back: it keeps its
	// last-seen time and holds nothing.
	f.clock.Advance(DefaultLeaseDuration)
	if _, err := f.queue.Heartbeat(ctx, a1, "node-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.queue.Heartbeat(ctx, a2, "node-a"); err != nil {
		t.Fatal(err)
	}
	if res := f.reap(); !reflect.DeepEqual(res.Requeued, []int64{c1}) {
		t.Fatalf("Reap = %+v, want job %d queued again", res, c1)
	}
	seenA := t0ms.Add(50*time.Second + DefaultLeaseDuration)
	want = []Clipper{
		{Name: "node-a", LastSeen: seenA, JobID: a2},
		{Name: "node-c", LastSeen: t0ms.Add(30 * time.Second), JobID: 0},
		{Name: "node-b", LastSeen: t0ms.Add(10 * time.Second), JobID: 0},
	}
	if got := f.clippers(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after node-c's lease expired: Clippers =\n%+v\nwant\n%+v", got, want)
	}
}

package scan

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

func discoverAccount(login string) source.Account {
	return source.Account{ID: "id-" + login, Streamer: api.Streamer{Login: login, DisplayName: "Display " + login}}
}

func TestSyncStreamersStoresAllowlistBeforeAnyVOD(t *testing.T) {
	d, src, st := discoverSetup(t, "alice", "ghost")
	d.Allowlist = append(d.Allowlist, discoverEntry("carol", false))
	src.AddAccount(discoverAccount("alice"))
	src.AddAccount(discoverAccount("carol"))
	ctx := context.Background()

	// dave was stored earlier and is no longer on the allowlist.
	if _, err := store.InsertStreamer(ctx, st.Writer(), store.Streamer{
		Platform: discoverPlatform, PlatformUserID: "id-dave", Login: "dave", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := d.SyncStreamers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stored) != 1 || res.Stored[0].Login != "alice" || res.Disabled != 1 || res.Lazy != 0 {
		t.Errorf("result: %+v", res)
	}
	if len(res.Failures) != 1 || res.Failures[0].Login != "ghost" || !errors.Is(res.Failures[0].Err, source.ErrStreamerNotFound) {
		t.Errorf("failures: %+v", res.Failures)
	}
	alice := discoverStreamerRow(t, st, "alice")
	if !alice.Enabled || alice.DisplayName != "Display alice" {
		t.Errorf("alice: %+v", alice)
	}
	if discoverStreamerRow(t, st, "dave").Enabled {
		t.Error("dave should be switched off")
	}
	// carol is switched off in the config, so she is not stored.
	if n := discoverStreamerCount(t, st); n != 2 {
		t.Errorf("%d streamers stored, want alice and dave", n)
	}

	// Discovery finds the row the sync made and adds no second one.
	src.AddVOD(discoverVOD("alice", "a1", time.Hour))
	pass := discoverPass(t, d)
	if len(pass.Recorded) != 1 || pass.Recorded[0].StreamerID != alice.ID {
		t.Errorf("discovery after sync: %+v", pass)
	}
	if n := discoverStreamerCount(t, st); n != 2 {
		t.Errorf("%d streamers stored after discovery, want 2", n)
	}

	// Syncing again changes nothing.
	again, err := d.SyncStreamers(ctx)
	if err != nil || len(again.Stored) != 1 || again.Stored[0].ID != alice.ID || again.Disabled != 0 {
		t.Errorf("second sync: %+v, %v", again, err)
	}
}

func TestSyncStreamersUpdatesRenamedAccount(t *testing.T) {
	d, src, st := discoverSetup(t, "newname")
	ctx := context.Background()
	old, err := store.InsertStreamer(ctx, st.Writer(), store.Streamer{
		Platform: discoverPlatform, PlatformUserID: "id-x", Login: "oldname", DisplayName: "Old", Enabled: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	src.AddAccount(source.Account{ID: "id-x", Streamer: api.Streamer{Login: "newname", DisplayName: "New"}})

	if _, err := d.SyncStreamers(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetStreamer(ctx, st.Reader(), old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Login != "newname" || got.DisplayName != "New" || !got.Enabled {
		t.Errorf("renamed streamer: %+v", got)
	}
}

func TestSyncStreamersWithoutLookupIsLazy(t *testing.T) {
	d, src, st := discoverSetup(t, "alice")
	src.AddAccount(discoverAccount("alice"))
	reg, err := source.NewRegistry(noLookupSource{src})
	if err != nil {
		t.Fatal(err)
	}
	d.Sources = reg

	res, err := d.SyncStreamers(context.Background())
	if err != nil || res.Lazy != 1 || len(res.Stored) != 0 || len(res.Failures) != 0 {
		t.Errorf("result %+v, %v; want one lazy entry", res, err)
	}
	if n := discoverStreamerCount(t, st); n != 0 {
		t.Errorf("%d streamers stored, want none", n)
	}
}

// noLookupSource has only the methods of source.Source.
type noLookupSource struct{ source.Source }

func TestSyncStreamersUnknownPlatformAndCancel(t *testing.T) {
	d, _, _ := discoverSetup(t)
	d.Allowlist = []config.Streamer{{Platform: "elsewhere", Login: "x"}}
	res, err := d.SyncStreamers(context.Background())
	if err != nil || len(res.Failures) != 1 || !errors.Is(res.Failures[0].Err, source.ErrUnknownPlatform) {
		t.Errorf("unknown platform: %+v, %v", res, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.SyncStreamers(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: err = %v", err)
	}
	if _, err := (&Discoverer{}).SyncStreamers(context.Background()); err == nil {
		t.Error("no store: no error")
	}
}

// Run syncs the allowlist, runs a pass at once, and stops on cancel while it
// waits for the next one.
func TestDiscovererRunSyncsDiscoversAndStops(t *testing.T) {
	d, src, st := discoverSetup(t, "alice", "bob")
	src.AddAccount(discoverAccount("bob")) // bob has no VODs yet
	src.AddVOD(discoverVOD("alice", "a1", time.Hour))
	sink := newLogSink()
	d.Logger = slog.New(sink)
	d.Interval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	sink.await(t, "discovery pass done", 1)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := discoverVODIDs(t, st); !slices.Equal(got, []string{"a1"}) {
		t.Errorf("recorded %v", got)
	}
	if !discoverStreamerRow(t, st, "bob").Enabled || !discoverStreamerRow(t, st, "alice").Enabled {
		t.Error("both streamers should be stored")
	}

	if err := (&Discoverer{}).Run(context.Background()); err == nil {
		t.Error("Run without a store: no error")
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if err := d.Run(ctx); err != nil {
		t.Errorf("Run with an ended context: %v", err)
	}
}

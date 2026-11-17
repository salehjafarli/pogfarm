package sourcetest

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
)

const platform api.Platform = "fakeplatform"

var day = time.Date(2026, 11, 17, 18, 0, 0, 0, time.UTC)

func vod(id, login string, startedAt time.Time, ended bool) source.VOD {
	return source.VOD{
		ID:        id,
		Streamer:  api.Streamer{Login: login, DisplayName: "Display " + login},
		Title:     "stream " + id,
		StartedAt: startedAt,
		Duration:  2 * time.Hour,
		Ended:     ended,
	}
}

func TestPlatform(t *testing.T) {
	if got := New(platform).Platform(); got != platform {
		t.Errorf("Platform() = %q, want %q", got, platform)
	}
	if got := NewWithClips(platform).Platform(); got != platform {
		t.Errorf("FakeWithClips Platform() = %q, want %q", got, platform)
	}
}

func TestRecentVODs(t *testing.T) {
	ctx := context.Background()
	f := New(platform)
	f.AddVOD(vod("old", "one", day, true))
	f.AddVOD(vod("live", "one", day.Add(48*time.Hour), false))
	f.AddVOD(vod("mid", "one", day.Add(24*time.Hour), true))
	f.AddVOD(vod("other", "two", day, true))

	got, err := f.RecentVODs(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, v := range got {
		ids = append(ids, v.ID)
		if v.Platform != platform {
			t.Errorf("VOD %s has platform %q, want %q", v.ID, v.Platform, platform)
		}
	}
	if want := []string{"live", "mid", "old"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("VODs = %v, want %v (newest first, one streamer only)", ids, want)
	}
	if got[0].Ended || !got[1].Ended {
		t.Errorf("Ended not preserved: live=%v mid=%v", got[0].Ended, got[1].Ended)
	}
	want := vod("mid", "one", day.Add(24*time.Hour), true)
	want.Platform = platform
	if !reflect.DeepEqual(got[1], want) {
		t.Errorf("VOD = %+v, want %+v", got[1], want)
	}

	none, err := f.RecentVODs(ctx, "nobody")
	if err != nil {
		t.Fatalf("unknown login: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("unknown login returned %d VODs", len(none))
	}
}

func TestAddVODReplaces(t *testing.T) {
	ctx := context.Background()
	f := New(platform)
	f.AddVOD(vod("v1", "one", day, false))
	f.AddVOD(vod("v1", "one", day, true)) // the stream ended

	got, err := f.RecentVODs(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Ended {
		t.Errorf("got %+v, want the one ended VOD", got)
	}
}

func TestChat(t *testing.T) {
	ctx := context.Background()
	f := New(platform)
	v := vod("v1", "one", day, true)
	f.AddVOD(v)

	empty, err := f.Chat(ctx, v)
	if err != nil {
		t.Fatalf("VOD without chat: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("VOD without chat returned %d messages", len(empty))
	}

	given := []source.ChatMessage{
		{Offset: 30 * time.Second, User: "c", Text: "third"},
		{Offset: 10 * time.Second, User: "a", Text: "first LUL", Emotes: []string{"LUL"}},
		{Offset: 10 * time.Second, User: "b", Text: "second"},
	}
	f.SetChat(v.ID, given)

	got, err := f.Chat(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	want := []source.ChatMessage{
		{Offset: 10 * time.Second, User: "a", Text: "first LUL", Emotes: []string{"LUL"}},
		{Offset: 10 * time.Second, User: "b", Text: "second"},
		{Offset: 30 * time.Second, User: "c", Text: "third"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chat = %+v\nwant   %+v (ordered by offset, ties in given order)", got, want)
	}
	if given[0].User != "c" {
		t.Error("SetChat reordered the caller's slice")
	}
}

func TestDataIsCopied(t *testing.T) {
	ctx := context.Background()
	f := NewWithClips(platform)
	v := vod("v1", "one", day, true)
	f.AddVOD(v)

	messages := []source.ChatMessage{{Offset: time.Second, User: "a", Text: "hi", Emotes: []string{"Kappa"}}}
	clips := []source.ViewerClip{{Offset: time.Minute, Duration: 20 * time.Second, Views: 5, Title: "clip"}}
	f.SetChat(v.ID, messages)
	f.SetViewerClips(v.ID, clips)

	// Changing what was handed in does not change the fake.
	messages[0].Text = "changed"
	messages[0].Emotes[0] = "changed"
	clips[0].Title = "changed"

	// Changing what came out does not change the fake either.
	for range 2 {
		gotChat, err := f.Chat(ctx, v)
		if err != nil {
			t.Fatal(err)
		}
		if gotChat[0].Text != "hi" || gotChat[0].Emotes[0] != "Kappa" {
			t.Fatalf("chat was changed from outside: %+v", gotChat[0])
		}
		gotChat[0].Text = "changed"
		gotChat[0].Emotes[0] = "changed"

		gotClips, err := f.ViewerClips(ctx, v)
		if err != nil {
			t.Fatal(err)
		}
		if gotClips[0].Title != "clip" {
			t.Fatalf("clips were changed from outside: %+v", gotClips[0])
		}
		gotClips[0].Title = "changed"

		gotVODs, err := f.RecentVODs(ctx, "one")
		if err != nil {
			t.Fatal(err)
		}
		if gotVODs[0].Title != "stream v1" {
			t.Fatalf("VODs were changed from outside: %+v", gotVODs[0])
		}
		gotVODs[0].Title = "changed"
	}
}

func TestViewerClips(t *testing.T) {
	ctx := context.Background()
	f := NewWithClips(platform)
	v := vod("v1", "one", day, true)
	f.AddVOD(v)

	empty, err := f.ViewerClips(ctx, v)
	if err != nil {
		t.Fatalf("VOD without clips: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("VOD without clips returned %d", len(empty))
	}

	f.SetViewerClips(v.ID, []source.ViewerClip{
		{Offset: 50 * time.Minute, Duration: 30 * time.Second, Views: 3, Title: "late"},
		{Offset: 5 * time.Minute, Duration: 45 * time.Second, Views: 900, Title: "early"},
	})
	got, err := f.ViewerClips(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	want := []source.ViewerClip{
		{Offset: 5 * time.Minute, Duration: 45 * time.Second, Views: 900, Title: "early"},
		{Offset: 50 * time.Minute, Duration: 30 * time.Second, Views: 3, Title: "late"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("clips = %+v, want %+v", got, want)
	}
}

func TestUnknownAndRemovedVOD(t *testing.T) {
	ctx := context.Background()
	f := NewWithClips(platform)
	v := vod("v1", "one", day, true)

	if _, err := f.Chat(ctx, v); !errors.Is(err, source.ErrVODNotFound) {
		t.Errorf("Chat of unknown VOD: err = %v, want ErrVODNotFound", err)
	}
	if _, err := f.ViewerClips(ctx, v); !errors.Is(err, source.ErrVODNotFound) {
		t.Errorf("ViewerClips of unknown VOD: err = %v, want ErrVODNotFound", err)
	}

	f.AddVOD(v)
	f.SetChat(v.ID, []source.ChatMessage{{User: "a", Text: "hi"}})
	if _, err := f.Chat(ctx, v); err != nil {
		t.Fatalf("Chat after AddVOD: %v", err)
	}

	f.RemoveVOD(v.ID)
	if _, err := f.Chat(ctx, v); !errors.Is(err, source.ErrVODNotFound) {
		t.Errorf("Chat of removed VOD: err = %v, want ErrVODNotFound", err)
	}
	if _, err := f.ViewerClips(ctx, v); !errors.Is(err, source.ErrVODNotFound) {
		t.Errorf("ViewerClips of removed VOD: err = %v, want ErrVODNotFound", err)
	}
	vods, err := f.RecentVODs(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	if len(vods) != 0 {
		t.Errorf("removed VOD is still listed: %+v", vods)
	}

	// A VOD added again starts without the old chat.
	f.AddVOD(v)
	chat, err := f.Chat(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat) != 0 {
		t.Errorf("re-added VOD kept %d old messages", len(chat))
	}
}

func TestSetError(t *testing.T) {
	ctx := context.Background()
	f := NewWithClips(platform)
	v := vod("v1", "one", day, true)
	f.AddVOD(v)

	boom := errors.New("boom")
	f.SetError(boom)
	if _, err := f.RecentVODs(ctx, "one"); !errors.Is(err, boom) {
		t.Errorf("RecentVODs: err = %v, want boom", err)
	}
	if _, err := f.Chat(ctx, v); !errors.Is(err, boom) {
		t.Errorf("Chat: err = %v, want boom", err)
	}
	if _, err := f.ViewerClips(ctx, v); !errors.Is(err, boom) {
		t.Errorf("ViewerClips: err = %v, want boom", err)
	}

	f.SetError(nil)
	if _, err := f.RecentVODs(ctx, "one"); err != nil {
		t.Errorf("RecentVODs after clearing the error: %v", err)
	}
}

func TestCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f := NewWithClips(platform)
	v := vod("v1", "one", day, true)
	f.AddVOD(v)

	if _, err := f.RecentVODs(ctx, "one"); !errors.Is(err, context.Canceled) {
		t.Errorf("RecentVODs: err = %v, want context.Canceled", err)
	}
	if _, err := f.Chat(ctx, v); !errors.Is(err, context.Canceled) {
		t.Errorf("Chat: err = %v, want context.Canceled", err)
	}
	if _, err := f.ViewerClips(ctx, v); !errors.Is(err, context.Canceled) {
		t.Errorf("ViewerClips: err = %v, want context.Canceled", err)
	}
}

func TestCalls(t *testing.T) {
	ctx := context.Background()
	f := NewWithClips(platform)
	v := vod("v1", "one", day, true)
	f.AddVOD(v)

	if got := f.Calls(); len(got) != 0 {
		t.Fatalf("new fake has calls: %v", got)
	}

	f.RecentVODs(ctx, "one")
	f.Chat(ctx, v)
	f.ViewerClips(ctx, v)
	f.Chat(ctx, source.VOD{ID: "missing"}) // failed calls count too

	want := []Call{
		{Method: "RecentVODs", Arg: "one"},
		{Method: "Chat", Arg: "v1"},
		{Method: "ViewerClips", Arg: "v1"},
		{Method: "Chat", Arg: "missing"},
	}
	if got := f.Calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestConcurrentUse(t *testing.T) {
	ctx := context.Background()
	f := NewWithClips(platform)
	v := vod("v1", "one", day, true)
	f.AddVOD(v)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			f.SetChat(v.ID, []source.ChatMessage{{User: "a", Text: "hi"}})
			f.SetViewerClips(v.ID, []source.ViewerClip{{Title: "clip"}})
			f.AddVOD(v)
		})
		wg.Go(func() {
			f.RecentVODs(ctx, "one")
			f.Chat(ctx, v)
			f.ViewerClips(ctx, v)
			f.Calls()
		})
	}
	wg.Wait()

	if got := len(f.Calls()); got != 24 {
		t.Errorf("recorded %d calls, want 24", got)
	}
}

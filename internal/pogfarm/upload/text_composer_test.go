package upload

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// posttextStore opens a temporary store holding one clip of SomeStreamer and
// returns it with the clip.
func posttextStore(t *testing.T, displayName string, words ...string) (*store.Store, store.Clip) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Path(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	w := st.Writer()
	streamer, err := store.InsertStreamer(ctx, w, store.Streamer{
		Platform: api.PlatformTwitch, PlatformUserID: "1001", Login: "somestreamer",
		DisplayName: displayName, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	vod, err := store.InsertVOD(ctx, w, store.VOD{
		Platform: api.PlatformTwitch, PlatformVODID: "v1", StreamerID: streamer.ID,
		Title: "Ranked grind day 3", DurationSeconds: 3600,
		StartedAt: posttextDate, EndedAt: posttextDate.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	cand, err := store.InsertCandidate(ctx, w, store.Candidate{
		Platform: api.PlatformTwitch, VODID: vod.ID, StartSeconds: 10, EndSeconds: 40,
		Score: 1, Signal: store.SignalChatSpike, Status: store.CandidateQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.InsertJob(ctx, w, store.Job{
		Platform: api.PlatformTwitch, CandidateID: cand.ID, Status: store.JobDone, Clipper: "node-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	transcript, _ := json.Marshal(posttextTranscript(words...))
	clip, err := store.InsertClip(ctx, w, store.Clip{
		Platform: api.PlatformTwitch, JobID: job.ID, FilePath: "clips/1.mp4", SHA256: strings.Repeat("ab", 32),
		SizeBytes: 1, DurationSeconds: 30, Width: 1080, Height: 1920, TranscriptJSON: string(transcript),
	})
	if err != nil {
		t.Fatal(err)
	}
	return st, clip
}

func posttextConfig() config.Post {
	return config.Post{
		Default: posttextDefault(),
		Streamers: []config.PostOverride{{
			Platform: api.PlatformTwitch, Login: "other", Title: "not this one",
		}},
	}
}

func TestComposer(t *testing.T) {
	ctx := context.Background()
	st, clip := posttextStore(t, "SomeStreamer", "No", "way", "he", "hit", "that.")
	compose := NewComposer(st, posttextConfig())

	title, desc, tags, err := compose(ctx, clip, "youtube")
	if err != nil {
		t.Fatal(err)
	}
	if title != "No way he hit that | SomeStreamer" {
		t.Errorf("title = %q", title)
	}
	if want := "No way he hit that\n\n#shorts #somestreamer\n\n" + posttextCredit; desc != want {
		t.Errorf("description = %q, want %q", desc, want)
	}
	if !reflect.DeepEqual(tags, []string{"shorts", "somestreamer"}) {
		t.Errorf("tags = %q", tags)
	}

	// The review message shows the same title.
	got, err := ReviewTitle(st, compose, "youtube")(ctx, clip.ID)
	if err != nil || got != title {
		t.Errorf("ReviewTitle = %q, %v; want %q", got, err, title)
	}

	// With an override the title is the owner's; the rest is unchanged.
	if err := store.SetTitleOverride(ctx, st.Writer(), clip.ID, "My own title"); err != nil {
		t.Fatal(err)
	}
	title2, desc2, tags2, err := compose(ctx, clip, "youtube")
	if err != nil {
		t.Fatal(err)
	}
	if title2 != "My own title" || desc2 != desc || !reflect.DeepEqual(tags2, tags) {
		t.Errorf("with override: %q, %q, %q", title2, desc2, tags2)
	}
	if got, _ := ReviewTitle(st, compose, "youtube")(ctx, clip.ID); got != "My own title" {
		t.Errorf("ReviewTitle with override = %q", got)
	}

	// The override still obeys the target's limit.
	long := strings.Repeat("override ", 30)
	if err := store.SetTitleOverride(ctx, st.Writer(), clip.ID, long); err != nil {
		t.Fatal(err)
	}
	title3, _, _, err := compose(ctx, clip, "youtube")
	if err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCountInString(title3); n > LimitsFor("youtube").Title || !strings.HasSuffix(title3, "override…") {
		t.Errorf("long override = %q (%d runes)", title3, n)
	}
}

func TestComposerPerStreamerOverride(t *testing.T) {
	st, clip := posttextStore(t, "SomeStreamer")
	cfg := posttextConfig()
	cfg.Streamers = append(cfg.Streamers, config.PostOverride{
		Platform: api.PlatformTwitch, Login: "somestreamer", Title: "{streamer} on {date}", Credit: "Watch {channel_url}",
	})
	title, desc, _, err := NewComposer(st, cfg)(context.Background(), clip, "fake")
	if err != nil {
		t.Fatal(err)
	}
	if title != "SomeStreamer on 2026-11-16" {
		t.Errorf("title = %q", title)
	}
	// Empty transcript: the streamer's name is the hook.
	if want := "SomeStreamer\n\n#shorts #somestreamer\n\nWatch https://www.twitch.tv/somestreamer"; desc != want {
		t.Errorf("description = %q, want %q", desc, want)
	}
}

func TestComposerWithoutDisplayName(t *testing.T) {
	st, clip := posttextStore(t, "", "hello", "chat")
	title, _, _, err := NewComposer(st, posttextConfig())(context.Background(), clip, "youtube")
	if err != nil {
		t.Fatal(err)
	}
	if title != "hello chat | somestreamer" {
		t.Errorf("title = %q", title)
	}
}

func TestComposerErrors(t *testing.T) {
	ctx := context.Background()
	st, clip := posttextStore(t, "SomeStreamer")
	compose := NewComposer(st, posttextConfig())

	missing := clip
	missing.ID += 100
	if _, _, _, err := compose(ctx, missing, "youtube"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing clip: error = %v, want ErrNotFound", err)
	}
	if _, err := ReviewTitle(st, compose, "youtube")(ctx, missing.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ReviewTitle of missing clip: error = %v, want ErrNotFound", err)
	}

	cfg := posttextConfig()
	cfg.Default.Credit = strings.Repeat("credit ", 1000)
	if _, _, _, err := NewComposer(st, cfg)(ctx, clip, "youtube"); !errors.Is(err, ErrCreditTooLong) {
		t.Errorf("long credit: error = %v, want ErrCreditTooLong", err)
	}
}

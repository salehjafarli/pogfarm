package twitch

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
)

// sourceServe returns a Source over a fake Twitch that answers every
// endpoint the parts use, and a chat tool that copies the chat fixture.
func sourceServe(t *testing.T) *Source {
	t.Helper()
	users := vodsFixture(t, "vods_users.json")
	videos := vodsFixture(t, "vods_videos.json")
	video := clipsFixture(t, "clips_video.json")
	clips := `{"data":[` +
		`{"id":"c1","video_id":"2001","title":"Clutch","view_count":90,"duration":30,"vod_offset":5400},` +
		`{"id":"c2","video_id":"9999","title":"other vod","view_count":5,"duration":20,"vod_offset":60}` +
		`],"pagination":{}}`

	f := newFakeTwitch(t)
	f.helix = func(w http.ResponseWriter, r *http.Request, _ int) {
		switch r.URL.Path {
		case "/helix/users":
			if r.URL.Query().Get("login") != "some_streamer" {
				writeJSON(w, http.StatusOK, `{"data":[]}`)
				return
			}
			writeJSON(w, http.StatusOK, users)
		case "/helix/streams":
			writeJSON(w, http.StatusOK, vodsOffline)
		case "/helix/videos":
			// A lookup by id is the clip lister's; a listing by user is
			// the VOD lister's.
			if r.URL.Query().Get("id") != "" {
				writeJSON(w, http.StatusOK, video)
				return
			}
			writeJSON(w, http.StatusOK, videos)
		case "/helix/clips":
			writeJSON(w, http.StatusOK, clips)
		default:
			writeJSON(w, http.StatusNotFound, `{"message":"unexpected path"}`)
		}
	}
	c, _ := newTestClient(t, f)

	fixture, err := filepath.Abs(filepath.Join("testdata", "chat.json"))
	if err != nil {
		t.Fatal(err)
	}
	bin, _ := chatFakeTool(t, `cp '`+fixture+`' "$out"`)
	s, err := NewSource(c, chatFetcher(t, bin))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSourceComposesTheParts(t *testing.T) {
	ctx := context.Background()
	s := sourceServe(t)

	if s.Platform() != api.PlatformTwitch {
		t.Errorf("Platform() = %q", s.Platform())
	}

	vods, err := s.RecentVODs(ctx, "some_streamer")
	if err != nil {
		t.Fatal(err)
	}
	if len(vods) != 3 || vods[0].ID != "2300000003" || vods[0].StreamerID != vodsUserID || vods[0].Platform != s.Platform() {
		t.Errorf("RecentVODs: %+v", vods)
	}

	acct, err := s.LookupStreamer(ctx, "some_streamer")
	want := source.Account{ID: vodsUserID, Streamer: api.Streamer{Login: "some_streamer", DisplayName: "Some_Streamer"}}
	if err != nil || acct != want {
		t.Errorf("LookupStreamer: got %+v, %v; want %+v", acct, err, want)
	}
	if _, err := s.LookupStreamer(ctx, "nobody_here"); !errors.Is(err, source.ErrStreamerNotFound) {
		t.Errorf("LookupStreamer of an unknown login: err = %v, want ErrStreamerNotFound", err)
	}
	if _, err := s.LookupStreamer(ctx, ""); err == nil {
		t.Error("LookupStreamer of an empty login: no error")
	}

	// Through the helpers of package source, as the scanner calls it.
	gotClips, err := source.ViewerClips(ctx, s, clipsVOD())
	if err != nil {
		t.Fatal(err)
	}
	wantClips := []source.ViewerClip{{Offset: 5400 * time.Second, Duration: 30 * time.Second, Views: 90, Title: "Clutch"}}
	if !reflect.DeepEqual(gotClips, wantClips) {
		t.Errorf("ViewerClips: got %+v, want %+v", gotClips, wantClips)
	}
	if acct, err := source.LookupStreamer(ctx, s, "some_streamer"); err != nil || acct != want {
		t.Errorf("source.LookupStreamer: got %+v, %v", acct, err)
	}

	chat, err := s.Chat(ctx, chatTestVOD)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(chat, chatFixtureMessages) {
		t.Errorf("Chat:\n got %+v\nwant %+v", chat, chatFixtureMessages)
	}
}

func TestSourceRegisters(t *testing.T) {
	r, err := source.NewRegistry(sourceServe(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(api.PlatformTwitch); err != nil {
		t.Error(err)
	}
}

func TestNewSourceNeedsBothParts(t *testing.T) {
	c, err := NewClient(Options{ClientID: testClientID, ClientSecret: config.NewSecret(testSecret)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSource(nil, &ChatFetcher{}); err == nil {
		t.Error("no client: no error")
	}
	if _, err := NewSource(c, nil); err == nil {
		t.Error("no chat fetcher: no error")
	}
}

func TestNewSourceFromConfig(t *testing.T) {
	if _, err := NewSourceFromConfig(nil); err == nil {
		t.Error("nil config: no error")
	}
	_, err := NewSourceFromConfig(&config.Pogfarm{DataDir: t.TempDir(), TwitchDownloaderPath: "TwitchDownloaderCLI"})
	if err == nil || !strings.Contains(err.Error(), "client id") {
		t.Errorf("no credentials: err = %v, want one about the client id", err)
	}

	dir := t.TempDir()
	s, err := NewSourceFromConfig(&config.Pogfarm{
		DataDir:              dir,
		TwitchDownloaderPath: "/opt/TwitchDownloaderCLI",
		Twitch:               config.Twitch{ClientID: testClientID, ClientSecret: config.NewSecret(testSecret)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.chat.Binary != "/opt/TwitchDownloaderCLI" || s.chat.Dir != dir {
		t.Errorf("chat fetcher: %+v", *s.chat)
	}
	if s.vods.client == nil || s.vods.client != s.clips.client {
		t.Error("the listers do not share one client")
	}
}

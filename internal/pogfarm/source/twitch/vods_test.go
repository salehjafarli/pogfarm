package twitch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
)

// The fixtures in testdata/vods_*.json are hand-written in the layout the
// Helix reference documents for Get Users, Get Videos and Get Streams. They
// are not captures of real responses.

const (
	vodsUserID  = "141981764"
	vodsOffline = `{"data":[],"pagination":{}}`
)

// vodsReply is one canned Helix answer.
type vodsReply struct {
	status int
	body   string
}

func vodsOK(body string) vodsReply { return vodsReply{http.StatusOK, body} }

// vodsFixture reads a file of testdata.
func vodsFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// vodsHelix is the Helix side of a fakeTwitch for RecentVODs. Each endpoint
// answers with its replies in order and repeats the last one. Every request
// is recorded as "endpoint?query".
type vodsHelix struct {
	mu       sync.Mutex
	replies  map[string][]vodsReply
	requests []string
	// onRequest, when set, runs before a request to the endpoint is
	// answered.
	onRequest func(endpoint string)
}

// vodsServe returns a lister that talks to a fake Helix with the given
// replies per endpoint ("users", "videos", "streams").
func vodsServe(t *testing.T, replies map[string][]vodsReply) (*VODLister, *vodsHelix) {
	t.Helper()
	h := &vodsHelix{replies: replies}
	f := newFakeTwitch(t)
	f.helix = func(w http.ResponseWriter, r *http.Request, _ int) {
		endpoint := strings.TrimPrefix(r.URL.Path, "/helix/")
		h.mu.Lock()
		h.requests = append(h.requests, endpoint+"?"+r.URL.Query().Encode())
		hook := h.onRequest
		queue := h.replies[endpoint]
		var reply vodsReply
		switch len(queue) {
		case 0:
			reply = vodsReply{http.StatusNotFound, `{"message":"no reply set for ` + endpoint + `"}`}
		case 1:
			reply = queue[0]
		default:
			reply = queue[0]
			h.replies[endpoint] = queue[1:]
		}
		h.mu.Unlock()
		if hook != nil {
			hook(endpoint)
		}
		writeJSON(w, reply.status, reply.body)
	}
	c, _ := newTestClient(t, f)
	return NewVODLister(c), h
}

func (h *vodsHelix) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.requests)
}

// vodsFixtureReplies is a streamer with the fixture's videos who is offline.
func vodsFixtureReplies(t *testing.T) map[string][]vodsReply {
	t.Helper()
	return map[string][]vodsReply{
		"users":   {vodsOK(vodsFixture(t, "vods_users.json"))},
		"videos":  {vodsOK(vodsFixture(t, "vods_videos.json"))},
		"streams": {vodsOK(vodsOffline)},
	}
}

// vodsVideoJSON is one archive video with the given fields. An empty
// streamID is sent as null.
func vodsVideoJSON(id, streamID, createdAt, duration string) string {
	stream := "null"
	if streamID != "" {
		stream = `"` + streamID + `"`
	}
	return fmt.Sprintf(`{"id":%q,"stream_id":%s,"user_id":%q,"title":"t","created_at":%q,"type":"archive","duration":%q}`,
		id, stream, vodsUserID, createdAt, duration)
}

func vodsList(items ...string) string {
	return `{"data":[` + strings.Join(items, ",") + `],"pagination":{}}`
}

func vodsStreamJSON(id, startedAt string) string {
	return vodsList(fmt.Sprintf(`{"id":%q,"user_id":%q,"type":"live","started_at":%q}`, id, vodsUserID, startedAt))
}

// vodsEnded maps the id of every VOD to its Ended flag.
func vodsEnded(vods []source.VOD) map[string]bool {
	out := make(map[string]bool)
	for _, v := range vods {
		out[v.ID] = v.Ended
	}
	return out
}

func vodsTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestRecentVODsListsArchives(t *testing.T) {
	l, h := vodsServe(t, vodsFixtureReplies(t))

	got, err := l.RecentVODs(context.Background(), "some_streamer")
	if err != nil {
		t.Fatal(err)
	}

	streamer := api.Streamer{Login: "some_streamer", DisplayName: "Some_Streamer"}
	want := []source.VOD{
		{
			Platform: api.PlatformTwitch, ID: "2300000003", Streamer: streamer, StreamerID: vodsUserID,
			Title: "Ranked until I drop", StartedAt: vodsTime(t, "2026-11-16T18:00:05Z"),
			Duration: 3*time.Hour + 2*time.Minute + time.Second, Ended: true,
		},
		{
			Platform: api.PlatformTwitch, ID: "2300000002", Streamer: streamer, StreamerID: vodsUserID,
			Title: "Short test stream", StartedAt: vodsTime(t, "2026-11-15T20:30:00Z"),
			Duration: 45 * time.Second, Ended: true,
		},
		// The highlight between these two is not a VOD.
		{
			Platform: api.PlatformTwitch, ID: "2300000001", Streamer: streamer, StreamerID: vodsUserID,
			Title: "Sunday chill", StartedAt: vodsTime(t, "2026-11-14T16:00:00Z"),
			Duration: 5*time.Hour + 30*time.Minute, Ended: true,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%+v\nwant\n%+v", got, want)
	}

	wantRequests := []string{
		"users?" + url.Values{"login": {"some_streamer"}}.Encode(),
		"streams?" + url.Values{"user_id": {vodsUserID}, "type": {"live"}}.Encode(),
		"videos?" + url.Values{"user_id": {vodsUserID}, "type": {"archive"}, "first": {"100"}}.Encode(),
		"streams?" + url.Values{"user_id": {vodsUserID}, "type": {"live"}}.Encode(),
	}
	if seen := h.seen(); !slices.Equal(seen, wantRequests) {
		t.Errorf("requests\n%q\nwant\n%q", seen, wantRequests)
	}
}

func TestRecentVODsUsesTheAccountsOwnLogin(t *testing.T) {
	// The login is looked up as given; the VODs carry the spelling Twitch
	// has for it.
	l, h := vodsServe(t, vodsFixtureReplies(t))

	got, err := l.RecentVODs(context.Background(), "Some_Streamer")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Streamer.Login != "some_streamer" {
		t.Errorf("unexpected VODs: %+v", got)
	}
	if seen := h.seen(); len(seen) == 0 || seen[0] != "users?login=Some_Streamer" {
		t.Errorf("unexpected requests: %q", seen)
	}
}

func TestRecentVODsLiveStreamIsNotEnded(t *testing.T) {
	// The fixture's live stream is the origin of the newest video.
	replies := vodsFixtureReplies(t)
	replies["streams"] = []vodsReply{vodsOK(vodsFixture(t, "vods_streams_live.json"))}
	l, _ := vodsServe(t, replies)

	got, err := l.RecentVODs(context.Background(), "some_streamer")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"2300000003": false, "2300000002": true, "2300000001": true}
	if ended := vodsEnded(got); !reflect.DeepEqual(ended, want) {
		t.Errorf("ended = %v, want %v", ended, want)
	}
}

func TestRecentVODsLiveDetection(t *testing.T) {
	const (
		liveStart = "2026-11-16T18:00:00Z"
		older     = "2026-11-15T10:00:00Z"
	)
	live := vodsOK(vodsStreamJSON("s-live", liveStart))
	offline := vodsOK(vodsOffline)

	tests := []struct {
		name    string
		videos  []string
		streams []vodsReply // answers to the check before and after the listing
		want    map[string]bool
	}{
		{
			name:    "offline: everything has ended",
			videos:  []string{vodsVideoJSON("new", "s-live", liveStart, "1h"), vodsVideoJSON("old", "s-old", older, "2h")},
			streams: []vodsReply{offline},
			want:    map[string]bool{"new": true, "old": true},
		},
		{
			name:    "live, matched by stream id",
			videos:  []string{vodsVideoJSON("new", "s-live", liveStart, "1h"), vodsVideoJSON("old", "s-old", older, "2h")},
			streams: []vodsReply{live},
			want:    map[string]bool{"new": false, "old": true},
		},
		{
			name: "live, matched by stream id although the video was created before the stream's start time",
			videos: []string{
				vodsVideoJSON("new", "s-live", "2026-11-16T17:59:50Z", "0s"),
				vodsVideoJSON("old", "s-old", older, "2h"),
			},
			streams: []vodsReply{live},
			want:    map[string]bool{"new": false, "old": true},
		},
		{
			name:    "live, video without a stream id is matched by time",
			videos:  []string{vodsVideoJSON("new", "", "2026-11-16T18:00:07Z", "20m"), vodsVideoJSON("old", "", older, "2h")},
			streams: []vodsReply{live},
			want:    map[string]bool{"new": false, "old": true},
		},
		{
			name:    "live, video with another stream id that reaches into the live stream",
			videos:  []string{vodsVideoJSON("new", "s-other", "2026-11-16T18:00:07Z", "20m"), vodsVideoJSON("old", "s-old", older, "2h")},
			streams: []vodsReply{live},
			want:    map[string]bool{"new": false, "old": true},
		},
		{
			name:    "live, but its video is not listed yet",
			videos:  []string{vodsVideoJSON("old", "s-old", older, "2h")},
			streams: []vodsReply{live},
			want:    map[string]bool{"old": true},
		},
		{
			name:    "stream started while the videos were listed",
			videos:  []string{vodsVideoJSON("new", "s-live", liveStart, "3s"), vodsVideoJSON("old", "s-old", older, "2h")},
			streams: []vodsReply{offline, live},
			want:    map[string]bool{"new": false, "old": true},
		},
		{
			name:    "stream ended while the videos were listed",
			videos:  []string{vodsVideoJSON("new", "s-live", liveStart, "1h"), vodsVideoJSON("old", "s-old", older, "2h")},
			streams: []vodsReply{live, offline},
			want:    map[string]bool{"new": false, "old": true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, _ := vodsServe(t, map[string][]vodsReply{
				"users":   {vodsOK(vodsFixture(t, "vods_users.json"))},
				"videos":  {vodsOK(vodsList(tt.videos...))},
				"streams": tt.streams,
			})
			got, err := l.RecentVODs(context.Background(), "some_streamer")
			if err != nil {
				t.Fatal(err)
			}
			if ended := vodsEnded(got); !reflect.DeepEqual(ended, tt.want) {
				t.Errorf("ended = %v, want %v", ended, tt.want)
			}
		})
	}
}

func TestRecentVODsEndedAfterTheStreamEnds(t *testing.T) {
	// One lister, two listings: live at the first, over at the second.
	const start = "2026-11-16T18:00:00Z"
	l, _ := vodsServe(t, map[string][]vodsReply{
		"users": {vodsOK(vodsFixture(t, "vods_users.json"))},
		"videos": {
			vodsOK(vodsList(vodsVideoJSON("v1", "s1", start, "1h"))),
			vodsOK(vodsList(vodsVideoJSON("v1", "s1", start, "2h30m"))),
		},
		"streams": {
			vodsOK(vodsStreamJSON("s1", start)), vodsOK(vodsStreamJSON("s1", start)),
			vodsOK(vodsOffline),
		},
	})

	first, err := l.RecentVODs(context.Background(), "some_streamer")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Ended || first[0].Duration != time.Hour {
		t.Errorf("while live: %+v", first)
	}
	second, err := l.RecentVODs(context.Background(), "some_streamer")
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || !second[0].Ended || second[0].Duration != 2*time.Hour+30*time.Minute {
		t.Errorf("after the stream: %+v", second)
	}
}

func TestRecentVODsNewestFirst(t *testing.T) {
	l, _ := vodsServe(t, map[string][]vodsReply{
		"users": {vodsOK(vodsFixture(t, "vods_users.json"))},
		"videos": {vodsOK(vodsList(
			vodsVideoJSON("b", "", "2026-11-12T10:00:00Z", "1h"),
			vodsVideoJSON("c", "", "2026-11-13T10:00:00+02:00", "1h"),
			vodsVideoJSON("a", "", "2026-11-11T10:00:00Z", "1h"),
		))},
		"streams": {vodsOK(vodsOffline)},
	})
	got, err := l.RecentVODs(context.Background(), "some_streamer")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, v := range got {
		ids = append(ids, v.ID)
		if v.StartedAt.Location() != time.UTC {
			t.Errorf("vod %s: start time is not in UTC: %v", v.ID, v.StartedAt)
		}
	}
	if !slices.Equal(ids, []string{"c", "b", "a"}) {
		t.Errorf("order = %v, want [c b a]", ids)
	}
}

func TestRecentVODsNoVideos(t *testing.T) {
	l, _ := vodsServe(t, map[string][]vodsReply{
		"users":   {vodsOK(vodsFixture(t, "vods_users.json"))},
		"videos":  {vodsOK(vodsOffline)},
		"streams": {vodsOK(vodsOffline)},
	})
	got, err := l.RecentVODs(context.Background(), "some_streamer")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("want no VODs, got %+v", got)
	}
}

func TestRecentVODsUnknownLogin(t *testing.T) {
	l, h := vodsServe(t, map[string][]vodsReply{"users": {vodsOK(`{"data":[]}`)}})

	_, err := l.RecentVODs(context.Background(), "nobody_here")
	if !errors.Is(err, source.ErrStreamerNotFound) {
		t.Fatalf("want ErrStreamerNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), `"nobody_here"`) {
		t.Errorf("error should name the login: %v", err)
	}
	if seen := h.seen(); len(seen) != 1 {
		t.Errorf("only the user lookup should be made, got %q", seen)
	}
}

func TestRecentVODsEmptyLogin(t *testing.T) {
	l, h := vodsServe(t, nil)
	if _, err := l.RecentVODs(context.Background(), ""); err == nil {
		t.Fatal("want an error for an empty login")
	}
	if seen := h.seen(); len(seen) != 0 {
		t.Errorf("no request should be made, got %q", seen)
	}
}

func TestRecentVODsAPIErrors(t *testing.T) {
	failure := vodsReply{http.StatusInternalServerError, `{"status":500,"message":"helix is down"}`}
	tests := []struct {
		name     string
		endpoint string
		replies  []vodsReply
		requests int
	}{
		{"user lookup", "users", []vodsReply{failure}, 1},
		{"first live check", "streams", []vodsReply{failure}, 2},
		{"video listing", "videos", []vodsReply{failure}, 3},
		{"second live check", "streams", []vodsReply{vodsOK(vodsOffline), failure}, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replies := vodsFixtureReplies(t)
			replies[tt.endpoint] = tt.replies
			l, h := vodsServe(t, replies)

			vods, err := l.RecentVODs(context.Background(), "some_streamer")
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
				t.Fatalf("want a 500 *APIError, got %v", err)
			}
			if errors.Is(err, source.ErrStreamerNotFound) {
				t.Errorf("an API failure must not look like an unknown login: %v", err)
			}
			if vods != nil {
				t.Errorf("no VODs should be returned with an error, got %+v", vods)
			}
			if seen := h.seen(); len(seen) != tt.requests {
				t.Errorf("want %d requests, got %q", tt.requests, seen)
			}
			noSecrets(t, err)
		})
	}
}

func TestRecentVODsMalformedResponses(t *testing.T) {
	const start = "2026-11-16T18:00:00Z"
	tests := []struct {
		name    string
		users   string
		videos  string
		streams string
		want    string
	}{
		{"user without id", `{"data":[{"login":"some_streamer"}]}`, "", "", "no id"},
		{"video without id", "", vodsList(vodsVideoJSON("", "s", start, "1h")), "", "no id"},
		{"video with a bad time", "", vodsList(vodsVideoJSON("v", "s", "yesterday", "1h")), "", "creation time"},
		{"video with a bad duration", "", vodsList(vodsVideoJSON("v", "s", start, "1:02:03")), "", "duration"},
		{"video without a duration", "", vodsList(vodsVideoJSON("v", "s", start, "")), "", "duration"},
		{"stream with a bad time", "", "", vodsStreamJSON("s", "now"), "start time"},
		{"videos not json", "", `<html>`, "", "decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replies := vodsFixtureReplies(t)
			for endpoint, body := range map[string]string{"users": tt.users, "videos": tt.videos, "streams": tt.streams} {
				if body != "" {
					replies[endpoint] = []vodsReply{vodsOK(body)}
				}
			}
			l, _ := vodsServe(t, replies)
			_, err := l.RecentVODs(context.Background(), "some_streamer")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("want an error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestRecentVODsCancelledBeforeStart(t *testing.T) {
	l, h := vodsServe(t, vodsFixtureReplies(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := l.RecentVODs(ctx, "some_streamer")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if seen := h.seen(); len(seen) != 0 {
		t.Errorf("no request should be made, got %q", seen)
	}
}

func TestRecentVODsCancelledBetweenRequests(t *testing.T) {
	l, h := vodsServe(t, vodsFixtureReplies(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.onRequest = func(endpoint string) {
		if endpoint == "videos" {
			cancel()
		}
	}

	vods, err := l.RecentVODs(ctx, "some_streamer")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if vods != nil {
		t.Errorf("no VODs should be returned, got %+v", vods)
	}
	// Whether the cancelled request itself still completed does not matter;
	// nothing may be asked after it.
	if seen := h.seen(); len(seen) != 3 {
		t.Errorf("want 3 requests, got %q", seen)
	}
}

func TestVODsParseDuration(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"3h2m1s", 3*time.Hour + 2*time.Minute + time.Second},
		{"3m21s", 3*time.Minute + 21*time.Second},
		{"45s", 45 * time.Second},
		{"0s", 0},
		{"1h", time.Hour},
		{"5h30m", 5*time.Hour + 30*time.Minute},
		{"1h0m0s", time.Hour},
		{"2h5s", 2*time.Hour + 5*time.Second},
		{"48h0m1s", 48*time.Hour + time.Second},
		{"100h59m59s", 100*time.Hour + 59*time.Minute + 59*time.Second},
		{"90m", 90 * time.Minute},
		{"07m08s", 7*time.Minute + 8*time.Second},
	}
	for _, tt := range tests {
		got, err := vodsParseDuration(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("vodsParseDuration(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
		}
	}

	for _, in := range []string{
		"", " ", "h", "s", "1", "90", "1:02:03", "PT3H2M1S", "3H2M1S", "1d2h", "1.5h", "1h2.5s",
		"-1h", "+1h", "1h -2m", "1s2m", "2m1h", "1h1h", "3h2m1s ", " 3h2m1s", "1h2m3", "100ms",
		"99999999999999999999h", "1000001h",
	} {
		if got, err := vodsParseDuration(in); err == nil {
			t.Errorf("vodsParseDuration(%q) = %v, want an error", in, got)
		}
	}
}

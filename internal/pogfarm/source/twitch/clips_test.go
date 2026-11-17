package twitch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
)

// The files in testdata/clips_*.json have the shape of Helix responses with
// made-up values. No test contacts Twitch.
const (
	clipsVODID       = "2001"
	clipsBroadcaster = "141981764"
)

// clipsVOD is the VOD the fixtures describe: three hours long.
func clipsVOD() source.VOD {
	return source.VOD{
		Platform:  api.PlatformTwitch,
		ID:        clipsVODID,
		Streamer:  api.Streamer{Login: "examplestreamer", DisplayName: "ExampleStreamer"},
		Title:     "ranked grind",
		StartedAt: time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC),
		Duration:  3 * time.Hour,
		Ended:     true,
	}
}

// clipsFixture reads one recorded response.
func clipsFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// clipsHelix is the Helix side of a fakeTwitch for clip listing. It answers
// the video lookup with video, and a clips request with the page stored
// under the request's "after" cursor ("" for the first page). It records
// the query of every request.
type clipsHelix struct {
	video func(w http.ResponseWriter)
	pages map[string]func(w http.ResponseWriter)

	mu     sync.Mutex
	videos []url.Values
	clips  []url.Values
}

// clipsBody answers with a 200 and the given body.
func clipsBody(body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) { writeJSON(w, http.StatusOK, body) }
}

// clipsStatus answers with an error status and a Helix error body.
func clipsStatus(status int, message string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		writeJSON(w, status, fmt.Sprintf(`{"error":%q,"status":%d,"message":%q}`, http.StatusText(status), status, message))
	}
}

// newClipsHelix starts a fake Twitch that knows the fixture VOD and serves
// the given clip pages, and returns a lister talking to it.
func newClipsHelix(t *testing.T, pages map[string]func(w http.ResponseWriter)) (*ClipLister, *clipsHelix, *fakeTwitch) {
	t.Helper()
	h := &clipsHelix{video: clipsBody(clipsFixture(t, "clips_video.json")), pages: pages}
	f := newFakeTwitch(t)
	f.helix = func(w http.ResponseWriter, r *http.Request, _ int) {
		q := r.URL.Query()
		h.mu.Lock()
		video := h.video
		switch r.URL.Path {
		case "/helix/videos":
			h.videos = append(h.videos, q)
		case "/helix/clips":
			h.clips = append(h.clips, q)
		}
		h.mu.Unlock()

		switch r.URL.Path {
		case "/helix/videos":
			video(w)
		case "/helix/clips":
			page, ok := h.pages[q.Get("after")]
			if !ok {
				writeJSON(w, http.StatusBadRequest, `{"message":"unexpected cursor"}`)
				return
			}
			page(w)
		default:
			writeJSON(w, http.StatusNotFound, `{"message":"unexpected path"}`)
		}
	}
	c, _ := newTestClient(t, f)
	return NewClipLister(c), h, f
}

// requests returns the queries of the video lookups and the clips requests
// seen so far.
func (h *clipsHelix) requests() (videos, clips []url.Values) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.videos), slices.Clone(h.clips)
}

// clipsWantRequests fails the test unless this many video lookups and clips
// requests were made.
func clipsWantRequests(t *testing.T, h *clipsHelix, videos, clips int) {
	t.Helper()
	gotVideos, gotClips := h.requests()
	if len(gotVideos) != videos || len(gotClips) != clips {
		t.Errorf("requests: %d video lookups, %d clips pages; want %d and %d", len(gotVideos), len(gotClips), videos, clips)
	}
}

// clipsThreePages serves the three recorded pages in order.
func clipsThreePages(t *testing.T) map[string]func(w http.ResponseWriter) {
	t.Helper()
	return map[string]func(w http.ResponseWriter){
		"":              clipsBody(clipsFixture(t, "clips_page1.json")),
		"cursor-page-2": clipsBody(clipsFixture(t, "clips_page2.json")),
		"cursor-page-3": clipsBody(clipsFixture(t, "clips_page3.json")),
	}
}

func TestClipsSinglePage(t *testing.T) {
	l, h, _ := newClipsHelix(t, map[string]func(w http.ResponseWriter){
		"": clipsBody(clipsFixture(t, "clips_page3.json")),
	})

	got, err := l.ViewerClips(context.Background(), clipsVOD())
	if err != nil {
		t.Fatal(err)
	}
	want := []source.ViewerClip{
		{Offset: 0, Duration: time.Minute, Views: 3, Title: "Stream starts"},
		{Offset: 7300 * time.Second, Duration: 18750 * time.Millisecond, Views: 12, Title: "Clipped from the VOD later"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("clips:\n got %+v\nwant %+v", got, want)
	}

	videos, clips := h.requests()
	if len(videos) != 1 || len(clips) != 1 {
		t.Fatalf("requests: %d video lookups, %d clips pages; want 1 and 1", len(videos), len(clips))
	}
	if got := videos[0]; len(got) != 1 || got.Get("id") != clipsVODID {
		t.Errorf("video lookup query: %v", got)
	}
	wantQuery := url.Values{
		"broadcaster_id": {clipsBroadcaster},
		"started_at":     {"2026-11-10T18:00:00Z"},
		// The end of the three hour VOD plus the day of grace.
		"ended_at": {"2026-11-11T21:00:00Z"},
		"first":    {"100"},
	}
	if clips[0].Encode() != wantQuery.Encode() {
		t.Errorf("clips query:\n got %v\nwant %v", clips[0], wantQuery)
	}
}

func TestClipsTimeRangeIsSentInUTC(t *testing.T) {
	l, h, _ := newClipsHelix(t, map[string]func(w http.ResponseWriter){
		"": clipsBody(clipsFixture(t, "clips_empty.json")),
	})
	vod := clipsVOD()
	vod.StartedAt = vod.StartedAt.In(time.FixedZone("plus4", 4*60*60))
	vod.Duration = 90 * time.Minute

	if _, err := l.ViewerClips(context.Background(), vod); err != nil {
		t.Fatal(err)
	}
	_, clips := h.requests()
	if len(clips) != 1 {
		t.Fatalf("clips pages: %d", len(clips))
	}
	if got := clips[0].Get("started_at"); got != "2026-11-10T18:00:00Z" {
		t.Errorf("started_at: %q", got)
	}
	if got := clips[0].Get("ended_at"); got != "2026-11-11T19:30:00Z" {
		t.Errorf("ended_at: %q", got)
	}
}

// The three pages hold a clip of another VOD, a clip with no VOD, a clip of
// this VOD whose offset is null, and one clip twice. What is left comes back
// ordered by offset, and the two clips at the same offset most viewed first.
func TestClipsFollowsPaginationFiltersAndOrders(t *testing.T) {
	l, h, f := newClipsHelix(t, clipsThreePages(t))

	got, err := l.ViewerClips(context.Background(), clipsVOD())
	if err != nil {
		t.Fatal(err)
	}
	want := []source.ViewerClip{
		{Offset: 0, Duration: time.Minute, Views: 3, Title: "Stream starts"},
		{Offset: 2 * time.Minute, Duration: 29900 * time.Millisecond, Views: 40, Title: "Intro fail"},
		{Offset: 90 * time.Minute, Duration: 45200 * time.Millisecond, Views: 1200, Title: "INSANE clutch"},
		{Offset: 90 * time.Minute, Duration: 30 * time.Second, Views: 900, Title: "Clutch 1v3"},
		{Offset: 7300 * time.Second, Duration: 18750 * time.Millisecond, Views: 12, Title: "Clipped from the VOD later"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("clips:\n got %+v\nwant %+v", got, want)
	}
	if !slices.IsSortedFunc(got, func(a, b source.ViewerClip) int { return int(a.Offset - b.Offset) }) {
		t.Errorf("clips are not ordered by offset: %+v", got)
	}

	_, clips := h.requests()
	var cursors []string
	for _, q := range clips {
		cursors = append(cursors, q.Get("after"))
		if q.Get("broadcaster_id") != clipsBroadcaster || q.Get("first") != "100" ||
			q.Get("started_at") != "2026-11-10T18:00:00Z" || q.Get("ended_at") != "2026-11-11T21:00:00Z" {
			t.Errorf("page query lost the filter: %v", q)
		}
	}
	if !slices.Equal(cursors, []string{"", "cursor-page-2", "cursor-page-3"}) {
		t.Errorf("cursors followed: %q", cursors)
	}
	// One token, one video lookup, three pages.
	f.wantCalls(t, 1, 4)
}

func TestClipsOffsetVariants(t *testing.T) {
	page := `{"data":[
		{"id":"a","video_id":"2001","title":"missing offset field","view_count":1,"duration":10},
		{"id":"b","video_id":"2001","title":"null offset","view_count":2,"duration":10,"vod_offset":null},
		{"id":"c","video_id":"2001","title":"negative offset","view_count":3,"duration":10,"vod_offset":-5},
		{"id":"d","video_id":"","title":"offset without a vod","view_count":4,"duration":10,"vod_offset":30},
		{"id":"e","video_id":"20010","title":"other vod with a similar id","view_count":5,"duration":10,"vod_offset":30},
		{"id":"f","video_id":"2001","title":"kept","view_count":6,"duration":12.345,"vod_offset":61},
		{"id":"g","video_id":"2001","title":"kept at zero","view_count":7,"duration":5,"vod_offset":0}
	],"pagination":{}}`
	l, _, _ := newClipsHelix(t, map[string]func(w http.ResponseWriter){"": clipsBody(page)})

	got, err := l.ViewerClips(context.Background(), clipsVOD())
	if err != nil {
		t.Fatal(err)
	}
	want := []source.ViewerClip{
		{Offset: 0, Duration: 5 * time.Second, Views: 7, Title: "kept at zero"},
		{Offset: 61 * time.Second, Duration: 12345 * time.Millisecond, Views: 6, Title: "kept"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("clips:\n got %+v\nwant %+v", got, want)
	}
}

func TestClipsEmptyResult(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"no clips", clipsFixture(t, "clips_empty.json")},
		{"no pagination object", `{"data":[]}`},
		{"only clips of other vods", `{"data":[{"id":"x","video_id":"7","vod_offset":10,"duration":5}],"pagination":{}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, h, _ := newClipsHelix(t, map[string]func(w http.ResponseWriter){"": clipsBody(tt.body)})
			got, err := l.ViewerClips(context.Background(), clipsVOD())
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Errorf("want no clips, got %+v", got)
			}
			clipsWantRequests(t, h, 1, 1)
		})
	}
}

func TestClipsVODNotFound(t *testing.T) {
	tests := []struct {
		name  string
		video func(w http.ResponseWriter)
	}{
		{"404", clipsStatus(http.StatusNotFound, "video not found")},
		{"empty data", clipsBody(`{"data":[],"pagination":{}}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, h, _ := newClipsHelix(t, clipsThreePages(t))
			h.video = tt.video

			got, err := l.ViewerClips(context.Background(), clipsVOD())
			if !errors.Is(err, source.ErrVODNotFound) {
				t.Fatalf("want ErrVODNotFound, got %v", err)
			}
			if got != nil {
				t.Errorf("clips returned with an error: %+v", got)
			}
			if !strings.Contains(err.Error(), `"2001"`) {
				t.Errorf("error does not name the vod: %v", err)
			}
			noSecrets(t, err)
			// The clips endpoint is not asked about a VOD that is gone.
			clipsWantRequests(t, h, 1, 0)
		})
	}
}

func TestClipsVideoWithoutBroadcaster(t *testing.T) {
	l, h, _ := newClipsHelix(t, clipsThreePages(t))
	h.video = clipsBody(`{"data":[{"id":"2001"}]}`)

	_, err := l.ViewerClips(context.Background(), clipsVOD())
	if err == nil || errors.Is(err, source.ErrVODNotFound) {
		t.Fatalf("want an error that is not ErrVODNotFound, got %v", err)
	}
	clipsWantRequests(t, h, 1, 0)
}

func TestClipsAPIErrors(t *testing.T) {
	tests := []struct {
		name string
		// set breaks one of the responses.
		set        func(h *clipsHelix)
		wantStatus int
		wantClips  int // clips requests made
	}{
		{
			name:       "video lookup fails",
			set:        func(h *clipsHelix) { h.video = clipsStatus(http.StatusInternalServerError, "boom") },
			wantStatus: http.StatusInternalServerError,
			wantClips:  0,
		},
		{
			name:       "first page fails",
			set:        func(h *clipsHelix) { h.pages[""] = clipsStatus(http.StatusBadRequest, "bad broadcaster_id") },
			wantStatus: http.StatusBadRequest,
			wantClips:  1,
		},
		{
			name: "later page fails",
			set: func(h *clipsHelix) {
				h.pages["cursor-page-3"] = clipsStatus(http.StatusServiceUnavailable, "try later")
			},
			wantStatus: http.StatusServiceUnavailable,
			wantClips:  3,
		},
		{
			name:       "clips endpoint says not found",
			set:        func(h *clipsHelix) { h.pages[""] = clipsStatus(http.StatusNotFound, "no such thing") },
			wantStatus: http.StatusNotFound,
			wantClips:  1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, h, _ := newClipsHelix(t, clipsThreePages(t))
			tt.set(h)

			got, err := l.ViewerClips(context.Background(), clipsVOD())
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("want an *APIError, got %v", err)
			}
			if apiErr.Status != tt.wantStatus {
				t.Errorf("status %d, want %d", apiErr.Status, tt.wantStatus)
			}
			// Only the video lookup says whether the VOD exists.
			if tt.wantClips > 0 && errors.Is(err, source.ErrVODNotFound) {
				t.Errorf("a failing clips request was reported as a missing vod: %v", err)
			}
			// A partial list is never returned.
			if got != nil {
				t.Errorf("clips returned with an error: %+v", got)
			}
			if !strings.Contains(err.Error(), `viewer clips of vod "2001"`) {
				t.Errorf("error does not say what failed: %v", err)
			}
			noSecrets(t, err)
			clipsWantRequests(t, h, 1, tt.wantClips)
		})
	}
}

func TestClipsMalformedPage(t *testing.T) {
	l, _, _ := newClipsHelix(t, map[string]func(w http.ResponseWriter){"": clipsBody(`{"data":[{"id":`)})
	got, err := l.ViewerClips(context.Background(), clipsVOD())
	if err == nil {
		t.Fatalf("want an error, got clips %+v", got)
	}
	if got != nil {
		t.Errorf("clips returned with an error: %+v", got)
	}
}

// A cursor that never ends is followed for clipsMaxPages pages, and what
// was found by then is returned.
func TestClipsPaginationIsBounded(t *testing.T) {
	var mu sync.Mutex
	n := 0
	endless := func(w http.ResponseWriter) {
		mu.Lock()
		n++
		i := n
		mu.Unlock()
		writeJSON(w, http.StatusOK, fmt.Sprintf(
			`{"data":[{"id":"clip-%d","video_id":"2001","title":"clip %d","view_count":1,"duration":10,"vod_offset":%d}],"pagination":{"cursor":"again"}}`,
			i, i, 10000-i))
	}
	l, h, _ := newClipsHelix(t, map[string]func(w http.ResponseWriter){"": endless, "again": endless})

	got, err := l.ViewerClips(context.Background(), clipsVOD())
	if err != nil {
		t.Fatal(err)
	}
	clipsWantRequests(t, h, 1, clipsMaxPages)
	if len(got) != clipsMaxPages {
		t.Fatalf("got %d clips, want %d", len(got), clipsMaxPages)
	}
	// The pages came with falling offsets; the result is ordered.
	if got[0].Title != fmt.Sprintf("clip %d", clipsMaxPages) || got[len(got)-1].Title != "clip 1" {
		t.Errorf("not ordered by offset: first %q, last %q", got[0].Title, got[len(got)-1].Title)
	}
}

func TestClipsInvalidVOD(t *testing.T) {
	tests := []struct {
		name string
		edit func(v *source.VOD)
	}{
		{"no id", func(v *source.VOD) { v.ID = "" }},
		{"no start time", func(v *source.VOD) { v.StartedAt = time.Time{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, _, f := newClipsHelix(t, clipsThreePages(t))
			vod := clipsVOD()
			tt.edit(&vod)
			if _, err := l.ViewerClips(context.Background(), vod); err == nil {
				t.Fatal("want an error")
			}
			f.wantCalls(t, 0, 0)
		})
	}
}

func TestClipsCancelledContextMakesNoRequest(t *testing.T) {
	l, _, f := newClipsHelix(t, clipsThreePages(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := l.ViewerClips(ctx, clipsVOD())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if got != nil {
		t.Errorf("clips returned with an error: %+v", got)
	}
	f.wantCalls(t, 0, 0)
}

// The context is cancelled while the first page is being served. Whether or
// not that response still arrives, the listing stops with the context's
// error and never asks for the second page.
func TestClipsCancelBetweenPages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pages := clipsThreePages(t)
	first := pages[""]
	pages[""] = func(w http.ResponseWriter) {
		cancel()
		first(w)
	}
	l, h, _ := newClipsHelix(t, pages)

	got, err := l.ViewerClips(ctx, clipsVOD())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if got != nil {
		t.Errorf("clips returned with an error: %+v", got)
	}
	clipsWantRequests(t, h, 1, 1)
}

func TestClipsCancelDuringVideoLookup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, h, _ := newClipsHelix(t, clipsThreePages(t))
	video := h.video
	h.video = func(w http.ResponseWriter) {
		cancel()
		video(w)
	}

	_, err := l.ViewerClips(ctx, clipsVOD())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	clipsWantRequests(t, h, 1, 0)
}

func TestClipsSeconds(t *testing.T) {
	tests := []struct {
		in   float64
		want time.Duration
	}{
		{0, 0},
		{30, 30 * time.Second},
		{29.9, 29900 * time.Millisecond},
		{0.1, 100 * time.Millisecond},
		{59.9999, time.Minute},
	}
	for _, tt := range tests {
		if got := clipsSeconds(tt.in); got != tt.want {
			t.Errorf("clipsSeconds(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

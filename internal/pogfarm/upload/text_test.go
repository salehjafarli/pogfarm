package upload

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/config"
)

var posttextDate = time.Date(2026, 11, 16, 21, 30, 0, 0, time.UTC)

func posttextSource() Source {
	return Source{
		Platform: api.PlatformTwitch, Login: "somestreamer", DisplayName: "SomeStreamer",
		VODTitle: "Ranked grind day 3", VODStartedAt: posttextDate,
	}
}

func posttextTranscript(words ...string) api.Transcript {
	t := api.Transcript{Language: "en"}
	for i, w := range words {
		t.Words = append(t.Words, api.Word{Text: w, StartSeconds: float64(i), EndSeconds: float64(i) + 0.5})
	}
	return t
}

func posttextDefault() config.PostTemplate {
	var cfg config.Post
	cfg.Default = config.PostTemplate{
		Title:       "{hook} | {streamer}",
		Description: "{hook}\n\n{hashtags}",
		Hashtags:    []string{"shorts", "{login}"},
		Credit:      "Credit: {streamer}, streamed live on {date}: {channel_url}",
	}
	return cfg.Default
}

// unlimited is larger than anything a test generates.
var unlimited = Limits{Title: 1 << 20, Description: 1 << 20, Tags: 1 << 20}

const posttextCredit = "Credit: SomeStreamer, streamed live on 2026-11-16: https://www.twitch.tv/somestreamer"

func TestGenerate(t *testing.T) {
	long := make([]string, 1000)
	for i := range long {
		long[i] = fmt.Sprintf("word%d", i)
	}
	tests := []struct {
		name       string
		tmpl       config.PostTemplate
		src        Source
		transcript api.Transcript
		want       Text
	}{
		{
			name:       "first sentence is the hook",
			tmpl:       posttextDefault(),
			src:        posttextSource(),
			transcript: posttextTranscript(" No", "way", "he", "hit", "that.", "Unreal", "chat"),
			want: Text{
				Title:       "No way he hit that | SomeStreamer",
				Description: "No way he hit that\n\n#shorts #somestreamer\n\n" + posttextCredit,
				Tags:        []string{"shorts", "somestreamer"},
			},
		},
		{
			name:       "a sentence ends only after a few words",
			tmpl:       config.PostTemplate{Title: "{hook}"},
			src:        posttextSource(),
			transcript: posttextTranscript("Yes.", "Yes!", "Let's", "go!", "More"),
			want:       Text{Title: "Yes. Yes! Let's go!"},
		},
		{
			name:       "empty transcript: the streamer's name is the hook",
			tmpl:       posttextDefault(),
			src:        posttextSource(),
			transcript: api.Transcript{},
			want: Text{
				Title:       "SomeStreamer | SomeStreamer",
				Description: "SomeStreamer\n\n#shorts #somestreamer\n\n" + posttextCredit,
				Tags:        []string{"shorts", "somestreamer"},
			},
		},
		{
			name:       "transcript of blank words counts as empty",
			tmpl:       config.PostTemplate{Title: "{hook}"},
			src:        posttextSource(),
			transcript: posttextTranscript(" ", "\n", "\x00"),
			want:       Text{Title: "SomeStreamer"},
		},
		{
			name:       "very long transcript: a few words and an ellipsis",
			tmpl:       config.PostTemplate{Title: "{hook}"},
			src:        posttextSource(),
			transcript: posttextTranscript(long...),
			want:       Text{Title: strings.Join(long[:hookMaxWords], " ") + "…"},
		},
		{
			name: "unicode and control characters",
			tmpl: config.PostTemplate{Title: "{hook} — {streamer}", Hashtags: []string{"#{login}", "Ünïcødé", "🔥", "a-b"}},
			src: Source{Platform: api.PlatformTwitch, Login: "über_streamer", DisplayName: "Über​Streamer\n",
				VODStartedAt: posttextDate},
			transcript: posttextTranscript("Ну", "это\x07", "было", "🔥🔥", "невероятно!", "да"),
			want: Text{
				Title: "Ну это было 🔥🔥 невероятно! — Über​Streamer",
				Tags:  []string{"über_streamer", "Ünïcødé", "ab"},
			},
		},
		{
			name: "every placeholder",
			tmpl: config.PostTemplate{
				Title:       "{streamer} {login} {date}",
				Description: "{vod_title} / {channel_url} / {hashtags} / {hook}",
				Hashtags:    []string{"x", "X", "#y"},
				Credit:      "by {login}",
			},
			src:        posttextSource(),
			transcript: posttextTranscript("hi", "there"),
			want: Text{
				Title:       "SomeStreamer somestreamer 2026-11-16",
				Description: "Ranked grind day 3 / https://www.twitch.tv/somestreamer / #x #y / hi there\n\nby somestreamer",
				Tags:        []string{"x", "y"},
			},
		},
		{
			name: "missing streamer data",
			tmpl: posttextDefault(),
			src:  Source{},
			want: Text{
				Title:       "the streamer | the streamer",
				Description: "the streamer\n\n#shorts\n\nCredit: the streamer, streamed live on :",
				Tags:        []string{"shorts"},
			},
		},
		{
			name: "display name missing: the login",
			tmpl: config.PostTemplate{Title: "{streamer}", Credit: "{streamer}"},
			src:  Source{Platform: api.PlatformTwitch, Login: "lower"},
			want: Text{Title: "lower", Description: "lower"},
		},
		{
			name: "title that renders empty falls back to the streamer",
			tmpl: config.PostTemplate{Title: "{vod_title}", Credit: "c"},
			src:  Source{DisplayName: "Name"},
			want: Text{Title: "Name", Description: "c"},
		},
		{
			name: "per-streamer override",
			tmpl: (config.Post{
				Default: posttextDefault(),
				Streamers: []config.PostOverride{{
					Platform: api.PlatformTwitch, Login: "somestreamer",
					Title: "{streamer}: {hook}", Hashtags: []string{},
				}},
			}).For(api.PlatformTwitch, "somestreamer"),
			src:        posttextSource(),
			transcript: posttextTranscript("gg"),
			want: Text{
				Title:       "SomeStreamer: gg",
				Description: "gg\n\n" + posttextCredit,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Generate(tt.tmpl, tt.src, tt.transcript, unlimited)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got\n%#v\nwant\n%#v", got, tt.want)
			}
			if got.Title == "" {
				t.Error("empty title")
			}
		})
	}
}

// Every placeholder the configuration accepts has a value.
func TestEveryConfigPlaceholderIsRendered(t *testing.T) {
	for _, name := range config.PostPlaceholders {
		tmpl := config.PostTemplate{Title: "t", Description: "{" + name + "}", Hashtags: []string{"x"}}
		got, err := Generate(tmpl, posttextSource(), posttextTranscript("hi"), unlimited)
		if err != nil {
			t.Fatal(err)
		}
		if got.Description == "" || strings.Contains(got.Description, "{") {
			t.Errorf("{%s} renders as %q", name, got.Description)
		}
	}
}

func TestChannelURL(t *testing.T) {
	if got := ChannelURL(api.PlatformTwitch, "abc"); got != "https://www.twitch.tv/abc" {
		t.Errorf("twitch: %q", got)
	}
	if got := ChannelURL("kick", "abc"); got != "" {
		t.Errorf("unknown platform: %q", got)
	}
	if got := ChannelURL(api.PlatformTwitch, ""); got != "" {
		t.Errorf("no login: %q", got)
	}
}

func TestTruncate(t *testing.T) {
	runes := utf8.RuneCountInString
	bytes := func(s string) int { return len(s) }
	tests := []struct {
		s       string
		limit   int
		measure func(string) int
		want    string
	}{
		{"short", 10, runes, "short"},
		{"exactly ten", 11, runes, "exactly ten"},
		{"hello brave new world", 15, runes, "hello brave…"},
		{"hello brave new world", 12, runes, "hello brave…"},
		{"hello, brave", 9, runes, "hello…"},
		{"supercalifragilistic", 8, runes, "superca…"},
		{"ab", 1, runes, ""},
		{"ab", 0, runes, ""},
		{"🔥🔥🔥 🔥🔥🔥", 5, runes, "🔥🔥🔥…"},
		// "ééé ééé" is 13 bytes; 10 bytes leave 7 for the text.
		{"ééé ééé", 10, bytes, "ééé…"},
		{"éééééé", 6, bytes, "é…"},
	}
	for _, tt := range tests {
		got := truncate(tt.s, tt.limit, tt.measure)
		if got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.s, tt.limit, got, tt.want)
		}
		if tt.measure(got) > tt.limit {
			t.Errorf("truncate(%q, %d) = %q is over the limit", tt.s, tt.limit, got)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncate(%q, %d) = %q is not valid UTF-8", tt.s, tt.limit, got)
		}
	}
}

// Under every target's limits the parts fit, and the credit is there in
// full at the end of the description.
func TestLimitsKeepTheCredit(t *testing.T) {
	words := make([]string, 3000)
	for i := range words {
		words[i] = "ünïcødé"
	}
	hashtags := make([]string, 200)
	for i := range hashtags {
		hashtags[i] = fmt.Sprintf("tag%03d", i)
	}
	tmpl := config.PostTemplate{
		Title:       strings.Repeat("{hook} ", 20),
		Description: strings.Repeat("{vod_title} {hook}\n", 400) + "{hashtags}",
		Hashtags:    hashtags,
		Credit:      "Credit: {streamer} {channel_url}",
	}
	src := posttextSource()
	src.VODTitle = strings.Repeat("ß", 300)
	credit := "Credit: SomeStreamer https://www.twitch.tv/somestreamer"

	cases := map[string]Limits{
		"youtube": LimitsFor("youtube"),
		"fake":    LimitsFor("fake"),
		"tiny":    {Title: 10, Description: len(credit) + 5, Tags: 13},
		"exact":   {Title: 1, Description: len(credit), Tags: 0},
	}
	for name, limits := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Generate(tmpl, src, posttextTranscript(words...), limits)
			if err != nil {
				t.Fatal(err)
			}
			measure := utf8.RuneCountInString
			if limits.DescriptionInBytes {
				measure = func(s string) int { return len(s) }
			}
			if n := utf8.RuneCountInString(got.Title); n > limits.Title {
				t.Errorf("title is %d runes, limit %d", n, limits.Title)
			}
			if got.Title == "" && limits.Title > 1 {
				t.Error("empty title")
			}
			if n := measure(got.Description); n > limits.Description {
				t.Errorf("description is %d, limit %d", n, limits.Description)
			}
			if !strings.HasSuffix(got.Description, credit) {
				t.Errorf("description does not end with the credit: ...%q", tail(got.Description, 80))
			}
			total := 0
			for i, tag := range got.Tags {
				total += utf8.RuneCountInString(tag)
				if i > 0 {
					total++
				}
				if tag != hashtags[i] {
					t.Fatalf("tag %d = %q, want %q: tags are dropped from the end", i, tag, hashtags[i])
				}
			}
			if total > limits.Tags {
				t.Errorf("tags are %d long, limit %d", total, limits.Tags)
			}
			for _, s := range []string{got.Title, got.Description} {
				if !utf8.ValidString(s) {
					t.Errorf("invalid UTF-8: %q", s)
				}
			}
		})
	}

	// Cut text ends at a word boundary with an ellipsis.
	got, _ := Generate(tmpl, src, posttextTranscript(words...), LimitsFor("youtube"))
	if !strings.HasSuffix(got.Title, "…") || strings.Contains(got.Title, "ünïcødé ü…") {
		t.Errorf("title not cut at a word: %q", got.Title)
	}
	body := strings.TrimSuffix(got.Description, "\n\n"+credit)
	if !strings.HasSuffix(body, "…") {
		t.Errorf("description body not cut with an ellipsis: ...%q", tail(body, 40))
	}
	if len(got.Tags) == len(hashtags) || len(got.Tags) == 0 {
		t.Errorf("%d of %d tags kept; want some dropped", len(got.Tags), len(hashtags))
	}
}

func TestCreditThatCannotFit(t *testing.T) {
	tmpl := config.PostTemplate{Title: "t", Description: "body", Credit: "Credit: {streamer} {channel_url}"}
	_, err := Generate(tmpl, posttextSource(), api.Transcript{}, Limits{Title: 100, Description: 20, Tags: 100})
	if !errors.Is(err, ErrCreditTooLong) {
		t.Errorf("error = %v, want ErrCreditTooLong", err)
	}
}

func TestFitTitle(t *testing.T) {
	yt := LimitsFor("youtube")
	if got := FitTitle("  my\nnew   title ", yt); got != "my new title" {
		t.Errorf("FitTitle = %q", got)
	}
	long := strings.Repeat("word ", 50)
	if got := FitTitle(long, yt); utf8.RuneCountInString(got) > yt.Title || !strings.HasSuffix(got, "word…") {
		t.Errorf("FitTitle(long) = %q", got)
	}
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

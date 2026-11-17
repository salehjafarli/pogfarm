package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/salehjafarli/pogfarm/internal/api"
)

func TestPostDefaults(t *testing.T) {
	cleanEnv(t)
	t.Setenv("POGFARM_API_SECRET", goodSecret)
	cfg, err := LoadPogfarm("")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Post, defaultPost()) {
		t.Errorf("post defaults:\n got %+v\nwant %+v", cfg.Post, defaultPost())
	}
	if p := defaultPost(); len(p.validate()) != 0 {
		t.Errorf("the defaults are invalid: %v", p.validate())
	}
}

func TestPostFromFile(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, "config.yaml", `
pogfarm:
  api_secret: `+goodSecret+`
  post:
    default:
      title: "{hook} ({streamer})"
      hashtags: ["#clips"]
    streamers:
      - platform: twitch
        login: "Some_Streamer"
        credit: "All credit to {streamer}: {channel_url}"
      - platform: twitch
        login: quiet
        hashtags: []
`)
	cfg, err := LoadPogfarm(path)
	if err != nil {
		t.Fatal(err)
	}
	want := defaultPost()
	want.Default.Title = "{hook} ({streamer})"
	want.Default.Hashtags = []string{"#clips"}
	want.Streamers = []PostOverride{
		{Platform: api.PlatformTwitch, Login: "some_streamer", Credit: "All credit to {streamer}: {channel_url}"},
		{Platform: api.PlatformTwitch, Login: "quiet", Hashtags: []string{}},
	}
	if !reflect.DeepEqual(cfg.Post, want) {
		t.Fatalf("got\n%#v\nwant\n%#v", cfg.Post, want)
	}

	// The override replaces only what it sets.
	got := cfg.Post.For(api.PlatformTwitch, "SOME_streamer")
	if got.Credit != "All credit to {streamer}: {channel_url}" || got.Title != want.Default.Title ||
		got.Description != want.Default.Description || !reflect.DeepEqual(got.Hashtags, []string{"#clips"}) {
		t.Errorf("For(some_streamer) = %+v", got)
	}
	// "hashtags: []" means none, not the default's.
	if got := cfg.Post.For(api.PlatformTwitch, "quiet"); got.Hashtags == nil || len(got.Hashtags) != 0 {
		t.Errorf("For(quiet).Hashtags = %#v, want empty", got.Hashtags)
	}
	// Anyone else gets the default.
	if got := cfg.Post.For(api.PlatformTwitch, "other"); !reflect.DeepEqual(got, want.Default) {
		t.Errorf("For(other) = %+v, want the default", got)
	}
	if got := cfg.Post.For("kick", "some_streamer"); !reflect.DeepEqual(got, want.Default) {
		t.Errorf("an override applies on its own platform only: got %+v", got)
	}
}

func TestPostValidation(t *testing.T) {
	tests := []struct {
		name string
		post Post
		want []string
	}{
		{"unknown placeholder in title", Post{Default: PostTemplate{Title: "{hook} {game}", Credit: "c"}},
			[]string{"pogfarm.post.default.title: unknown placeholder {game}"}},
		{"unknown placeholder in credit", Post{Default: PostTemplate{Title: "t", Credit: "{channel}"}},
			[]string{"pogfarm.post.default.credit: unknown placeholder {channel}"}},
		{"unknown placeholder in hashtag", Post{Default: PostTemplate{Title: "t", Credit: "c", Hashtags: []string{"{nick}"}}},
			[]string{"pogfarm.post.default.hashtags[0]: unknown placeholder {nick}"}},
		{"hashtags outside the description", Post{Default: PostTemplate{Title: "{hashtags}", Credit: "c"}},
			[]string{"pogfarm.post.default.title: {hashtags} may be used in the description only"}},
		{"empty title and credit", Post{Default: PostTemplate{Title: " ", Credit: ""}},
			[]string{"pogfarm.post.default.title must not be empty", "pogfarm.post.default.credit must not be empty"}},
		{"bad hashtags", Post{Default: PostTemplate{Title: "t", Credit: "c", Hashtags: []string{"#", "two words"}}},
			[]string{"pogfarm.post.default.hashtags[0] must be one word", "pogfarm.post.default.hashtags[1] must be one word"}},
		{"override needs a streamer", Post{Default: PostTemplate{Title: "t", Credit: "c"}, Streamers: []PostOverride{
			{}, {Platform: "kick", Login: "a b"}}},
			[]string{"pogfarm.post.streamers[0].platform is required", "pogfarm.post.streamers[0].login is required",
				"pogfarm.post.streamers[1].platform is not a platform", "pogfarm.post.streamers[1].login must be the account name alone"}},
		{"override repeated", Post{Default: PostTemplate{Title: "t", Credit: "c"}, Streamers: []PostOverride{
			{Platform: api.PlatformTwitch, Login: "a"}, {Platform: api.PlatformTwitch, Login: "A"}}},
			[]string{"pogfarm.post.streamers[1] repeats the streamer twitch/a"}},
		{"override placeholders checked", Post{Default: PostTemplate{Title: "t", Credit: "c"}, Streamers: []PostOverride{
			{Platform: api.PlatformTwitch, Login: "a", Description: "{oops}"}}},
			[]string{"pogfarm.post.streamers[0].description: unknown placeholder {oops}"}},
		{"every placeholder is known", Post{Default: PostTemplate{
			Title:       "{streamer} {login} {channel_url} {hook} {date} {vod_title} { } {Hook} {",
			Description: "{hashtags} {hook}", Credit: "{streamer} {channel_url}", Hashtags: []string{"{login}", "#x"},
		}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.post.validate()
			if len(got) != len(tt.want) {
				t.Fatalf("got %d problems, want %d:\n%s", len(got), len(tt.want), strings.Join(got, "\n"))
			}
			for i, w := range tt.want {
				if !strings.HasPrefix(got[i], w) {
					t.Errorf("problem %d = %q, want it to start with %q", i, got[i], w)
				}
			}
		})
	}
}

func TestPostProblemsReachTheLoader(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, "config.yaml", `
pogfarm:
  api_secret: `+goodSecret+`
  post:
    default:
      title: "{nope}"
`)
	_, err := LoadPogfarm(path)
	got := strings.Join(problemsOf(t, err), "\n")
	if !strings.Contains(got, "pogfarm.post.default.title: unknown placeholder {nope}") {
		t.Errorf("problems: %s", got)
	}
}

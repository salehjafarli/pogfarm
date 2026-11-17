package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/salehjafarli/pogfarm/internal/api"
)

// Post is the "post" section of the main service's configuration: how the
// title, description and tags of a posted clip are built. It is set in the
// config file only.
//
// Patterns are plain text with placeholders in braces, such as
// "{hook} | {streamer}". The placeholders are listed in PostPlaceholders; any
// other name in braces is a configuration error. Text in braces that is not a
// lower-case name, such as "{ }" or "{Hook}", is kept as it is.
type Post struct {
	// Default is the template of every streamer without an override.
	Default PostTemplate `yaml:"default"`
	// Streamers are per-streamer overrides. A field left out of an override
	// takes the default's value.
	Streamers []PostOverride `yaml:"streamers"`
}

// PostTemplate is one set of patterns.
type PostTemplate struct {
	// Title is the pattern of the title.
	Title string `yaml:"title"`
	// Description is the pattern of the description. The credit line is
	// added after it and is never cut, so the description may be shortened
	// to fit a platform's limit but the credit never is.
	Description string `yaml:"description"`
	// Hashtags are the tags of every post, with or without a leading '#'.
	// They may hold placeholders such as "{login}".
	Hashtags []string `yaml:"hashtags"`
	// Credit is the pattern of the credit line naming the streamer and
	// linking their channel. It is required in the default.
	Credit string `yaml:"credit"`
}

// PostOverride is the template of one streamer. Title, Description and
// Credit left empty, and Hashtags left out, take the default's value;
// "hashtags: []" means no hashtags.
type PostOverride struct {
	// Platform and Login name the streamer, as in the allowlist. Login is
	// lower-cased when the configuration is loaded.
	Platform    api.Platform `yaml:"platform"`
	Login       string       `yaml:"login"`
	Title       string       `yaml:"title"`
	Description string       `yaml:"description"`
	Hashtags    []string     `yaml:"hashtags"`
	Credit      string       `yaml:"credit"`
}

// PostPlaceholders are the placeholders a post pattern may use:
//
//   - {streamer}: the streamer's display name, or the login when it has none
//   - {login}: the streamer's account name
//   - {channel_url}: the link to the streamer's channel on their platform
//   - {hook}: a short hook taken from the clip's transcript; the streamer's
//     name when the clip has no speech
//   - {date}: the date the VOD started, as 2006-01-02 (UTC)
//   - {vod_title}: the title of the VOD
//   - {hashtags}: the hashtags as "#one #two"; in the description only
var PostPlaceholders = []string{"streamer", "login", "channel_url", "hook", "date", "vod_title", "hashtags"}

// PostPlaceholderPattern matches a placeholder: a lower-case name in braces.
var PostPlaceholderPattern = regexp.MustCompile(`\{([a-z_]+)\}`)

// defaultPost is the post section when nothing is configured.
func defaultPost() Post {
	return Post{Default: PostTemplate{
		Title:       "{hook} | {streamer}",
		Description: "{hook}\n\n{hashtags}",
		Hashtags:    []string{"shorts", "{login}"},
		Credit:      "Credit: {streamer}, streamed live on {date}: {channel_url}",
	}}
}

// For returns the template of a streamer: the default with the streamer's
// override, if any, applied.
func (p Post) For(platform api.Platform, login string) PostTemplate {
	t := p.Default
	login = strings.ToLower(login)
	for _, o := range p.Streamers {
		if o.Platform != platform || strings.ToLower(o.Login) != login {
			continue
		}
		if o.Title != "" {
			t.Title = o.Title
		}
		if o.Description != "" {
			t.Description = o.Description
		}
		if o.Hashtags != nil {
			t.Hashtags = o.Hashtags
		}
		if o.Credit != "" {
			t.Credit = o.Credit
		}
		break
	}
	return t
}

// validate checks the post section and puts it in its loaded form: override
// logins lower-cased.
func (p *Post) validate() []string {
	const key = "pogfarm.post."
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, key+fmt.Sprintf(format, args...)) }

	if strings.TrimSpace(p.Default.Title) == "" {
		add("default.title must not be empty")
	}
	if strings.TrimSpace(p.Default.Credit) == "" {
		add("default.credit must not be empty: every post credits the streamer")
	}
	problems = append(problems, p.Default.check(key+"default")...)

	type streamerKey struct {
		platform api.Platform
		login    string
	}
	seen := map[streamerKey]bool{}
	for i := range p.Streamers {
		o := &p.Streamers[i]
		name := fmt.Sprintf("streamers[%d]", i)
		o.Login = strings.ToLower(o.Login)
		switch {
		case o.Platform == "":
			add("%s.platform is required, for example %q", name, api.PlatformTwitch)
		case o.Platform != api.PlatformTwitch:
			add("%s.platform is not a platform pogfarm knows: got %q, want %q", name, o.Platform, api.PlatformTwitch)
		}
		if o.Login == "" {
			add("%s.login is required: the streamer's account name", name)
		} else if strings.ContainsFunc(o.Login, func(r rune) bool { return r <= ' ' || r == '/' || r == 0x7f }) {
			add("%s.login must be the account name alone, without spaces or a URL: got %q", name, o.Login)
		}
		k := streamerKey{o.Platform, o.Login}
		if o.Login != "" && seen[k] {
			add("%s repeats the streamer %s/%s: give each streamer one override", name, o.Platform, o.Login)
		}
		seen[k] = true
		problems = append(problems, PostTemplate{
			Title: o.Title, Description: o.Description, Hashtags: o.Hashtags, Credit: o.Credit,
		}.check(key+name)...)
	}
	return problems
}

// check reports unknown placeholders and malformed hashtags. name is the
// template's full config key.
func (t PostTemplate) check(name string) []string {
	var problems []string
	field := func(field, pattern string, allowHashtags bool) {
		for _, m := range PostPlaceholderPattern.FindAllStringSubmatch(pattern, -1) {
			switch {
			case !slices.Contains(PostPlaceholders, m[1]):
				problems = append(problems, fmt.Sprintf("%s.%s: unknown placeholder %s; known: {%s}",
					name, field, m[0], strings.Join(PostPlaceholders, "}, {")))
			case m[1] == "hashtags" && !allowHashtags:
				problems = append(problems, fmt.Sprintf("%s.%s: {hashtags} may be used in the description only", name, field))
			}
		}
	}
	field("title", t.Title, false)
	field("description", t.Description, true)
	field("credit", t.Credit, false)
	for i, h := range t.Hashtags {
		f := fmt.Sprintf("hashtags[%d]", i)
		tag := strings.TrimPrefix(h, "#")
		if tag == "" || strings.ContainsFunc(tag, unicode.IsSpace) {
			problems = append(problems, fmt.Sprintf("%s.%s must be one word, with or without a leading '#': got %q", name, f, h))
			continue
		}
		field(f, tag, false)
	}
	return problems
}

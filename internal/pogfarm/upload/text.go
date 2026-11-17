package upload

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// Limits are the length limits of one target.
type Limits struct {
	// Title is the most characters (runes) of the title.
	Title int
	// Description is the most characters (runes) of the description, or
	// the most UTF-8 bytes when DescriptionInBytes is set.
	Description        int
	DescriptionInBytes bool
	// Tags is the most characters (runes) of all tags together, counting
	// one separator between two tags.
	Tags int
}

// targetLimits are the limits of the targets pogfarm knows, by target name.
//
// UNVERIFIED: these values come from the platforms' public documentation as
// remembered when this was written, not from a test against the platform.
// They are meant to be conservative; check them before relying on them.
var targetLimits = map[string]Limits{
	// YouTube: title 100 characters, description 5000 bytes, all tags
	// together 500 characters.
	"youtube": {Title: 100, Description: 5000, DescriptionInBytes: true, Tags: 500},
}

// DefaultLimits apply to a target that is not in the table. They are no
// larger than any known target's.
//
// UNVERIFIED, like targetLimits: 2200 is the caption limit short-video
// platforms commonly use.
var DefaultLimits = Limits{Title: 100, Description: 2200, Tags: 500}

// LimitsFor returns the limits of a target.
func LimitsFor(target string) Limits {
	if l, ok := targetLimits[target]; ok {
		return l
	}
	return DefaultLimits
}

// Source is what a post says about where the clip came from.
type Source struct {
	Platform    api.Platform
	Login       string
	DisplayName string
	VODTitle    string
	// VODStartedAt is when the VOD started; its date is {date}.
	VODStartedAt time.Time
}

// Text is the generated text of one post.
type Text struct {
	Title       string
	Description string
	Tags        []string
}

// ErrCreditTooLong is returned (wrapped) when the credit line alone does not
// fit in the target's description limit. The credit is never cut, so such a
// clip cannot be posted to that target with this template.
var ErrCreditTooLong = errors.New("upload: the credit line does not fit in the description")

// ChannelURL returns the link to a streamer's channel on their platform, or
// "" for a platform without a known URL scheme.
func ChannelURL(platform api.Platform, login string) string {
	if login == "" {
		return ""
	}
	switch platform {
	case api.PlatformTwitch:
		return "https://www.twitch.tv/" + login
	}
	return ""
}

// Hook limits: the hook is the first sentence of the transcript, at most
// hookMaxWords words and hookMaxRunes characters. A sentence ends only
// after hookMinWords words, so "Yes." alone does not make a hook.
const (
	hookMaxWords = 12
	hookMinWords = 3
	hookMaxRunes = 80
)

// Hook returns a short line from the start of a transcript: its first
// sentence, cut to a few words. It is "" for a transcript without words.
func Hook(t api.Transcript) string {
	var words []string
	for _, w := range t.Words {
		if s := oneLine(w.Text); s != "" {
			words = append(words, s)
		}
	}
	if len(words) == 0 {
		return ""
	}
	var out []string
	ended := false
	for _, w := range words {
		out = append(out, w)
		if endsSentence(w) && len(out) >= hookMinWords {
			ended = true
			break
		}
		if len(out) == hookMaxWords {
			break
		}
	}
	h := strings.TrimRight(strings.Join(out, " "), ".,;:-–— ")
	if !ended && len(out) < len(words) {
		h += "…"
	}
	return truncate(h, hookMaxRunes, utf8.RuneCountInString)
}

func endsSentence(w string) bool {
	r, _ := utf8.DecodeLastRuneInString(w)
	return strings.ContainsRune(".!?…。！？", r)
}

// Generate builds the text of one post from a template, the clip's source
// and transcript, within limits. The title is never empty. The credit line
// ends the description and is never cut; the rest of the description is cut
// first, and tags are dropped from the end, to fit.
func Generate(tmpl config.PostTemplate, src Source, transcript api.Transcript, limits Limits) (Text, error) {
	streamer := oneLine(src.DisplayName)
	if streamer == "" {
		streamer = oneLine(src.Login)
	}
	if streamer == "" {
		streamer = "the streamer"
	}
	hook := Hook(transcript)
	if hook == "" {
		hook = streamer
	}
	vals := map[string]string{
		"streamer":    streamer,
		"login":       oneLine(src.Login),
		"channel_url": ChannelURL(src.Platform, oneLine(src.Login)),
		"hook":        hook,
		"vod_title":   oneLine(src.VODTitle),
	}
	if !src.VODStartedAt.IsZero() {
		vals["date"] = src.VODStartedAt.UTC().Format("2006-01-02")
	}

	var text Text
	seen := map[string]bool{}
	for _, h := range tmpl.Hashtags {
		tag := cleanTag(render(strings.TrimPrefix(h, "#"), vals))
		if tag == "" || seen[strings.ToLower(tag)] {
			continue
		}
		seen[strings.ToLower(tag)] = true
		text.Tags = append(text.Tags, tag)
	}
	text.Tags = fitTags(text.Tags, limits.Tags)
	hashtags := make([]string, len(text.Tags))
	for i, tag := range text.Tags {
		hashtags[i] = "#" + tag
	}
	vals["hashtags"] = strings.Join(hashtags, " ")

	text.Title = FitTitle(render(tmpl.Title, vals), limits)
	if text.Title == "" {
		text.Title = FitTitle(streamer, limits)
	}

	measure := utf8.RuneCountInString
	if limits.DescriptionInBytes {
		measure = func(s string) int { return len(s) }
	}
	credit := cleanBlock(render(tmpl.Credit, vals))
	body := cleanBlock(render(tmpl.Description, vals))
	switch {
	case credit == "":
		text.Description = truncate(body, limits.Description, measure)
	case measure(credit) > limits.Description:
		return Text{}, fmt.Errorf("%w: %d of at most %d", ErrCreditTooLong, measure(credit), limits.Description)
	case body == "":
		text.Description = credit
	default:
		const sep = "\n\n"
		body = truncate(body, limits.Description-measure(sep+credit), measure)
		if body == "" {
			text.Description = credit
		} else {
			text.Description = body + sep + credit
		}
	}
	return text, nil
}

// FitTitle cleans a title to one line and cuts it to the target's limit.
func FitTitle(title string, limits Limits) string {
	return truncate(oneLine(title), limits.Title, utf8.RuneCountInString)
}

// placeholder matches a placeholder, as the configuration does.
var placeholder = config.PostPlaceholderPattern

// render replaces the placeholders of a pattern by their values. A name
// without a value becomes "", which configuration validation makes
// impossible except for {date} when the VOD date is unknown.
func render(pattern string, vals map[string]string) string {
	return placeholder.ReplaceAllStringFunc(pattern, func(m string) string {
		return vals[m[1:len(m)-1]]
	})
}

// oneLine makes untrusted text a single clean line: invalid UTF-8 replaced,
// control characters and line breaks turned into spaces, runs of spaces
// collapsed, ends trimmed.
func oneLine(s string) string {
	s = strings.ToValidUTF8(s, "�")
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
}

var blankLines = regexp.MustCompile(`\n{3,}`)

// cleanBlock cleans text that may span lines: every line is cleaned like
// oneLine, runs of blank lines become one, and the ends are trimmed.
func cleanBlock(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = oneLine(l)
	}
	return strings.TrimSpace(blankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// cleanTag keeps the letters, digits and underscores of a tag.
func cleanTag(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			return r
		}
		return -1
	}, strings.ToValidUTF8(s, ""))
}

// fitTags drops tags from the end until all of them, with a separator
// between two, are at most limit characters.
func fitTags(tags []string, limit int) []string {
	total := 0
	for i, t := range tags {
		n := utf8.RuneCountInString(t)
		if i > 0 {
			n++
		}
		if total+n > limit {
			return tags[:i]
		}
		total += n
	}
	return tags
}

const ellipsis = "…"

// truncate cuts s to at most limit, as measure counts, at a word boundary
// where there is one, and ends it with an ellipsis when anything was cut.
// The cut is always on a rune boundary.
func truncate(s string, limit int, measure func(string) int) string {
	if measure(s) <= limit {
		return s
	}
	budget := limit - measure(ellipsis)
	if budget <= 0 {
		return ""
	}
	// cut is the end of the longest prefix within the budget.
	cut, n := 0, 0
	for i, r := range s {
		w := measure(string(r))
		if n+w > budget {
			break
		}
		n += w
		cut = i + utf8.RuneLen(r)
	}
	prefix := s[:cut]
	next, _ := utf8.DecodeRuneInString(s[cut:])
	if !unicode.IsSpace(next) {
		// Mid-word: go back to the last space, unless the cut word is the
		// only one.
		if j := strings.LastIndexFunc(prefix, unicode.IsSpace); j > 0 {
			prefix = prefix[:j]
		}
	}
	prefix = strings.TrimRightFunc(prefix, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(".,;:-–—", r)
	})
	if prefix == "" {
		return ""
	}
	return prefix + ellipsis
}

// NewComposer returns the composer of posts: for a clip and a target it
// loads the clip's streamer and VOD, builds the text from the streamer's
// template in cfg and the clip's transcript, and fits it to the target's
// limits. A title override stored on the clip replaces the generated
// title, and is cut to the target's limit too.
func NewComposer(st *store.Store, cfg config.Post) Composer {
	return func(ctx context.Context, clip store.Clip, target string) (string, string, []string, error) {
		src, override, err := loadSource(ctx, st.Reader(), clip.ID)
		if err != nil {
			return "", "", nil, err
		}
		var transcript api.Transcript
		// A transcript that does not decode counts as no speech: the post
		// is still made, with the streamer's name as the hook.
		_ = json.Unmarshal([]byte(clip.TranscriptJSON), &transcript)
		limits := LimitsFor(target)
		text, err := Generate(cfg.For(src.Platform, src.Login), src, transcript, limits)
		if err != nil {
			return "", "", nil, fmt.Errorf("upload: compose clip %d for %s: %w", clip.ID, target, err)
		}
		if t := FitTitle(override, limits); t != "" {
			text.Title = t
		}
		return text.Title, text.Description, text.Tags, nil
	}
}

// ReviewTitle returns a function giving the title a clip will be posted
// with on target. It is meant for review.Sender.PostTitle, so the review
// message shows the title.
func ReviewTitle(st *store.Store, compose Composer, target string) func(ctx context.Context, clipID int64) (string, error) {
	return func(ctx context.Context, clipID int64) (string, error) {
		clip, err := store.GetClip(ctx, st.Reader(), clipID)
		if err != nil {
			return "", err
		}
		title, _, _, err := compose(ctx, clip, target)
		return title, err
	}
}

// loadSource reads the streamer and VOD of a clip, and its title override.
func loadSource(ctx context.Context, q store.Querier, clipID int64) (Source, string, error) {
	var (
		src       Source
		startedAt string
		override  sql.NullString
	)
	err := q.QueryRowContext(ctx, `
		SELECT s.platform, s.login, s.display_name, v.title, v.started_at, c.title_override
		FROM clips c
		JOIN jobs j ON j.id = c.job_id
		JOIN candidates cand ON cand.id = j.candidate_id
		JOIN vods v ON v.id = cand.vod_id
		JOIN streamers s ON s.id = v.streamer_id
		WHERE c.id = ?`, clipID).Scan(
		&src.Platform, &src.Login, &src.DisplayName, &src.VODTitle, &startedAt, &override)
	if errors.Is(err, sql.ErrNoRows) {
		return src, "", fmt.Errorf("upload: clip %d: %w", clipID, store.ErrNotFound)
	}
	if err != nil {
		return src, "", fmt.Errorf("upload: load clip %d: %w", clipID, err)
	}
	if src.VODStartedAt, err = store.ParseTime(startedAt); err != nil {
		return src, "", fmt.Errorf("upload: load clip %d: %w", clipID, err)
	}
	return src, override.String, nil
}

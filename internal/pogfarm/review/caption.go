package review

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/salehjafarli/pogfarm/internal/api"
)

// Callback actions of the review message. The callback data of a button is
// the action, a colon and the clip id, which is the bot's "action:payload"
// convention: "approve:42".
const (
	ActionApprove = "approve"
	ActionReject  = "reject"
)

// CallbackData returns the callback data of a review button. The longest
// possible value, an action and the largest int64, is 27 bytes, well within
// MaxCallbackDataBytes.
func CallbackData(action string, clipID int64) string {
	return action + ":" + strconv.FormatInt(clipID, 10)
}

// ParseClipID reads the clip id out of the payload of a review callback
// ("42" from "approve:42").
func ParseClipID(payload string) (int64, error) {
	id, err := strconv.ParseInt(payload, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("review: callback payload %q is not a clip id", payload)
	}
	return id, nil
}

// ReviewKeyboard returns the Approve and Reject buttons for a clip.
func ReviewKeyboard(clipID int64) *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{
		{Text: "Approve", CallbackData: CallbackData(ActionApprove, clipID)},
		{Text: "Reject", CallbackData: CallbackData(ActionReject, clipID)},
	}}}
}

// rejectKeyboard returns the Reject button alone, for a clip whose file is
// gone and so cannot be posted.
func rejectKeyboard(clipID int64) *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{
		{Text: "Reject", CallbackData: CallbackData(ActionReject, clipID)},
	}}}
}

// clipInfo is what the review message says about a clip.
type clipInfo struct {
	ClipID          int64
	Streamer        string
	VODTitle        string
	OffsetSeconds   float64
	DurationSeconds float64
	TranscriptJSON  string
	width, height   int
}

// maxTitleLength caps the VOD title in a caption so a long title cannot
// crowd out the transcript.
const maxTitleLength = 200

// header is the part of the review message before the transcript.
func (c clipInfo) header() string {
	streamer := clean(c.Streamer)
	if streamer == "" {
		streamer = "unknown streamer"
	}
	title := truncateText(clean(c.VODTitle), maxTitleLength)
	if title == "" {
		title = "(untitled VOD)"
	}
	return fmt.Sprintf("Clip #%d · %s\n%s\nAt %s in the VOD · %s long",
		c.ClipID, streamer, title, formatSeconds(c.OffsetSeconds), formatSeconds(c.DurationSeconds))
}

// caption is the caption of the review video: the header, a blank line and
// the transcript, cut to limit characters as Telegram counts them.
func (c clipInfo) caption(limit int) string {
	return truncateText(c.header()+"\n\n"+transcriptText(c.TranscriptJSON), limit)
}

// transcriptText returns the words of a stored transcript as one line of
// text.
func transcriptText(raw string) string {
	var t api.Transcript
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return "(transcript unreadable)"
	}
	words := make([]string, 0, len(t.Words))
	for _, w := range t.Words {
		if s := strings.TrimSpace(clean(w.Text)); s != "" {
			words = append(words, s)
		}
	}
	if len(words) == 0 {
		return "(no speech)"
	}
	return "“" + strings.Join(words, " ") + "”"
}

// clean makes untrusted text safe to put in a plain text message: invalid
// UTF-8 is replaced and control characters, line breaks included, become
// spaces, so the text cannot change the layout around it. Captions are sent
// without a parse mode, so nothing else in the text has any meaning to
// Telegram and no escaping is needed.
func clean(s string) string {
	s = strings.ToValidUTF8(s, "�")
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, s))
}

// textLength is the length of s as Telegram counts it: in UTF-16 code
// units, so a character outside the Basic Multilingual Plane (most emoji)
// counts twice. It is never less than the number of runes, so a text within
// the limit by this count is within it however Telegram counts.
func textLength(s string) int {
	n := 0
	for _, r := range s {
		n += utf16Width(r)
	}
	return n
}

func utf16Width(r rune) int {
	if r > 0xFFFF {
		return 2
	}
	return 1
}

// truncateText cuts s to at most limit characters (as textLength counts
// them), on a rune boundary, ending it with an ellipsis when anything was
// cut.
func truncateText(s string, limit int) string {
	if textLength(s) <= limit {
		return s
	}
	const ellipsis = "…"
	budget := limit - textLength(ellipsis)
	if budget < 0 {
		return ""
	}
	n := 0
	for i, r := range s {
		w := utf16Width(r)
		if n+w > budget {
			return strings.TrimRightFunc(s[:i], unicode.IsSpace) + ellipsis
		}
		n += w
	}
	return s
}

// formatSeconds writes seconds as m:ss, or h:mm:ss from an hour up.
func formatSeconds(seconds float64) string {
	if seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		seconds = 0
	}
	total := int64(seconds)
	h, m, s := total/3600, total/60%60, total%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// formatMB writes a size in megabytes (10^6 bytes) with one decimal.
func formatMB(n int64) string {
	return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + " MB"
}

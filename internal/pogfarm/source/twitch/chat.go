package twitch

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
	"github.com/salehjafarli/pogfarm/internal/runner"
)

// DefaultChatTimeout bounds one chat download when ChatFetcher.Timeout is
// zero. The chat of a long, busy stream takes minutes to download, so the
// limit is generous: it is there to stop a hung download, not a slow one.
const DefaultChatTimeout = 30 * time.Minute

// chatVODGoneMarker is what TwitchDownloaderCLI prints when Twitch no longer
// has the VOD. The tool has no exit code for it, only this message.
const chatVODGoneMarker = "deleted/expired VOD"

// chatCancelCheckEvery is how many comments are parsed between two looks at
// the context.
const chatCancelCheckEvery = 1024

// ChatFetcher fetches the chat of a Twitch VOD by running TwitchDownloaderCLI
// and converting its JSON output to the common chat format. It holds no
// state and is safe for concurrent use.
type ChatFetcher struct {
	// Binary is the TwitchDownloaderCLI program: a path, or a name looked
	// up in PATH.
	Binary string
	// Dir is the directory the download is written to before it is parsed,
	// normally pogfarm's data directory. It is created if missing. Each
	// call works in its own subdirectory, which is removed before the call
	// returns.
	Dir string
	// Timeout kills a download that runs longer. Zero means
	// DefaultChatTimeout.
	Timeout time.Duration
}

// Chat downloads the whole chat of the VOD and returns it ordered by offset.
// Only vod.ID is used.
//
// It stops early when ctx is cancelled. A failed download returns an error
// wrapping the *runner.Error of the run, so errors.Is sees
// context.DeadlineExceeded for a timeout and context.Canceled for a
// cancellation. When the tool reports that Twitch no longer has the VOD the
// error also wraps source.ErrVODNotFound.
func (f *ChatFetcher) Chat(ctx context.Context, vod source.VOD) ([]source.ChatMessage, error) {
	if !chatValidVODID(vod.ID) {
		return nil, fmt.Errorf("twitch: fetch chat: vod id %q is not a number", vod.ID)
	}
	fail := func(err error) ([]source.ChatMessage, error) {
		return nil, fmt.Errorf("twitch: fetch chat of vod %s: %w", vod.ID, err)
	}
	if f.Binary == "" {
		return fail(errors.New("no TwitchDownloaderCLI binary configured"))
	}
	if f.Dir == "" {
		return fail(errors.New("no download directory configured"))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}

	if err := os.MkdirAll(f.Dir, 0o755); err != nil {
		return fail(err)
	}
	// A directory of its own, not just a file: the output path must not
	// exist when the tool starts, and whatever the tool leaves next to its
	// output goes away with the directory.
	work, err := os.MkdirTemp(f.Dir, "chat-"+vod.ID+"-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(work)
	out := filepath.Join(work, "chat.json")

	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultChatTimeout
	}
	res, err := runner.Run(ctx, runner.Command{
		Name: f.Binary,
		// The .json extension selects the JSON format. Images are not
		// embedded unless --embed-images is given, so it is left out.
		Args: []string{
			"chatdownload",
			"--id", vod.ID,
			"--output", out,
			"--collision", "Overwrite",
			"--banner=false",
		},
		Timeout: timeout,
	})
	if err != nil {
		return fail(f.chatRunError(ctx, res, err, timeout))
	}

	file, err := os.Open(out)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fail(fmt.Errorf("%s exited without error but wrote no chat file%s",
				f.Binary, chatOutputSuffix(res.Stdout)))
		}
		return fail(err)
	}
	defer file.Close()

	msgs, err := parseChat(ctx, file)
	if err != nil {
		return fail(err)
	}
	return msgs, nil
}

// chatRunError turns a failed run into the error Chat returns, adding what
// the runner's own error cannot know.
func (f *ChatFetcher) chatRunError(ctx context.Context, res runner.Result, err error, timeout time.Duration) error {
	var rerr *runner.Error
	if !errors.As(err, &rerr) {
		return err
	}
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("TwitchDownloaderCLI binary %q not found: %w", f.Binary, err)
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		// The caller's context is still alive, so it was our own limit.
		return fmt.Errorf("download did not finish within %s: %w", timeout, err)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return err
	}
	// TwitchDownloaderCLI logs its own errors to stdout; only an unhandled
	// exception reaches stderr. Show stdout when stderr says nothing.
	suffix := ""
	if rerr.Stderr == "" {
		suffix = chatOutputSuffix(res.Stdout)
	}
	if strings.Contains(rerr.Stderr, chatVODGoneMarker) || bytes.Contains(res.Stdout, []byte(chatVODGoneMarker)) {
		return fmt.Errorf("%w: %w%s", source.ErrVODNotFound, err, suffix)
	}
	if suffix != "" {
		return fmt.Errorf("%w%s", err, suffix)
	}
	return err
}

// chatValidVODID reports whether id looks like a Twitch VOD ID: digits only.
// Anything else is refused before it can reach the tool's command line.
func chatValidVODID(id string) bool {
	if id == "" || len(id) > 32 {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// chatOutputSuffix formats the last lines of the tool's stdout for an error
// message. It returns "" when there is nothing to show. The tool redraws its
// progress line with "\r", so that ends a line too.
func chatOutputSuffix(stdout []byte) string {
	const maxLines, maxBytes = 10, 2 << 10
	if len(stdout) > maxBytes {
		stdout = stdout[len(stdout)-maxBytes:]
	}
	var lines []string
	for _, l := range strings.FieldsFunc(string(stdout), func(r rune) bool { return r == '\n' || r == '\r' }) {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return "; stdout tail:\n" + strings.Join(lines, "\n")
}

// chatComment is the part of one entry of the "comments" array that is kept.
// The field names are those of TwitchDownloader's chat JSON.
type chatComment struct {
	OffsetSeconds float64 `json:"content_offset_seconds"`
	Commenter     *struct {
		DisplayName string `json:"display_name"`
		Name        string `json:"name"`
	} `json:"commenter"`
	Message *struct {
		Body      string `json:"body"`
		Fragments []struct {
			Text     string `json:"text"`
			Emoticon *struct {
				ID string `json:"emoticon_id"`
			} `json:"emoticon"`
		} `json:"fragments"`
	} `json:"message"`
}

// message converts the comment to the common format.
func (c *chatComment) message() source.ChatMessage {
	m := source.ChatMessage{
		// Whole microseconds: the float cannot hold nanoseconds exactly.
		Offset: time.Duration(math.Round(c.OffsetSeconds*1e6)) * time.Microsecond,
	}
	if c.Commenter != nil {
		// The commenter is null for an account that no longer exists.
		m.User = c.Commenter.DisplayName
		if m.User == "" {
			m.User = c.Commenter.Name
		}
	}
	if c.Message == nil {
		return m
	}
	m.Text = c.Message.Body
	for _, frag := range c.Message.Fragments {
		// A fragment with an emoticon is one emote, and its text is the
		// emote's name.
		if frag.Emoticon == nil {
			continue
		}
		name := strings.TrimSpace(frag.Text)
		if name != "" && !slices.Contains(m.Emotes, name) {
			m.Emotes = append(m.Emotes, name)
		}
	}
	return m
}

// parseChat reads a TwitchDownloader chat JSON document and returns its
// comments in the common format, ordered by offset. Comments with the same
// offset keep their order in the file.
//
// The document is read token by token and one comment at a time, so memory
// is used for the returned messages and not for the file: everything outside
// the "comments" array is skipped without being stored.
func parseChat(ctx context.Context, r io.Reader) ([]source.ChatMessage, error) {
	msgs, err := parseChatDocument(ctx, r)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			return nil, err
		}
		return nil, fmt.Errorf("parse chat JSON: %w", err)
	}
	slices.SortStableFunc(msgs, func(a, b source.ChatMessage) int {
		return cmp.Compare(a.Offset, b.Offset)
	})
	return msgs, nil
}

func parseChatDocument(ctx context.Context, r io.Reader) ([]source.ChatMessage, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	// Skip a UTF-8 byte order mark, which encoding/json does not accept.
	if bom, err := br.Peek(3); err == nil && bytes.Equal(bom, []byte{0xEF, 0xBB, 0xBF}) {
		br.Discard(3)
	}
	dec := json.NewDecoder(br)

	tok, err := dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the file is empty")
		}
		return nil, err
	}
	if tok != json.Delim('{') {
		return nil, fmt.Errorf("want an object at the top level, got %v", tok)
	}

	var msgs []source.ChatMessage
	sawComments := false
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, chatUnexpectedEOF(err)
		}
		if key, _ := tok.(string); key != "comments" {
			if err := chatSkipValue(dec); err != nil {
				return nil, chatUnexpectedEOF(err)
			}
			continue
		}
		sawComments = true
		if msgs, err = parseChatComments(ctx, dec, msgs); err != nil {
			return nil, chatUnexpectedEOF(err)
		}
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, chatUnexpectedEOF(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("unexpected data after the top-level object")
		}
		return nil, err
	}
	if !sawComments {
		return nil, errors.New(`no "comments" array: not a chat file`)
	}
	return msgs, nil
}

// parseChatComments reads the value of the "comments" key, an array of
// comments or null, and appends its messages to msgs.
func parseChatComments(ctx context.Context, dec *json.Decoder, msgs []source.ChatMessage) ([]source.ChatMessage, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return msgs, nil
	}
	if tok != json.Delim('[') {
		return nil, fmt.Errorf(`"comments" must be an array, got %v`, tok)
	}
	for n := 0; dec.More(); n++ {
		if n%chatCancelCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		var c chatComment
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("comment %d: %w", n+1, chatUnexpectedEOF(err))
		}
		msgs = append(msgs, c.message())
	}
	if _, err := dec.Token(); err != nil { // the closing bracket
		return nil, err
	}
	return msgs, nil
}

// chatSkipValue reads past one JSON value of any shape without keeping it.
func chatSkipValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

// chatUnexpectedEOF makes the end of a cut-off file read as what it is. The
// decoder reports it as a plain io.EOF when it falls between two tokens.
func chatUnexpectedEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

package twitch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
	"github.com/salehjafarli/pogfarm/internal/runner"
)

// chatFixtureMessages is what testdata/chat.json converts to.
var chatFixtureMessages = []source.ChatMessage{
	{Offset: 1500 * time.Millisecond, User: "ViewerOne", Text: "hello chat"},
	{
		Offset: 12250 * time.Millisecond, User: "ViewerTwo",
		Text:   "that was insane PogChamp PogChamp LUL",
		Emotes: []string{"PogChamp", "LUL"},
	},
	{Offset: 65 * time.Second, User: "", Text: "message from a deleted account"},
	{Offset: 3599999 * time.Millisecond, User: "viewerfour", Text: "gg \"çox sağ ol\" 👋 Cheer100"},
}

// chatDoc wraps a comments array in the smallest valid chat document.
func chatDoc(comments string) string {
	return `{"FileInfo":{"Version":{"Major":1,"Minor":4,"Patch":0}},"video":{"id":"1"},"comments":` +
		comments + `,"embeddedData":null}`
}

// chatCommentJSON renders one comment the way the tool writes it, with the
// fields the parser ignores left in.
func chatCommentJSON(offset float64, user, body string) string {
	return fmt.Sprintf(`{"_id":"id-%s","created_at":"2026-11-16T18:00:00Z","channel_id":"1","content_type":"video",`+
		`"content_id":"1","content_offset_seconds":%g,"commenter":{"display_name":%q,"_id":"2","name":%q,"bio":null,`+
		`"logo":"https://example.invalid/logo.png"},"message":{"body":%q,"bits_spent":0,`+
		`"fragments":[{"text":%q,"emoticon":null}],"user_badges":[],"user_color":null,"emoticons":[]}}`,
		user, offset, user, strings.ToLower(user), body, body)
}

func chatParseString(t *testing.T, doc string) ([]source.ChatMessage, error) {
	t.Helper()
	return parseChat(context.Background(), strings.NewReader(doc))
}

func TestParseChatFixture(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "chat.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got, err := parseChat(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, chatFixtureMessages) {
		t.Errorf("messages:\n got %+v\nwant %+v", got, chatFixtureMessages)
	}
}

func TestParseChatEmpty(t *testing.T) {
	for _, comments := range []string{"[]", "null"} {
		got, err := chatParseString(t, chatDoc(comments))
		if err != nil {
			t.Errorf("comments %s: %v", comments, err)
		}
		if len(got) != 0 {
			t.Errorf("comments %s: got %d messages, want none", comments, len(got))
		}
	}
}

func TestParseChatOrdersByOffset(t *testing.T) {
	doc := chatDoc("[" + strings.Join([]string{
		chatCommentJSON(30, "C", "third"),
		chatCommentJSON(10, "A", "first"),
		chatCommentJSON(20, "B1", "second, written first"),
		chatCommentJSON(20, "B2", "second, written last"),
		chatCommentJSON(0, "Z", "at the very start"),
	}, ",") + "]")

	got, err := chatParseString(t, doc)
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	for _, m := range got {
		users = append(users, m.User)
	}
	// Equal offsets keep the order they had in the file.
	if want := []string{"Z", "A", "B1", "B2", "C"}; !slices.Equal(users, want) {
		t.Errorf("order: got %v, want %v", users, want)
	}
}

func TestParseChatEmotes(t *testing.T) {
	doc := chatDoc(`[
		{"content_offset_seconds":1,"commenter":{"display_name":"A","name":"a"},"message":{"body":"Kappa hi Kappa  LUL","fragments":[
			{"text":"Kappa","emoticon":{"emoticon_id":"25"}},
			{"text":" hi ","emoticon":null},
			{"text":"Kappa","emoticon":{"emoticon_id":"25"}},
			{"text":"  "},
			{"text":" LUL ","emoticon":{"emoticon_id":"425618"}},
			{"text":"","emoticon":{"emoticon_id":"1"}}
		]}},
		{"content_offset_seconds":2,"commenter":{"display_name":"B","name":"b"},"message":{"body":"no emotes","fragments":[{"text":"no emotes","emoticon":null}]}},
		{"content_offset_seconds":3,"commenter":{"display_name":"C","name":"c"},"message":{"body":"no fragments","fragments":null}},
		{"content_offset_seconds":4,"commenter":{"display_name":"D","name":"d"},"message":null}
	]`)
	got, err := chatParseString(t, doc)
	if err != nil {
		t.Fatal(err)
	}
	want := []source.ChatMessage{
		{Offset: 1 * time.Second, User: "A", Text: "Kappa hi Kappa  LUL", Emotes: []string{"Kappa", "LUL"}},
		{Offset: 2 * time.Second, User: "B", Text: "no emotes"},
		{Offset: 3 * time.Second, User: "C", Text: "no fragments"},
		{Offset: 4 * time.Second, User: "D"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("messages:\n got %+v\nwant %+v", got, want)
	}
	if got[1].Emotes != nil {
		t.Errorf("Emotes of a message without emotes: got %v, want nil", got[1].Emotes)
	}
}

func TestParseChatSkipsEverythingElse(t *testing.T) {
	// Sections before and after the comments, nested and of every JSON
	// type, behind a byte order mark.
	doc := "\xEF\xBB\xBF" + `{
		"FileInfo": {"Version": {"Major": 1, "Minor": 4, "Patch": 0}, "CreatedAt": "x"},
		"streamer": {"name": "s", "comments": [{"content_offset_seconds": 99}]},
		"clipper": null,
		"video": {"chapters": [{"a": [1, 2, {"b": [true, false, null]}]}], "length": 1.5e3},
		"comments": [` + chatCommentJSON(7, "Only", "one") + `],
		"embeddedData": {"firstParty": [{"data": "AAAA", "name": "x"}], "thirdParty": [], "twitchBadges": [[], {}]},
		"unknownFutureKey": "value"
	}` + "\n"
	got, err := chatParseString(t, doc)
	if err != nil {
		t.Fatal(err)
	}
	want := []source.ChatMessage{{Offset: 7 * time.Second, User: "Only", Text: "one"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("messages: got %+v, want %+v", got, want)
	}
}

func TestParseChatMalformed(t *testing.T) {
	whole := chatDoc("[" + chatCommentJSON(1, "A", "one") + "," + chatCommentJSON(2, "B", "two") + "]")
	cases := []struct {
		name, doc, want string
	}{
		{"empty file", "", "the file is empty"},
		{"not JSON", "this is not json", "invalid character"},
		{"array at top level", "[]", "want an object at the top level"},
		// "unexpected " matches both "unexpected EOF" and encoding/json's
		// "unexpected end of JSON input".
		{"cut off inside a comment", whole[:len(whole)-60], "unexpected "},
		{"cut off between comments", whole[:strings.Index(whole, `,{"_id":"id-B"`)+1], "unexpected "},
		{"cut off before the closing brace", strings.TrimSuffix(whole, "}"), "unexpected "},
		{"cut off after a key", `{"comments":`, "unexpected "},
		{"comments is an object", chatDoc(`{"a":1}`), `"comments" must be an array`},
		{"comments is a string", chatDoc(`"nope"`), `"comments" must be an array`},
		{"comment is not an object", chatDoc(`[1]`), "comment 1:"},
		{"offset is not a number", chatDoc(`[` + chatCommentJSON(1, "A", "one") + `,{"content_offset_seconds":"soon"}]`), "comment 2:"},
		{"no comments key", `{"FileInfo":{},"video":{}}`, `no "comments" array`},
		{"data after the document", whole + `{"more":1}`, "unexpected data after the top-level object"},
		{"garbage after the document", whole + "!!", "invalid character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chatParseString(t, tc.doc)
			if err == nil {
				t.Fatalf("no error, got %d messages", len(got))
			}
			if got != nil {
				t.Errorf("got %d messages together with an error", len(got))
			}
			if !strings.Contains(err.Error(), "parse chat JSON") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// chatGenReader produces a chat document of total comments without ever
// holding more than one comment. Offsets are scrambled so the output is not
// already in order. onComment, if set, is called before comment i is produced.
type chatGenReader struct {
	total     int
	next      int
	buf       []byte
	done      bool
	served    int64
	onComment func(i int)
}

// chatGenOffset gives comment i a distinct offset in seconds in [0, total).
// 7919 is prime, so the mapping is a permutation whenever total is not a
// multiple of it.
func chatGenOffset(i, total int) int { return (i * 7919) % total }

func (g *chatGenReader) Read(p []byte) (int, error) {
	for len(g.buf) == 0 {
		switch {
		case g.done:
			return 0, io.EOF
		case g.next == 0 && g.served == 0:
			g.buf = append(g.buf, `{"FileInfo":{"Version":{"Major":1,"Minor":4,"Patch":0}},"video":{"id":"1"},"comments":[`...)
		case g.next < g.total:
			if g.onComment != nil {
				g.onComment(g.next)
			}
			if g.next > 0 {
				g.buf = append(g.buf, ',')
			}
			off := chatGenOffset(g.next, g.total)
			g.buf = append(g.buf, chatCommentJSON(float64(off), fmt.Sprintf("user%d", off), fmt.Sprintf("message %d", off))...)
			g.next++
		default:
			g.buf = append(g.buf, `],"embeddedData":null}`...)
			g.done = true
		}
	}
	n := copy(p, g.buf)
	g.buf = g.buf[n:]
	g.served += int64(n)
	return n, nil
}

// chatCheckGenerated checks the result of parsing a chatGenReader document.
func chatCheckGenerated(t *testing.T, got []source.ChatMessage, total int) {
	t.Helper()
	if len(got) != total {
		t.Fatalf("got %d messages, want %d", len(got), total)
	}
	for i, m := range got {
		if m.Offset != time.Duration(i)*time.Second || m.User != fmt.Sprintf("user%d", i) || m.Text != fmt.Sprintf("message %d", i) {
			t.Fatalf("message %d: got %+v", i, m)
		}
	}
}

// The parser must work through the input as it goes, not read it whole
// first: when the context ends mid-way, it stops having consumed only a
// little more than it had parsed.
func TestParseChatIsStreamingAndCancellable(t *testing.T) {
	const total, cancelAt = 1_000_000, 5_000
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gen := &chatGenReader{total: total, onComment: func(i int) {
		if i == cancelAt {
			cancel()
		}
	}}

	got, err := parseChat(ctx, gen)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error: got %v, want context.Canceled", err)
	}
	if got != nil {
		t.Errorf("got %d messages together with an error", len(got))
	}
	// Slack: the interval between context checks plus what the read
	// buffers hold ahead of the parser.
	if limit := cancelAt + chatCancelCheckEvery + 2_000; gen.next > limit {
		t.Errorf("parser consumed %d of %d comments before stopping, want at most %d", gen.next, total, limit)
	}
}

// chatFakeTool writes a shell script standing in for TwitchDownloaderCLI and
// returns its path and the file it records its arguments in, one per line.
// Before running body the script puts the value of --output in $out.
func chatFakeTool(t *testing.T, body string) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "TwitchDownloaderCLI")
	argsFile = filepath.Join(dir, "args")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > '" + argsFile + "'\n" +
		"out=\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = \"--output\" ]; then out=\"$2\"; fi\n" +
		"  shift\n" +
		"done\n" +
		body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile
}

// chatWriteFile writes content to a new file outside the fetcher's directory
// and returns its path.
func chatWriteFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recorded.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// chatFetcher returns a fetcher running bin, and registers a check that the
// fetcher's directory is empty when the test ends: whatever happened, no
// temporary file may be left behind.
func chatFetcher(t *testing.T, bin string) *ChatFetcher {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	t.Cleanup(func() { chatCheckNoLeftovers(t, dir) })
	return &ChatFetcher{Binary: bin, Dir: dir}
}

func chatCheckNoLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("left behind in %s: %s", dir, e.Name())
	}
}

var chatTestVOD = source.VOD{Platform: "twitch", ID: "2000000001"}

func TestChatReturnsMessages(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", "chat.json"))
	if err != nil {
		t.Fatal(err)
	}
	bin, argsFile := chatFakeTool(t, `cp '`+fixture+`' "$out"`)
	f := chatFetcher(t, bin)

	got, err := f.Chat(context.Background(), chatTestVOD)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, chatFixtureMessages) {
		t.Errorf("messages:\n got %+v\nwant %+v", got, chatFixtureMessages)
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(args) != 8 {
		t.Fatalf("arguments: got %q", args)
	}
	out := args[4]
	want := []string{"chatdownload", "--id", "2000000001", "--output", out, "--collision", "Overwrite", "--banner=false"}
	if !slices.Equal(args, want) {
		t.Errorf("arguments:\n got %q\nwant %q", args, want)
	}
	// The output goes to a .json file inside the fetcher's directory.
	rel, err := filepath.Rel(f.Dir, out)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.Ext(out) != ".json" || !filepath.IsAbs(out) {
		t.Errorf("output path %q is not a .json file under %q", out, f.Dir)
	}
	chatCheckNoLeftovers(t, f.Dir)
}

func TestChatEmpty(t *testing.T) {
	bin, _ := chatFakeTool(t, `cp '`+chatWriteFile(t, chatDoc("[]"))+`' "$out"`)
	got, err := chatFetcher(t, bin).Chat(context.Background(), chatTestVOD)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d messages, want none", len(got))
	}
}

func TestChatOrdersByOffset(t *testing.T) {
	doc := chatDoc("[" + chatCommentJSON(50, "Late", "b") + "," + chatCommentJSON(5, "Early", "a") + "]")
	bin, _ := chatFakeTool(t, `cp '`+chatWriteFile(t, doc)+`' "$out"`)
	got, err := chatFetcher(t, bin).Chat(context.Background(), chatTestVOD)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].User != "Early" || got[1].User != "Late" {
		t.Errorf("got %+v, want Early before Late", got)
	}
}

func TestChatLargeFile(t *testing.T) {
	total := 200_000
	if testing.Short() {
		total = 20_000
	}
	big := filepath.Join(t.TempDir(), "big.json")
	file, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	size, err := io.Copy(file, &chatGenReader{total: total})
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("generated chat file: %d comments, %d MiB", total, size>>20)

	bin, _ := chatFakeTool(t, `cp '`+big+`' "$out"`)
	got, err := chatFetcher(t, bin).Chat(context.Background(), chatTestVOD)
	if err != nil {
		t.Fatal(err)
	}
	chatCheckGenerated(t, got, total)
}

func TestChatMalformedOutput(t *testing.T) {
	doc := chatDoc("[" + chatCommentJSON(1, "A", "one") + "]")
	for name, content := range map[string]string{
		"cut off":  doc[:len(doc)-40],
		"not JSON": "<html>rate limited</html>",
		"empty":    "",
	} {
		t.Run(name, func(t *testing.T) {
			bin, _ := chatFakeTool(t, `cp '`+chatWriteFile(t, content)+`' "$out"`)
			got, err := chatFetcher(t, bin).Chat(context.Background(), chatTestVOD)
			if err == nil {
				t.Fatalf("no error, got %d messages", len(got))
			}
			for _, want := range []string{"fetch chat of vod 2000000001", "parse chat JSON"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestChatToolFails(t *testing.T) {
	// A partial output file is written before failing: it must go too.
	bin, _ := chatFakeTool(t, `echo '{"comments":[' > "$out"
echo 'first line of the log' >&2
echo 'Unhandled exception. System.Exception: something broke' >&2
exit 3`)
	got, err := chatFetcher(t, bin).Chat(context.Background(), chatTestVOD)
	if err == nil {
		t.Fatalf("no error, got %d messages", len(got))
	}
	var rerr *runner.Error
	if !errors.As(err, &rerr) {
		t.Fatalf("error %T is not a *runner.Error: %v", err, err)
	}
	if rerr.ExitCode != 3 {
		t.Errorf("exit code: got %d, want 3", rerr.ExitCode)
	}
	for _, want := range []string{"fetch chat of vod 2000000001", "exit code 3", "first line of the log", "System.Exception: something broke"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if errors.Is(err, source.ErrVODNotFound) {
		t.Errorf("error %q must not be ErrVODNotFound", err)
	}
}

// The tool logs its own errors to stdout. With nothing on stderr the error
// shows the end of stdout instead.
func TestChatToolFailsWithOnlyStdout(t *testing.T) {
	bin, _ := chatFakeTool(t, `printf '[STATUS] - Downloading 10%%\r[STATUS] - Downloading 20%%\r\n'
echo '[ERROR] - Unable to parse Vod/Clip ID/URL.'
exit 1`)
	_, err := chatFetcher(t, bin).Chat(context.Background(), chatTestVOD)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"exit code 1", "stdout tail:", "Downloading 20%\n[ERROR] - Unable to parse Vod/Clip ID/URL."} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestChatVODGone(t *testing.T) {
	for name, body := range map[string]string{
		"on stderr": `echo 'Unhandled exception. System.AggregateException: One or more errors occurred. (Invalid VOD, deleted/expired VOD possibly?)' >&2
exit 134`,
		"on stdout": `echo '[ERROR] - Invalid VOD, deleted/expired VOD possibly?'
exit 1`,
	} {
		t.Run(name, func(t *testing.T) {
			bin, _ := chatFakeTool(t, body)
			_, err := chatFetcher(t, bin).Chat(context.Background(), chatTestVOD)
			if !errors.Is(err, source.ErrVODNotFound) {
				t.Fatalf("error: got %v, want ErrVODNotFound", err)
			}
			var rerr *runner.Error
			if !errors.As(err, &rerr) {
				t.Errorf("error %T does not wrap a *runner.Error", err)
			}
			if !strings.Contains(err.Error(), "deleted/expired VOD possibly?") {
				t.Errorf("error %q does not carry the tool's message", err)
			}
		})
	}
}

func TestChatNoOutputFile(t *testing.T) {
	bin, _ := chatFakeTool(t, `echo 'nothing to do'`)
	_, err := chatFetcher(t, bin).Chat(context.Background(), chatTestVOD)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"wrote no chat file", "nothing to do"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestChatTimeout(t *testing.T) {
	// The tool has started writing when it hangs.
	bin, _ := chatFakeTool(t, `echo '{"comments":[' > "$out"
echo 'still downloading' >&2
sleep 60`)
	f := chatFetcher(t, bin)
	f.Timeout = 300 * time.Millisecond

	start := time.Now()
	got, err := f.Chat(context.Background(), chatTestVOD)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error: got %v, want context.DeadlineExceeded", err)
	}
	if got != nil {
		t.Errorf("got %d messages together with an error", len(got))
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("returned after %s, the tool was not killed", d)
	}
	for _, want := range []string{"fetch chat of vod 2000000001", "did not finish within 300ms", "timed out and was killed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	chatCheckNoLeftovers(t, f.Dir)
}

func TestChatCancelled(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	bin, _ := chatFakeTool(t, `echo '{"comments":[' > "$out"
: > '`+started+`'
sleep 60`)
	f := chatFetcher(t, bin)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Cancel once the tool is running, or give up after a while and
		// cancel anyway so the test fails instead of hanging.
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if _, err := os.Stat(started); err == nil {
				break
			}
		}
		cancel()
	}()

	start := time.Now()
	got, err := f.Chat(ctx, chatTestVOD)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error: got %v, want context.Canceled", err)
	}
	if got != nil {
		t.Errorf("got %d messages together with an error", len(got))
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("returned after %s, the tool was not killed", d)
	}
	if _, statErr := os.Stat(started); statErr != nil {
		t.Errorf("the tool never started: %v", statErr)
	}
	if !strings.Contains(err.Error(), "cancelled and was killed") {
		t.Errorf("error %q does not say the run was cancelled", err)
	}
	chatCheckNoLeftovers(t, f.Dir)
}

func TestChatCancelledBeforeStart(t *testing.T) {
	bin, argsFile := chatFakeTool(t, `exit 0`)
	f := chatFetcher(t, bin)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := f.Chat(ctx, chatTestVOD)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error: got %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(argsFile); statErr == nil {
		t.Error("the tool was run although the context was already cancelled")
	}
}

// A caller's own deadline is reported as such, not as the fetcher's timeout.
func TestChatCallerDeadline(t *testing.T) {
	bin, _ := chatFakeTool(t, `sleep 60`)
	f := chatFetcher(t, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, err := f.Chat(ctx, chatTestVOD)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error: got %v, want context.DeadlineExceeded", err)
	}
	if strings.Contains(err.Error(), "did not finish within") {
		t.Errorf("error %q blames the fetcher's timeout", err)
	}
}

func TestChatMissingBinary(t *testing.T) {
	cases := map[string]string{
		"path that does not exist": filepath.Join(t.TempDir(), "bin", "TwitchDownloaderCLI"),
		"name not in PATH":         "pogfarm-test-no-such-twitchdownloadercli",
	}
	for name, bin := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := chatFetcher(t, bin).Chat(context.Background(), chatTestVOD)
			if err == nil {
				t.Fatal("no error")
			}
			want := fmt.Sprintf("TwitchDownloaderCLI binary %q not found", bin)
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
			var rerr *runner.Error
			if !errors.As(err, &rerr) {
				t.Errorf("error %T does not wrap a *runner.Error", err)
			}
		})
	}
}

func TestChatRejectsBadInput(t *testing.T) {
	bin, argsFile := chatFakeTool(t, `exit 0`)
	for _, id := range []string{"", "--help", "12a", "12 34", "https://twitch.tv/videos/1", strings.Repeat("1", 33)} {
		f := chatFetcher(t, bin)
		_, err := f.Chat(context.Background(), source.VOD{ID: id})
		if err == nil || !strings.Contains(err.Error(), "is not a number") {
			t.Errorf("vod id %q: got error %v", id, err)
		}
	}

	_, err := (&ChatFetcher{Dir: t.TempDir()}).Chat(context.Background(), chatTestVOD)
	if err == nil || !strings.Contains(err.Error(), "no TwitchDownloaderCLI binary configured") {
		t.Errorf("empty Binary: got error %v", err)
	}
	_, err = (&ChatFetcher{Binary: bin}).Chat(context.Background(), chatTestVOD)
	if err == nil || !strings.Contains(err.Error(), "no download directory configured") {
		t.Errorf("empty Dir: got error %v", err)
	}
	if _, statErr := os.Stat(argsFile); statErr == nil {
		t.Error("the tool was run for input that should have been refused")
	}
}

// Concurrent fetches of the same VOD must not share a work directory.
func TestChatConcurrentCalls(t *testing.T) {
	doc := chatDoc("[" + chatCommentJSON(1, "A", "one") + "]")
	bin, _ := chatFakeTool(t, `sleep 0.2
cp '`+chatWriteFile(t, doc)+`' "$out"`)
	f := chatFetcher(t, bin)

	const calls = 4
	errs := make(chan error, calls)
	for range calls {
		go func() {
			got, err := f.Chat(context.Background(), chatTestVOD)
			if err == nil && len(got) != 1 {
				err = fmt.Errorf("got %d messages, want 1", len(got))
			}
			errs <- err
		}()
	}
	for range calls {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

func TestChatOutputSuffix(t *testing.T) {
	if got := chatOutputSuffix(nil); got != "" {
		t.Errorf("no output: got %q", got)
	}
	if got := chatOutputSuffix([]byte(" \r\n\r \n")); got != "" {
		t.Errorf("blank output: got %q", got)
	}
	var b bytes.Buffer
	for i := range 500 {
		fmt.Fprintf(&b, "line %d\r\n", i)
	}
	got := chatOutputSuffix(b.Bytes())
	if lines := strings.Split(strings.TrimPrefix(got, "; stdout tail:\n"), "\n"); len(lines) != 10 || lines[9] != "line 499" || lines[0] != "line 490" {
		t.Errorf("long output: got %q", got)
	}
}

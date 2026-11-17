package review

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/queue"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/status"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// statusFixture is the sender fixture with a bot that has the status
// commands registered. Updates are dispatched one at a time, without
// polling.
type statusFixture struct {
	*senderFixture
	bot *Bot
	// nextStart keeps the windows of extra candidates apart from the clips'.
	nextStart float64
}

func newStatusFixture(t *testing.T, opts StatusOptions) *statusFixture {
	t.Helper()
	f := &statusFixture{senderFixture: newSenderFixture(t), nextStart: 100_000}
	q, err := queue.New(f.store, queue.Options{Now: f.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := status.New(f.store, q, status.Options{Now: f.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	client := f.api.client()
	cmds, err := NewStatusCommands(reader, client, f.sender, opts)
	if err != nil {
		t.Fatal(err)
	}
	f.bot = NewBot(client, ownerChat, slog.New(slog.NewTextHandler(f.log, nil)))
	cmds.Register(f.bot)
	return f
}

func (f *statusFixture) dispatch(u Update) {
	f.bot.dispatch(context.Background(), &u)
}

// statusAddJob stores a job that holds no clip. A leased job's clipper last
// sent a heartbeat at heartbeat.
func (f *statusFixture) statusAddJob(st store.JobStatus, clipper string, heartbeat time.Time) int64 {
	f.t.Helper()
	ctx := context.Background()
	f.nextStart += 100
	cand, err := store.InsertCandidate(ctx, f.store.Writer(), store.Candidate{
		Platform: api.PlatformTwitch, VODID: f.vod.ID, StartSeconds: f.nextStart, EndSeconds: f.nextStart + 30,
		Score: 1, Signal: store.SignalChatSpike, Status: store.CandidateQueued,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	j := store.Job{Platform: api.PlatformTwitch, CandidateID: cand.ID, Status: st, Clipper: clipper,
		LastHeartbeatAt: heartbeat, CreatedAt: t0.Add(-2 * time.Hour)}
	if st == store.JobLeased {
		j.LeaseExpiresAt = heartbeat.Add(time.Minute)
	}
	job, err := store.InsertJob(ctx, f.store.Writer(), j)
	if err != nil {
		f.t.Fatal(err)
	}
	return job.ID
}

// statusDoneAt moves the completion time of every done job to at, so the
// done-today count does not depend on the wall clock the clips' jobs were
// stored with.
func (f *statusFixture) statusDoneAt(at time.Time) {
	f.t.Helper()
	if _, err := f.store.Writer().Exec(`UPDATE jobs SET updated_at = ? WHERE status = ?`,
		store.FormatTime(at), store.JobDone); err != nil {
		f.t.Fatal(err)
	}
}

func TestStatusCommand(t *testing.T) {
	f := newStatusFixture(t, StatusOptions{})
	f.addClip(clipSpec{offset: 10, messageID: 50})
	f.addClip(clipSpec{offset: 20})
	approved := f.addClip(clipSpec{offset: 30, status: store.ReviewApproved, messageID: 51})
	f.statusDoneAt(t0.Add(-time.Hour))
	if _, err := store.InsertUpload(context.Background(), f.store.Writer(), store.Upload{
		ClipID: approved, TargetPlatform: "fake"}); err != nil {
		t.Fatal(err)
	}
	f.statusAddJob(store.JobQueued, "", time.Time{})
	jobB := f.statusAddJob(store.JobLeased, "node-b", t0.Add(-30*time.Second))
	jobC := f.statusAddJob(store.JobLeased, "node-c", t0.Add(-time.Hour))

	// A stranger gets nothing.
	f.dispatch(textUpdate(1, strangerChat, "/status"))
	if calls := f.api.take(); len(calls) != 0 {
		t.Fatalf("a stranger's /status made %d calls", len(calls))
	}

	f.dispatch(textUpdate(2, ownerChat, "/status"))
	calls := f.api.take()
	if len(calls) != 1 || calls[0].method != "sendMessage" {
		t.Fatalf("calls = %+v, want one sendMessage", calls)
	}
	if got := calls[0].fields["chat_id"]; got != fmt.Sprint(ownerChat) {
		t.Errorf("chat_id = %s, want the owner's", got)
	}
	if _, ok := calls[0].fields["parse_mode"]; ok {
		t.Error("a parse mode was set")
	}
	want := fmt.Sprintf(`pogfarm · Nov 17 12:00 UTC

Jobs: 1 queued, 2 leased, 3 done today, 0 failed
VODs: 0 scanning, 1 new, 0 retrying, 0 failed
Review: 2 awaiting (1 not sent yet)
Uploads: 1 pending, 0 uploading, 0 failed

Clippers: 1 online, 1 offline
node-b: online, seen 30s, job %d
node-c: OFFLINE, seen 1h 0m, job %d
(offline: silent for over 4m)`, jobB, jobC)
	if got := calls[0].fields["text"]; got != want {
		t.Errorf("text =\n%s\nwant\n%s", got, want)
	}
}

func TestStatusCommandEmpty(t *testing.T) {
	f := newStatusFixture(t, StatusOptions{})
	f.dispatch(textUpdate(1, ownerChat, "/status"))
	calls := f.api.take()
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want one", calls)
	}
	if got := calls[0].fields["text"]; !strings.HasSuffix(got, "\n\nClippers: none seen yet") ||
		!strings.Contains(got, "Jobs: 0 queued, 0 leased, 0 done today, 0 failed") {
		t.Errorf("text =\n%s", got)
	}
}

// TestStatusCommandLength checks that many clippers with long names are cut
// to a list with a count of the rest, and that even with no cap the message
// stays within Telegram's limit.
func TestStatusCommandLength(t *testing.T) {
	for _, tc := range []struct {
		name     string
		clippers int
		more     string
	}{
		{"default cap", 0, "and 275 more"},
		{"no cap", 1000, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStatusFixture(t, StatusOptions{Clippers: tc.clippers})
			for i := range 300 {
				name := fmt.Sprintf("%03d-%s\nline", i, strings.Repeat("ñ", 200))
				f.statusAddJob(store.JobLeased, name, t0.Add(-time.Duration(i)*time.Second))
			}
			f.dispatch(textUpdate(1, ownerChat, "/status"))
			calls := f.api.take()
			if len(calls) != 1 {
				t.Fatalf("calls = %d, want one", len(calls))
			}
			text := calls[0].fields["text"]
			if n := textLength(text); n > MaxMessageLength {
				t.Errorf("message is %d characters, over %d", n, MaxMessageLength)
			}
			if strings.Contains(text, "\nline") {
				t.Error("a line break in a clipper name reached the message")
			}
			if tc.more != "" && !strings.Contains(text, "\n"+tc.more+"\n") {
				t.Errorf("text does not say %q:\n%s", tc.more, text)
			}
			if tc.more == "" && !strings.HasSuffix(text, "…") {
				t.Errorf("uncapped text was not cut: ends %q", text[len(text)-40:])
			}
		})
	}
}

func TestPendingCommand(t *testing.T) {
	f := newStatusFixture(t, StatusOptions{PendingLimit: 2})

	f.dispatch(textUpdate(1, ownerChat, "/pending"))
	calls := f.api.take()
	if len(calls) != 1 || calls[0].fields["text"] != "No clips awaiting review." {
		t.Fatalf("calls with no clips = %+v", calls)
	}
	if _, ok := calls[0].fields["reply_markup"]; ok {
		t.Error("an empty list has buttons")
	}

	first := f.addClip(clipSpec{offset: 10, messageID: 50}) // created t0-59m
	second := f.addClip(clipSpec{offset: 20})               // t0-58m
	f.addClip(clipSpec{offset: 30})                         // t0-57m
	f.addClip(clipSpec{offset: 40, status: store.ReviewRejected})

	f.dispatch(textUpdate(2, strangerChat, "/pending"))
	if calls := f.api.take(); len(calls) != 0 {
		t.Fatalf("a stranger's /pending made %d calls", len(calls))
	}

	f.dispatch(textUpdate(3, ownerChat, "/pending"))
	calls = f.api.take()
	if len(calls) != 1 || calls[0].method != "sendMessage" {
		t.Fatalf("calls = %+v, want one sendMessage", calls)
	}
	want := fmt.Sprintf(`3 awaiting review, oldest first:

#%d SomeStreamer, 59m ago
#%d SomeStreamer, 58m ago, not sent yet
and 1 more

Tap a button to send that clip's review message again.`, first, second)
	if got := calls[0].fields["text"]; got != want {
		t.Errorf("text =\n%s\nwant\n%s", got, want)
	}
	wantKeyboard := keyboardJSON([]InlineKeyboardButton{
		{Text: fmt.Sprintf("Resend #%d", first), CallbackData: fmt.Sprintf("resend:%d", first)},
		{Text: fmt.Sprintf("Resend #%d", second), CallbackData: fmt.Sprintf("resend:%d", second)},
	})
	if got := calls[0].fields["reply_markup"]; got != wantKeyboard {
		t.Errorf("reply_markup = %s, want %s", got, wantKeyboard)
	}
	if got := fmt.Sprintf("resend:%d", int64(1<<63-1)); len(got) > MaxCallbackDataBytes {
		t.Errorf("the longest resend callback data is %d bytes", len(got))
	}
}

func TestResendCallback(t *testing.T) {
	f := newStatusFixture(t, StatusOptions{})
	id := f.addClip(clipSpec{offset: 10, words: []string{"hi"}, content: []byte("video"), messageID: 50})
	other := f.addClip(clipSpec{offset: 20, content: []byte("video"), messageID: 51})
	decided := f.addClip(clipSpec{offset: 30, content: []byte("video"), status: store.ReviewApproved, messageID: 52})

	// A stranger's tap does nothing.
	f.dispatch(callbackUpdate(1, strangerChat, fmt.Sprintf("resend:%d", id)))
	if calls := f.api.take(); len(calls) != 0 {
		t.Fatalf("a stranger's tap made %d calls", len(calls))
	}

	f.dispatch(callbackUpdate(2, ownerChat, fmt.Sprintf("resend:%d", id)))
	calls := f.api.take()
	if len(calls) != 2 || calls[0].method != "sendVideo" || calls[1].method != "answerCallbackQuery" {
		t.Fatalf("calls = %+v, want sendVideo then answerCallbackQuery", calls)
	}
	if got := calls[0].fields["reply_markup"]; got != keyboardJSON(reviewButtons(id)) {
		t.Errorf("resent message has buttons %s", got)
	}
	if got := calls[0].fields["caption"]; !strings.HasPrefix(got, fmt.Sprintf("Clip #%d · SomeStreamer", id)) {
		t.Errorf("caption = %q", got)
	}
	if got := calls[1].fields["text"]; got != "Sent again." {
		t.Errorf("callback answer = %q", got)
	}
	c := f.clip(id)
	if c.TelegramChatID != ownerChat || c.TelegramMessageID == 50 || c.TelegramMessageID == 0 {
		t.Errorf("clip records message %d/%d, want the new one in the owner's chat", c.TelegramChatID, c.TelegramMessageID)
	}
	if o := f.clip(other); o.TelegramMessageID != 51 {
		t.Errorf("another clip's message changed to %d", o.TelegramMessageID)
	}

	// A decided clip and a missing one are answered, not sent.
	for _, tc := range []struct {
		data, notice string
	}{
		{fmt.Sprintf("resend:%d", decided), fmt.Sprintf("Clip #%d is no longer awaiting review.", decided)},
		{"resend:999999", "Clip #999999 does not exist."},
	} {
		f.dispatch(callbackUpdate(3, ownerChat, tc.data))
		calls := f.api.take()
		if len(calls) != 1 || calls[0].method != "answerCallbackQuery" || calls[0].fields["text"] != tc.notice {
			t.Errorf("%s: calls = %+v, want only the answer %q", tc.data, calls, tc.notice)
		}
	}
	if d := f.clip(decided); d.TelegramMessageID != 52 {
		t.Errorf("decided clip's message changed to %d", d.TelegramMessageID)
	}
}

// TestResendUnsentClip checks that a clip that never had a review message
// gets one, recorded with both ids.
func TestResendUnsentClip(t *testing.T) {
	f := newStatusFixture(t, StatusOptions{})
	id := f.addClip(clipSpec{offset: 10}) // no file: sent as text
	msg, err := f.sender.Resend(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	calls := f.api.take()
	if len(calls) != 1 || calls[0].method != "sendMessage" {
		t.Fatalf("calls = %+v, want one sendMessage", calls)
	}
	c := f.clip(id)
	if c.TelegramChatID != ownerChat || c.TelegramMessageID != msg.MessageID {
		t.Errorf("clip records %d/%d, want %d/%d", c.TelegramChatID, c.TelegramMessageID, ownerChat, msg.MessageID)
	}
	// The sender then has nothing left to send.
	if res := f.pass(); len(res.Sent)+len(res.Notified) != 0 {
		t.Errorf("SendPending after Resend sent %+v", res)
	}
}

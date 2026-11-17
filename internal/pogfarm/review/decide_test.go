package review

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// decideFixture is a bot with a Decider registered, wired to a fake Bot API
// and a temporary store.
type decideFixture struct {
	*senderFixture
	decider *Decider
	bot     *Bot
	nextUpd int64
}

var decideTargets = []string{"youtube", "fake"}

const decideMessageID = int64(77)

func newDecideFixture(t *testing.T) *decideFixture {
	t.Helper()
	sf := newSenderFixture(t)
	logger := slog.New(slog.NewTextHandler(sf.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d, err := NewDecider(DeciderOptions{
		Store: sf.store, Client: sf.api.client(), Targets: decideTargets,
		UndoWindow: 5 * time.Minute, Now: sf.clock.Now, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	b := NewBot(sf.api.client(), ownerChat, logger)
	d.Register(b)
	sf.api.setRespond(decideRespond)
	return &decideFixture{senderFixture: sf, decider: d, bot: b}
}

// addSentClip adds a pending clip whose review message is recorded.
func (f *decideFixture) addSentClip() int64 {
	return f.addClip(clipSpec{words: []string{"no", "way"}, content: []byte("video"), messageID: decideMessageID})
}

// videoMessage and textMessage are the review message a tap arrives with.
func videoMessage(chat int64) *Message {
	return &Message{MessageID: decideMessageID, Chat: Chat{ID: chat, Type: "private"}, Date: 1700000000}
}

func textMessage(chat int64, text string) *Message {
	m := videoMessage(chat)
	m.Text = text
	return m
}

// tap delivers a button tap through the bot, as polling would.
func (f *decideFixture) tap(chat int64, data string, m *Message) {
	f.t.Helper()
	f.nextUpd++
	u := Update{UpdateID: f.nextUpd, CallbackQuery: &CallbackQuery{
		ID: fmt.Sprintf("cb-%d", f.nextUpd), From: User{ID: chat}, Message: m, Data: data,
	}}
	f.bot.dispatch(context.Background(), &u)
}

func (f *decideFixture) uploads(clipID int64) []store.Upload {
	f.t.Helper()
	rows, err := f.store.Reader().Query(`SELECT id FROM uploads WHERE clip_id = ? ORDER BY id`, clipID)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			f.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	var out []store.Upload
	for _, id := range ids {
		u, err := store.GetUpload(context.Background(), f.store.Reader(), id)
		if err != nil {
			f.t.Fatal(err)
		}
		out = append(out, u)
	}
	return out
}

// wantCalls checks the methods of the Bot API calls since the last check
// and returns the calls.
func (f *decideFixture) wantCalls(methods ...string) []botCall {
	f.t.Helper()
	calls := f.api.take()
	var got []string
	for _, c := range calls {
		got = append(got, c.method)
	}
	if !reflect.DeepEqual(got, methods) {
		f.t.Fatalf("Bot API calls: got %v, want %v", got, methods)
	}
	return calls
}

func undoButton(id int64) []InlineKeyboardButton {
	return []InlineKeyboardButton{{Text: "Undo (5 min)", CallbackData: fmt.Sprintf("undo:%d", id)}}
}

func (f *decideFixture) caption(id int64) string {
	f.t.Helper()
	info, ok, err := decideInfo(context.Background(), f.store, id)
	if err != nil || !ok {
		f.t.Fatalf("decideInfo: %v %v", ok, err)
	}
	return info.caption(MaxCaptionLength)
}

func TestApproveCreatesOneUploadPerTargetAndEditsTheMessage(t *testing.T) {
	f := newDecideFixture(t)
	id := f.addSentClip()

	f.tap(ownerChat, CallbackData(ActionApprove, id), videoMessage(ownerChat))

	c := f.clip(id)
	if c.ReviewStatus != store.ReviewApproved || !c.ReviewedAt.Equal(t0) {
		t.Fatalf("clip: status %s reviewed %s", c.ReviewStatus, c.ReviewedAt)
	}
	ups := f.uploads(id)
	var targets []string
	for _, u := range ups {
		if u.Status != store.UploadPending || u.Attempts != 0 {
			t.Errorf("upload %+v is not a fresh pending row", u)
		}
		targets = append(targets, u.TargetPlatform)
	}
	if !reflect.DeepEqual(targets, decideTargets) {
		t.Fatalf("upload targets: got %v, want %v", targets, decideTargets)
	}

	calls := f.wantCalls("answerCallbackQuery", "editMessageCaption")
	if calls[0].fields["text"] != "Approved." {
		t.Errorf("answer: %q", calls[0].fields["text"])
	}
	edit := calls[1]
	if edit.fields["chat_id"] != fmt.Sprint(ownerChat) || edit.fields["message_id"] != fmt.Sprint(decideMessageID) {
		t.Errorf("edited the wrong message: %v", edit.fields)
	}
	if want := "Decision: approved, queued for posting.\n\n" + f.caption(id); edit.fields["caption"] != want {
		t.Errorf("caption:\n got %q\nwant %q", edit.fields["caption"], want)
	}
	if got, want := edit.fields["reply_markup"], keyboardJSON(undoButton(id)); got != want {
		t.Errorf("keyboard: got %s, want %s", got, want)
	}
	if n := len(CallbackData(ActionUndo, 1<<62)); n > MaxCallbackDataBytes {
		t.Errorf("undo callback data is %d bytes", n)
	}
}

func TestRejectCreatesNoUploadsAndEditsTextMessages(t *testing.T) {
	f := newDecideFixture(t)
	id := f.addSentClip()
	original := "Clip #1 · SomeStreamer\nThe clip is too large.\n\n“no way”"

	f.tap(ownerChat, CallbackData(ActionReject, id), textMessage(ownerChat, original))

	if c := f.clip(id); c.ReviewStatus != store.ReviewRejected || !c.ReviewedAt.Equal(t0) {
		t.Fatalf("clip: status %s reviewed %s", c.ReviewStatus, c.ReviewedAt)
	}
	if ups := f.uploads(id); len(ups) != 0 {
		t.Fatalf("rejection created uploads: %+v", ups)
	}
	calls := f.wantCalls("answerCallbackQuery", "editMessageText")
	if calls[0].fields["text"] != "Rejected." {
		t.Errorf("answer: %q", calls[0].fields["text"])
	}
	if want := "Decision: rejected.\n\n" + original; calls[1].fields["text"] != want {
		t.Errorf("text: got %q, want %q", calls[1].fields["text"], want)
	}

	// Undo brings the text back exactly as it was.
	f.clock.Advance(time.Minute)
	f.tap(ownerChat, CallbackData(ActionUndo, id), textMessage(ownerChat, calls[1].fields["text"]))
	calls = f.wantCalls("answerCallbackQuery", "editMessageText")
	if calls[1].fields["text"] != original {
		t.Errorf("text after undo: got %q, want %q", calls[1].fields["text"], original)
	}
	if got, want := calls[1].fields["reply_markup"], keyboardJSON(reviewButtons(id)); got != want {
		t.Errorf("keyboard after undo: got %s, want %s", got, want)
	}
}

func TestRepeatedAndCrossedTapsChangeNothing(t *testing.T) {
	f := newDecideFixture(t)
	id := f.addSentClip()
	f.tap(ownerChat, CallbackData(ActionApprove, id), videoMessage(ownerChat))
	f.wantCalls("answerCallbackQuery", "editMessageCaption")
	before, beforeUps := f.clip(id), f.uploads(id)

	// The second tap's edit finds the message already like that.
	f.api.setRespond(func(c botCall) (int, string, bool) {
		if strings.HasPrefix(c.method, "edit") {
			return http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`, true
		}
		return decideRespond(c)
	})
	f.clock.Advance(time.Minute)
	f.tap(ownerChat, CallbackData(ActionApprove, id), videoMessage(ownerChat))
	calls := f.wantCalls("answerCallbackQuery", "editMessageCaption")
	if calls[0].fields["text"] != "Already approved." {
		t.Errorf("answer to a repeated tap: %q", calls[0].fields["text"])
	}
	f.api.setRespond(decideRespond)

	// A Reject tap on the approved clip (a second review message, say).
	f.tap(ownerChat, CallbackData(ActionReject, id), videoMessage(ownerChat))
	calls = f.wantCalls("answerCallbackQuery", "editMessageCaption")
	if calls[0].fields["text"] != "Already approved. Tap Undo to change it." {
		t.Errorf("answer to a crossed tap: %q", calls[0].fields["text"])
	}
	if !strings.HasPrefix(calls[1].fields["caption"], "Decision: approved") {
		t.Errorf("the message must show the existing decision: %q", calls[1].fields["caption"])
	}

	// A late tap, after the undo window: no Undo button any more.
	f.clock.Advance(time.Hour)
	f.tap(ownerChat, CallbackData(ActionReject, id), videoMessage(ownerChat))
	calls = f.wantCalls("answerCallbackQuery", "editMessageCaption")
	if calls[0].fields["text"] != "Already approved." || calls[1].fields["reply_markup"] != decideNoKeyboard {
		t.Errorf("late tap: answer %q, keyboard %s", calls[0].fields["text"], calls[1].fields["reply_markup"])
	}

	if after := f.clip(id); !reflect.DeepEqual(after, before) {
		t.Errorf("the clip changed:\n got %+v\nwant %+v", after, before)
	}
	if after := f.uploads(id); !reflect.DeepEqual(after, beforeUps) {
		t.Errorf("the uploads changed:\n got %+v\nwant %+v", after, beforeUps)
	}
	if strings.Contains(f.log.String(), "handler failed") {
		t.Errorf("a not-modified edit must not be an error:\n%s", f.log)
	}
}

func TestTapFromAnotherChatIsIgnored(t *testing.T) {
	f := newDecideFixture(t)
	id := f.addSentClip()
	for _, action := range []string{ActionApprove, ActionReject, ActionUndo} {
		f.tap(strangerChat, CallbackData(action, id), videoMessage(strangerChat))
	}
	f.wantCalls()
	if c := f.clip(id); c.ReviewStatus != store.ReviewPending || len(f.uploads(id)) != 0 {
		t.Fatalf("a stranger's tap changed the clip: %+v", c)
	}
}

func TestUnknownOrInvalidClipIsAnsweredPolitely(t *testing.T) {
	f := newDecideFixture(t)
	id := f.addSentClip()
	for _, tc := range []struct{ data, answer string }{
		{CallbackData(ActionApprove, 9999), "This clip no longer exists."},
		{CallbackData(ActionReject, 9999), "This clip no longer exists."},
		{CallbackData(ActionUndo, 9999), "This clip no longer exists."},
		{"approve:abc", "This button is not valid."},
		{"undo:-1", "This button is not valid."},
	} {
		f.tap(ownerChat, tc.data, videoMessage(ownerChat))
		calls := f.wantCalls("answerCallbackQuery")
		if calls[0].fields["text"] != tc.answer {
			t.Errorf("%s: answer %q, want %q", tc.data, calls[0].fields["text"], tc.answer)
		}
	}
	if c := f.clip(id); c.ReviewStatus != store.ReviewPending {
		t.Fatalf("clip changed: %+v", c)
	}
	var n int
	if err := f.store.Reader().QueryRow(`SELECT COUNT(*) FROM uploads`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("uploads: %d %v", n, err)
	}
}

func TestUndoWithinTheWindowRestoresPending(t *testing.T) {
	f := newDecideFixture(t)
	id := f.addSentClip()
	f.tap(ownerChat, CallbackData(ActionApprove, id), videoMessage(ownerChat))
	f.wantCalls("answerCallbackQuery", "editMessageCaption")

	f.clock.Advance(5*time.Minute - time.Millisecond)
	f.tap(ownerChat, CallbackData(ActionUndo, id), videoMessage(ownerChat))

	c := f.clip(id)
	if c.ReviewStatus != store.ReviewPending || !c.ReviewedAt.IsZero() {
		t.Fatalf("clip after undo: status %s reviewed %s", c.ReviewStatus, c.ReviewedAt)
	}
	if ups := f.uploads(id); len(ups) != 0 {
		t.Fatalf("undo left uploads: %+v", ups)
	}
	calls := f.wantCalls("answerCallbackQuery", "editMessageCaption")
	if calls[0].fields["text"] != "Undone: the clip is back in review." {
		t.Errorf("answer: %q", calls[0].fields["text"])
	}
	if calls[1].fields["caption"] != f.caption(id) {
		t.Errorf("caption after undo: %q", calls[1].fields["caption"])
	}
	if got, want := calls[1].fields["reply_markup"], keyboardJSON(reviewButtons(id)); got != want {
		t.Errorf("keyboard after undo: got %s, want %s", got, want)
	}

	// A second undo changes nothing; approving again works.
	f.tap(ownerChat, CallbackData(ActionUndo, id), videoMessage(ownerChat))
	if calls := f.wantCalls("answerCallbackQuery", "editMessageCaption"); calls[0].fields["text"] != "The clip is already back in review." {
		t.Errorf("answer to a second undo: %q", calls[0].fields["text"])
	}
	f.tap(ownerChat, CallbackData(ActionApprove, id), videoMessage(ownerChat))
	f.wantCalls("answerCallbackQuery", "editMessageCaption")
	if c := f.clip(id); c.ReviewStatus != store.ReviewApproved || len(f.uploads(id)) != len(decideTargets) {
		t.Fatalf("approve after undo: %s with %d uploads", c.ReviewStatus, len(f.uploads(id)))
	}
}

func TestUndoAfterTheWindowIsRefused(t *testing.T) {
	f := newDecideFixture(t)
	id := f.addSentClip()
	f.tap(ownerChat, CallbackData(ActionApprove, id), videoMessage(ownerChat))
	f.wantCalls("answerCallbackQuery", "editMessageCaption")
	before := f.uploads(id)

	f.clock.Advance(5 * time.Minute)
	f.tap(ownerChat, CallbackData(ActionUndo, id), videoMessage(ownerChat))

	if c := f.clip(id); c.ReviewStatus != store.ReviewApproved || !c.ReviewedAt.Equal(t0) {
		t.Fatalf("clip after a late undo: %s %s", c.ReviewStatus, c.ReviewedAt)
	}
	if after := f.uploads(id); !reflect.DeepEqual(after, before) {
		t.Fatalf("uploads changed: %+v", after)
	}
	calls := f.wantCalls("answerCallbackQuery", "editMessageCaption")
	if calls[0].fields["text"] != "Undo is no longer possible: the undo window has passed." {
		t.Errorf("answer: %q", calls[0].fields["text"])
	}
	if calls[1].fields["reply_markup"] != decideNoKeyboard {
		t.Errorf("the Undo button must go: %s", calls[1].fields["reply_markup"])
	}
	if !strings.HasPrefix(calls[1].fields["caption"], "Decision: approved") {
		t.Errorf("caption: %q", calls[1].fields["caption"])
	}
}

func TestUndoAfterAnUploadStartedIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(u *store.Upload)
	}{
		{"uploading", func(u *store.Upload) { u.Status = store.UploadUploading; u.Attempts = 1 }},
		{"done", func(u *store.Upload) { u.Status = store.UploadDone; u.Attempts = 1; u.PostedAt = t0 }},
		{"pending after a failed attempt", func(u *store.Upload) { u.Attempts = 1; u.LastError = "boom" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDecideFixture(t)
			id := f.addSentClip()
			f.tap(ownerChat, CallbackData(ActionApprove, id), videoMessage(ownerChat))
			f.wantCalls("answerCallbackQuery", "editMessageCaption")
			ups := f.uploads(id)
			tc.start(&ups[0])
			if err := store.UpdateUpload(context.Background(), f.store.Writer(), ups[0]); err != nil {
				t.Fatal(err)
			}
			before := f.uploads(id)

			// A tap on Approve now shows no Undo button.
			f.tap(ownerChat, CallbackData(ActionApprove, id), videoMessage(ownerChat))
			if calls := f.wantCalls("answerCallbackQuery", "editMessageCaption"); calls[1].fields["reply_markup"] != decideNoKeyboard {
				t.Errorf("Undo offered after an upload started: %s", calls[1].fields["reply_markup"])
			}

			f.clock.Advance(time.Minute)
			f.tap(ownerChat, CallbackData(ActionUndo, id), videoMessage(ownerChat))
			if c := f.clip(id); c.ReviewStatus != store.ReviewApproved {
				t.Fatalf("undo went through: %s", c.ReviewStatus)
			}
			if after := f.uploads(id); !reflect.DeepEqual(after, before) {
				t.Fatalf("uploads changed:\n got %+v\nwant %+v", after, before)
			}
			calls := f.wantCalls("answerCallbackQuery", "editMessageCaption")
			if calls[0].fields["text"] != "Undo is no longer possible: posting has started." {
				t.Errorf("answer: %q", calls[0].fields["text"])
			}
			if calls[1].fields["reply_markup"] != decideNoKeyboard {
				t.Errorf("the Undo button must go: %s", calls[1].fields["reply_markup"])
			}
		})
	}
}

func TestTapWithoutMessageEditsTheRecordedMessageButtons(t *testing.T) {
	f := newDecideFixture(t)
	id := f.addSentClip()
	f.tap(ownerChat, CallbackData(ActionReject, id), &Message{MessageID: decideMessageID, Chat: Chat{ID: ownerChat}})
	calls := f.wantCalls("answerCallbackQuery", "editMessageReplyMarkup")
	if calls[1].fields["chat_id"] != fmt.Sprint(ownerChat) || calls[1].fields["message_id"] != fmt.Sprint(decideMessageID) ||
		calls[1].fields["reply_markup"] != keyboardJSON(undoButton(id)) {
		t.Errorf("edit: %v", calls[1].fields)
	}
}

func TestNewDeciderChecksItsOptions(t *testing.T) {
	sf := newSenderFixture(t)
	for _, opts := range []DeciderOptions{
		{Client: sf.api.client()},
		{Store: sf.store},
		{Store: sf.store, Client: sf.api.client(), UndoWindow: -time.Second},
		{Store: sf.store, Client: sf.api.client(), Targets: []string{"youtube", " "}},
		{Store: sf.store, Client: sf.api.client(), Targets: []string{"youtube", "youtube"}},
	} {
		if _, err := NewDecider(opts); err == nil {
			t.Errorf("options %+v accepted", opts)
		}
	}
	d, err := NewDecider(DeciderOptions{Store: sf.store, Client: sf.api.client()})
	if err != nil || d.window != DefaultUndoWindow {
		t.Fatalf("defaults: %v %v", d, err)
	}
}

// decideCheckInvariant checks, in one read snapshot, that every clip's
// upload rows match its review state: none for a pending or rejected clip,
// one per target for an approved one, and a started upload only on an
// approved clip.
func decideCheckInvariant(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.Reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Error(err)
		return
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id, c.review_status, COUNT(u.id), COALESCE(SUM(u.status <> 'pending'), 0)
		FROM clips c LEFT JOIN uploads u ON u.clip_id = c.id GROUP BY c.id`)
	if err != nil {
		t.Error(err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var status store.ReviewStatus
		var n, started int
		if err := rows.Scan(&id, &status, &n, &started); err != nil {
			t.Error(err)
			return
		}
		if (status == store.ReviewApproved && n != len(decideTargets)) || (status != store.ReviewApproved && n != 0) {
			t.Errorf("clip %d is %s with %d upload rows (%d started)", id, status, n, started)
		}
	}
	if err := rows.Err(); err != nil {
		t.Error(err)
	}
}

// Approve, undo and an uploader starting uploads run at the same time; the
// rows always match the review state, and a clip whose upload started stays
// approved.
func TestConcurrentApproveUndoAndUploaderKeepTheInvariant(t *testing.T) {
	f := newDecideFixture(t)
	var ids []int64
	for range 4 {
		ids = append(ids, f.addSentClip2())
	}
	const rounds = 15
	ctx := context.Background()
	var wg sync.WaitGroup
	tap := func(action string, id int64) {
		q := &CallbackQuery{ID: "cb", From: User{ID: ownerChat}, Message: videoMessage(ownerChat),
			Data: CallbackData(action, id)}
		var err error
		switch action {
		case ActionApprove:
			err = f.decider.handleApprove(ctx, q, fmt.Sprint(id))
		case ActionReject:
			err = f.decider.handleReject(ctx, q, fmt.Sprint(id))
		case ActionUndo:
			err = f.decider.handleUndo(ctx, q, fmt.Sprint(id))
		}
		if err != nil {
			t.Error(err)
		}
	}
	for _, id := range ids {
		for _, action := range []string{ActionApprove, ActionUndo, ActionReject} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range rounds {
					tap(action, id)
				}
			}()
		}
	}
	// The uploader: starts the oldest pending upload, in a write
	// transaction of its own, as #29's loop does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range rounds {
			err := f.store.WriteTx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `
					UPDATE uploads SET status = 'uploading', attempts = attempts + 1
					WHERE id = (SELECT id FROM uploads WHERE status = 'pending' ORDER BY id LIMIT 1)`)
				return err
			})
			if err != nil {
				t.Error(err)
			}
			decideCheckInvariant(t, f.store)
		}
	}()
	wg.Wait()
	decideCheckInvariant(t, f.store)

	// Whatever happened, a clip with a started upload can no longer be
	// undone.
	for _, id := range ids {
		started := false
		for _, u := range f.uploads(id) {
			started = started || u.Status != store.UploadPending
		}
		if !started {
			continue
		}
		tap(ActionUndo, id)
		if c := f.clip(id); c.ReviewStatus != store.ReviewApproved {
			t.Errorf("clip %d with a started upload is %s", id, c.ReviewStatus)
		}
	}
}

// addSentClip2 is addSentClip with a review message id of its own, so many
// clips can be recorded as sent.
func (f *decideFixture) addSentClip2() int64 {
	f.nextUpd++
	return f.addClip(clipSpec{offset: float64(f.nextUpd) * 60, words: []string{"hi"}, content: []byte("v"),
		messageID: 1000 + f.nextUpd})
}

// decideNoKeyboard is how an edit removes the buttons.
const decideNoKeyboard = `{"inline_keyboard":[]}`

// decideRespond answers answerCallbackQuery the way Telegram does, with
// true; the fake's default answer is a sent message.
func decideRespond(c botCall) (int, string, bool) {
	if c.method == "answerCallbackQuery" {
		return http.StatusOK, `{"ok":true,"result":true}`, true
	}
	return 0, "", false
}

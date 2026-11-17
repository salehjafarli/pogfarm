package review

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	ownerChat    = int64(4242)
	strangerChat = int64(666)
	// waitLimit is how long a test waits for the bot before failing.
	waitLimit = 5 * time.Second
)

// poll is one scripted answer to GetUpdates.
type poll struct {
	updates []Update
	err     error
}

type sentMessage struct {
	chatID int64
	text   string
}

// fakeAPI plays a script of GetUpdates answers and records what the bot does.
// Once the script runs out, GetUpdates blocks like an idle long poll.
type fakeAPI struct {
	mu       sync.Mutex
	script   []poll
	offsets  []int64
	sent     []sentMessage
	answered []string
	sendErr  error

	idleOnce sync.Once
	idle     chan struct{}
}

func newFakeAPI(script ...poll) *fakeAPI {
	return &fakeAPI{script: script, idle: make(chan struct{})}
}

func (f *fakeAPI) GetUpdates(ctx context.Context, offset int64, _ time.Duration) ([]Update, error) {
	f.mu.Lock()
	f.offsets = append(f.offsets, offset)
	if len(f.script) > 0 {
		next := f.script[0]
		f.script = f.script[1:]
		f.mu.Unlock()
		return next.updates, next.err
	}
	f.mu.Unlock()
	f.idleOnce.Do(func() { close(f.idle) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *fakeAPI) SendMessage(_ context.Context, chatID int64, text string) (*Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	f.sent = append(f.sent, sentMessage{chatID, text})
	return &Message{MessageID: int64(len(f.sent)), Chat: Chat{ID: chatID}, Text: text}, nil
}

func (f *fakeAPI) AnswerCallbackQuery(_ context.Context, id, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answered = append(f.answered, id)
	return nil
}

func textUpdate(id, chatID int64, text string) Update {
	return Update{UpdateID: id, Message: &Message{MessageID: id, Chat: Chat{ID: chatID, Type: "private"}, Text: text}}
}

func callbackUpdate(id, chatID int64, data string) Update {
	return Update{UpdateID: id, CallbackQuery: &CallbackQuery{
		ID:      "cb-" + data,
		From:    User{ID: chatID},
		Message: &Message{MessageID: 1, Chat: Chat{ID: chatID, Type: "private"}},
		Data:    data,
	}}
}

// newTestBot returns a bot that logs into the returned buffer and retries
// almost immediately.
func newTestBot(api API) (*Bot, *bytes.Buffer) {
	var log bytes.Buffer
	b := NewBot(api, ownerChat, slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})))
	b.minBackoff = time.Millisecond
	b.maxBackoff = 4 * time.Millisecond
	return b, &log
}

// runScript runs the bot until it has consumed the whole script and gone
// idle, then cancels it and waits for Run to return.
func runScript(t *testing.T, b *Bot, api *fakeAPI) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Run(ctx)
	}()
	select {
	case <-api.idle:
	case <-time.After(waitLimit):
		t.Fatal("the bot did not work through its updates")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(waitLimit):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestPingFromOwnerAnswersPong(t *testing.T) {
	for _, text := range []string{"/ping", "/PING", "/ping@pogfarm_bot", "/ping  now please", "/ping\nnow"} {
		t.Run(text, func(t *testing.T) {
			api := newFakeAPI(poll{updates: []Update{textUpdate(1, ownerChat, text)}})
			b, _ := newTestBot(api)
			runScript(t, b, api)

			want := []sentMessage{{ownerChat, "pong"}}
			if !reflect.DeepEqual(api.sent, want) {
				t.Errorf("sent %v, want %v", api.sent, want)
			}
		})
	}
}

func TestOtherChatsAreIgnored(t *testing.T) {
	const secretText = "/ping let-me-in-0xDEADBEEF"
	var handled []string
	api := newFakeAPI(poll{updates: []Update{
		textUpdate(1, strangerChat, secretText),
		textUpdate(2, strangerChat, "hello from a stranger"),
		callbackUpdate(3, strangerChat, "approve:clip-abc"),
		// A tap with no message attached is judged by who tapped.
		{UpdateID: 4, CallbackQuery: &CallbackQuery{ID: "cb-bare", From: User{ID: strangerChat}, Data: "approve:clip-xyz"}},
		// The owner writing in some group: the chat is what counts, not the sender.
		{UpdateID: 5, Message: &Message{Chat: Chat{ID: -100123, Type: "group"}, From: &User{ID: ownerChat}, Text: "/ping"}},
		textUpdate(6, ownerChat, "/ping"),
	}})
	b, log := newTestBot(api)
	b.HandleMessage(func(_ context.Context, m *Message) error {
		handled = append(handled, "message "+m.Text)
		return nil
	})
	b.HandleCallback("approve", func(_ context.Context, _ *CallbackQuery, payload string) error {
		handled = append(handled, "approve "+payload)
		return nil
	})
	runScript(t, b, api)

	if len(handled) != 0 {
		t.Errorf("handlers ran for a chat that is not allowed: %v", handled)
	}
	if len(api.answered) != 0 {
		t.Errorf("a stranger's tap was answered: %v", api.answered)
	}
	// Only the owner's ping, which came after, is answered: the strangers
	// neither got a reply nor stopped the bot.
	if want := []sentMessage{{ownerChat, "pong"}}; !reflect.DeepEqual(api.sent, want) {
		t.Errorf("sent %v, want %v", api.sent, want)
	}

	out := log.String()
	if n := strings.Count(out, "ignored update from a chat that is not allowed"); n != 5 {
		t.Errorf("want 5 ignored updates logged, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "chat_id=666") || !strings.Contains(out, "chat_id=-100123") {
		t.Errorf("the log should name the chats that were refused:\n%s", out)
	}
	for _, content := range []string{"let-me-in", "DEADBEEF", "stranger", "approve", "clip-"} {
		if strings.Contains(out, content) {
			t.Errorf("the log contains content from an ignored update (%q):\n%s", content, out)
		}
	}
}

func TestRouting(t *testing.T) {
	var got []string
	api := newFakeAPI(poll{updates: []Update{
		textUpdate(1, ownerChat, "/status  all nodes "),
		textUpdate(2, ownerChat, "just some words"),
		callbackUpdate(3, ownerChat, "approve:17"),
		callbackUpdate(4, ownerChat, "undo:9:extra"),
		callbackUpdate(5, ownerChat, "refresh"),
		callbackUpdate(6, ownerChat, "nobody-handles-this:1"),
		textUpdate(7, ownerChat, "/nosuchcommand"),
		// An update of a kind the bot does not use.
		{UpdateID: 8},
	}})
	b, _ := newTestBot(api)
	b.HandleCommand("/Status", func(_ context.Context, m *Message, args string) error {
		got = append(got, "status ["+args+"]")
		return nil
	})
	b.HandleMessage(func(_ context.Context, m *Message) error {
		got = append(got, "message ["+m.Text+"]")
		return nil
	})
	for _, action := range []string{"approve", "undo", "refresh"} {
		b.HandleCallback(action, func(_ context.Context, q *CallbackQuery, payload string) error {
			got = append(got, action+" ["+payload+"] "+q.ID)
			return nil
		})
	}
	runScript(t, b, api)

	want := []string{
		"status [all nodes]",
		"message [just some words]",
		"approve [17] cb-approve:17",
		"undo [9:extra] cb-undo:9:extra",
		"refresh [] cb-refresh",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("handlers saw\n%v\nwant\n%v", got, want)
	}
	// A tap nobody handles is still answered, so the button stops spinning.
	if want := []string{"cb-nobody-handles-this:1"}; !reflect.DeepEqual(api.answered, want) {
		t.Errorf("answered %v, want %v", api.answered, want)
	}
	if len(api.sent) != 0 {
		t.Errorf("unexpected messages sent: %v", api.sent)
	}
}

func TestMessagesAreDroppedWithoutAHandler(t *testing.T) {
	api := newFakeAPI(poll{updates: []Update{textUpdate(1, ownerChat, "hello"), textUpdate(2, ownerChat, "/ping")}})
	b, _ := newTestBot(api)
	runScript(t, b, api)
	if want := []sentMessage{{ownerChat, "pong"}}; !reflect.DeepEqual(api.sent, want) {
		t.Errorf("sent %v, want %v", api.sent, want)
	}
}

func TestOffsetConfirmsHandledUpdates(t *testing.T) {
	api := newFakeAPI(
		poll{updates: []Update{textUpdate(10, ownerChat, "/ping"), textUpdate(11, ownerChat, "/ping")}},
		poll{}, // a long poll that timed out with nothing
		poll{err: errors.New("connection reset")},
		poll{updates: []Update{textUpdate(15, strangerChat, "/ping")}},
	)
	b, _ := newTestBot(api)
	runScript(t, b, api)

	// Every update is asked for once only, ignored ones included, and a
	// failed poll does not move the offset.
	if want := []int64{0, 12, 12, 12, 16}; !reflect.DeepEqual(api.offsets, want) {
		t.Errorf("offsets %v, want %v", api.offsets, want)
	}
	if len(api.sent) != 2 {
		t.Errorf("want 2 pongs, got %v", api.sent)
	}
}

func TestPollFailuresBackOffAndRecover(t *testing.T) {
	netErr := errors.New("dial tcp: connection refused")
	api := newFakeAPI(
		poll{err: netErr},
		poll{err: netErr},
		poll{err: netErr},
		poll{err: netErr},
		poll{err: netErr},
		poll{updates: []Update{textUpdate(1, ownerChat, "/ping")}},
		// After a success the delay starts from the bottom again.
		poll{err: netErr},
		// Telegram's own flood limit asks for a longer wait than the backoff.
		poll{err: &APIError{Method: "getUpdates", Code: 429, Description: "Too Many Requests", RetryAfter: 7 * time.Second}},
		poll{updates: []Update{textUpdate(2, ownerChat, "/ping")}},
	)
	b, log := newTestBot(api)
	b.minBackoff = time.Second
	b.maxBackoff = 8 * time.Second
	var waits []time.Duration
	b.sleep = func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		return true
	}
	runScript(t, b, api)

	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second,
		time.Second, 7 * time.Second,
	}
	if !reflect.DeepEqual(waits, want) {
		t.Errorf("waits %v, want %v", waits, want)
	}
	if want := []sentMessage{{ownerChat, "pong"}, {ownerChat, "pong"}}; !reflect.DeepEqual(api.sent, want) {
		t.Errorf("the bot should work again after the failures: sent %v", api.sent)
	}
	out := log.String()
	if n := strings.Count(out, "poll failed"); n != 7 {
		t.Errorf("want 7 failures logged, got %d:\n%s", n, out)
	}
	if n := strings.Count(out, "polling again"); n != 2 {
		t.Errorf("want 2 recoveries logged, got %d:\n%s", n, out)
	}
}

func TestBrokenHandlersDoNotStopTheBot(t *testing.T) {
	api := newFakeAPI(poll{updates: []Update{
		textUpdate(1, ownerChat, "/fail"),
		textUpdate(2, ownerChat, "/panic"),
		textUpdate(3, ownerChat, "/ping"),
	}})
	b, log := newTestBot(api)
	b.HandleCommand("fail", func(context.Context, *Message, string) error { return errors.New("store is down") })
	b.HandleCommand("panic", func(context.Context, *Message, string) error { panic("boom") })
	runScript(t, b, api)

	if want := []sentMessage{{ownerChat, "pong"}}; !reflect.DeepEqual(api.sent, want) {
		t.Errorf("sent %v, want %v", api.sent, want)
	}
	out := log.String()
	for _, want := range []string{"handler failed", "store is down", "handler panicked", "boom"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

func TestFailedReplyIsLoggedAndTheBotGoesOn(t *testing.T) {
	api := newFakeAPI(
		poll{updates: []Update{textUpdate(1, ownerChat, "/ping")}},
		poll{updates: []Update{textUpdate(2, ownerChat, "/ping")}},
	)
	api.sendErr = errors.New("telegram sendMessage: timeout")
	b, log := newTestBot(api)
	runScript(t, b, api)

	if want := []int64{0, 2, 3}; !reflect.DeepEqual(api.offsets, want) {
		t.Errorf("offsets %v, want %v", api.offsets, want)
	}
	if n := strings.Count(log.String(), "handler failed"); n != 2 {
		t.Errorf("want 2 failed replies logged, got %d:\n%s", n, log.String())
	}
}

func TestRunStopsOnCancellation(t *testing.T) {
	stopsWithin := func(t *testing.T, b *Bot, ready <-chan struct{}) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			b.Run(ctx)
		}()
		select {
		case <-ready:
		case <-time.After(waitLimit):
			t.Fatal("the bot never got going")
		}
		cancel()
		select {
		case <-done:
		case <-time.After(waitLimit):
			t.Fatal("Run did not return after cancellation")
		}
	}

	t.Run("while waiting in a long poll", func(t *testing.T) {
		api := newFakeAPI()
		b, _ := newTestBot(api)
		stopsWithin(t, b, api.idle)
	})

	t.Run("while backing off", func(t *testing.T) {
		api := newFakeAPI(poll{err: errors.New("no route to host")})
		b, _ := newTestBot(api)
		b.minBackoff = time.Hour
		b.maxBackoff = time.Hour
		sleeping := make(chan struct{})
		b.sleep = func(ctx context.Context, d time.Duration) bool {
			close(sleeping)
			return sleep(ctx, d)
		}
		stopsWithin(t, b, sleeping)
	})

	t.Run("when already cancelled", func(t *testing.T) {
		api := newFakeAPI()
		b, _ := newTestBot(api)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		b.Run(ctx)
		if len(api.offsets) != 0 {
			t.Errorf("a cancelled bot should not poll, polled %d times", len(api.offsets))
		}
	})
}

func TestParseCommand(t *testing.T) {
	cases := []struct {
		text, name, args string
		ok               bool
	}{
		{"/ping", "ping", "", true},
		{"/Ping@pogfarm_bot  a b ", "ping", "a b", true},
		{"/pending\n3", "pending", "3", true},
		{"/", "", "", true},
		{"ping", "", "", false},
		{" /ping", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		name, args, ok := parseCommand(tc.text)
		if name != tc.name || args != tc.args || ok != tc.ok {
			t.Errorf("parseCommand(%q) = %q, %q, %v; want %q, %q, %v", tc.text, name, args, ok, tc.name, tc.args, tc.ok)
		}
	}
}

func TestSendGoesToTheOwner(t *testing.T) {
	api := newFakeAPI()
	b, _ := newTestBot(api)
	if err := b.Send(context.Background(), "clip ready"); err != nil {
		t.Fatal(err)
	}
	if want := []sentMessage{{ownerChat, "clip ready"}}; !reflect.DeepEqual(api.sent, want) {
		t.Errorf("sent %v, want %v", api.sent, want)
	}
	if b.OwnerChatID() != ownerChat {
		t.Errorf("OwnerChatID = %d", b.OwnerChatID())
	}
}

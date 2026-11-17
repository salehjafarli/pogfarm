package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
)

// API is the part of the Telegram Bot API the bot itself needs. *Client
// implements it; tests substitute a fake.
type API interface {
	GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error)
	SendMessage(ctx context.Context, chatID int64, text string) (*Message, error)
	AnswerCallbackQuery(ctx context.Context, callbackID, text string) error
}

// CommandHandler handles a command such as "/status now". args is the text
// after the command name, trimmed.
type CommandHandler func(ctx context.Context, m *Message, args string) error

// MessageHandler handles a message that is not a command.
type MessageHandler func(ctx context.Context, m *Message) error

// CallbackHandler handles a button tap. Callback data has the form
// "action" or "action:payload"; payload is the part after the first colon.
// The handler is responsible for answering the callback query.
type CallbackHandler func(ctx context.Context, q *CallbackQuery, payload string) error

const (
	defaultPollTimeout = 50 * time.Second
	defaultMinBackoff  = time.Second
	defaultMaxBackoff  = time.Minute
)

// Bot receives updates by long polling and routes those from the owner's chat
// to handlers. Updates from any other chat are dropped.
//
// Register handlers before calling Run; registration is not safe to do
// concurrently with it. Handlers run one at a time, in the order the updates
// arrived, on the goroutine that called Run, so a handler that needs to do
// long work should hand it to a background loop and return.
type Bot struct {
	api   API
	owner int64
	log   *slog.Logger

	commands  map[string]CommandHandler
	callbacks map[string]CallbackHandler
	message   MessageHandler

	pollTimeout time.Duration
	minBackoff  time.Duration
	maxBackoff  time.Duration
	// sleep waits between retries; tests replace it to observe the delays.
	sleep func(ctx context.Context, d time.Duration) bool
}

// NewBot returns a bot that serves only the chat ownerChatID. The /ping
// command, which answers "pong", is already registered.
func NewBot(api API, ownerChatID int64, logger *slog.Logger) *Bot {
	b := &Bot{
		api:         api,
		owner:       ownerChatID,
		log:         logger,
		commands:    map[string]CommandHandler{},
		callbacks:   map[string]CallbackHandler{},
		pollTimeout: defaultPollTimeout,
		minBackoff:  defaultMinBackoff,
		maxBackoff:  defaultMaxBackoff,
		sleep:       sleep,
	}
	b.HandleCommand("ping", func(ctx context.Context, _ *Message, _ string) error {
		return b.Send(ctx, "pong")
	})
	return b
}

// OwnerChatID returns the only chat the bot serves.
func (b *Bot) OwnerChatID() int64 { return b.owner }

// Send sends a plain text message to the owner.
func (b *Bot) Send(ctx context.Context, text string) error {
	_, err := b.api.SendMessage(ctx, b.owner, text)
	return err
}

// HandleCommand registers the handler for a command. name is given without
// the leading slash and is matched case-insensitively. Registering a name
// again replaces its handler.
func (b *Bot) HandleCommand(name string, h CommandHandler) {
	b.commands[strings.ToLower(strings.TrimPrefix(name, "/"))] = h
}

// HandleCallback registers the handler for button taps whose callback data is
// action or starts with action followed by a colon.
func (b *Bot) HandleCallback(action string, h CallbackHandler) {
	b.callbacks[action] = h
}

// HandleMessage registers the handler for messages that are not commands.
// Without one they are dropped.
func (b *Bot) HandleMessage(h MessageHandler) {
	b.message = h
}

// Run polls for updates and dispatches them until ctx is cancelled, then
// returns. A failed poll is logged and retried after a delay that doubles
// from one second up to one minute, and resets once a poll succeeds; no
// failure makes Run return. It blocks, so pogfarm runs it on its own
// goroutine.
func (b *Bot) Run(ctx context.Context) {
	var offset int64
	backoff := b.minBackoff
	failing := false
	for ctx.Err() == nil {
		updates, err := b.api.GetUpdates(ctx, offset, b.pollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			wait := backoff
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.RetryAfter > wait {
				wait = apiErr.RetryAfter
			}
			b.log.Warn("telegram: poll failed, will retry", "err", err, "retry_in", wait)
			failing = true
			backoff = min(backoff*2, b.maxBackoff)
			if !b.sleep(ctx, wait) {
				return
			}
			continue
		}
		if failing {
			b.log.Info("telegram: polling again")
			failing = false
		}
		backoff = b.minBackoff
		for i := range updates {
			if updates[i].UpdateID >= offset {
				offset = updates[i].UpdateID + 1
			}
			b.dispatch(ctx, &updates[i])
		}
	}
}

// sleep waits for d and reports whether it did so in full, as opposed to
// being cut short by ctx.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (b *Bot) dispatch(ctx context.Context, u *Update) {
	switch {
	case u.Message != nil:
		m := u.Message
		if !b.allowed(u, m.Chat.ID) {
			return
		}
		name, args, isCommand := parseCommand(m.Text)
		switch {
		case !isCommand:
			if b.message != nil {
				b.handle(ctx, u, "message", func() error { return b.message(ctx, m) })
			}
		case b.commands[name] != nil:
			h := b.commands[name]
			b.handle(ctx, u, "command "+name, func() error { return h(ctx, m, args) })
		default:
			b.log.Info("telegram: unknown command", "update_id", u.UpdateID)
		}
	case u.CallbackQuery != nil:
		q := u.CallbackQuery
		// A tap arrives with the message its button is on. Without one, the
		// only identity left is the user, whose id is also the id of their
		// private chat with the bot.
		chatID := q.From.ID
		if q.Message != nil {
			chatID = q.Message.Chat.ID
		}
		if !b.allowed(u, chatID) {
			return
		}
		action, payload, _ := strings.Cut(q.Data, ":")
		if h := b.callbacks[action]; h != nil {
			b.handle(ctx, u, "callback "+action, func() error { return h(ctx, q, payload) })
			return
		}
		b.log.Info("telegram: callback with no handler", "update_id", u.UpdateID)
		// Answer anyway, or the button shows a progress indicator for a while.
		b.handle(ctx, u, "callback", func() error { return b.api.AnswerCallbackQuery(ctx, q.ID, "") })
	}
}

// allowed reports whether chatID is the owner's chat. A refusal is logged
// with the chat id only: what a stranger wrote is never logged.
func (b *Bot) allowed(u *Update, chatID int64) bool {
	if chatID == b.owner {
		return true
	}
	b.log.Warn("telegram: ignored update from a chat that is not allowed", "update_id", u.UpdateID, "chat_id", chatID)
	return false
}

// handle runs one handler. An error or a panic is logged and goes no further,
// so a broken handler cannot stop the bot.
func (b *Bot) handle(ctx context.Context, u *Update, what string, fn func() error) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("telegram: handler panicked", "handler", what, "update_id", u.UpdateID, "panic", fmt.Sprint(r))
		}
	}()
	if err := fn(); err != nil && ctx.Err() == nil {
		b.log.Error("telegram: handler failed", "handler", what, "update_id", u.UpdateID, "err", err)
	}
}

// parseCommand splits "/name@bot args" into its lower-cased name and its
// arguments. Text that does not start with a slash is not a command.
func parseCommand(text string) (name, args string, ok bool) {
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	name = text[1:]
	if i := strings.IndexFunc(name, unicode.IsSpace); i >= 0 {
		name, args = name[:i], strings.TrimSpace(name[i:])
	}
	// In a group Telegram appends the bot's username: "/ping@pogfarm_bot".
	name, _, _ = strings.Cut(name, "@")
	return strings.ToLower(name), args, true
}

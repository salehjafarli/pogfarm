package review

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/status"
)

// ActionResend is the callback action of the resend buttons of /pending:
// "resend:42" sends the review message of clip 42 again.
const ActionResend = "resend"

// Defaults of the status commands.
const (
	// DefaultPendingLimit is how many clips /pending lists.
	DefaultPendingLimit = 20
	// DefaultStatusClippers is how many clippers /status lists.
	DefaultStatusClippers = 25
)

// statusResendPerRow is how many resend buttons share a keyboard row.
const statusResendPerRow = 4

// statusNameLength caps a clipper name or streamer name in a status line.
const statusNameLength = 40

// StatusOptions configure the status commands. The zero value of every field
// selects its default.
type StatusOptions struct {
	// PendingLimit is how many clips /pending lists; the rest are counted.
	// Default DefaultPendingLimit.
	PendingLimit int
	// Clippers is how many clippers /status lists; the rest are counted.
	// Default DefaultStatusClippers.
	Clippers int
}

// StatusCommands are the owner's read-only commands: /status, the state of
// the farm, and /pending, the clips awaiting review with a button each to
// send its review message again.
type StatusCommands struct {
	reader       *status.Reader
	client       *Client
	sender       *Sender
	pendingLimit int
	clippers     int
}

// NewStatusCommands returns the commands reading from reader. Replies with
// buttons go out through client; the resend buttons use sender.
func NewStatusCommands(reader *status.Reader, client *Client, sender *Sender, opts StatusOptions) (*StatusCommands, error) {
	if reader == nil || client == nil || sender == nil {
		return nil, errors.New("review: status commands need a reader, a client and a sender")
	}
	c := &StatusCommands{reader: reader, client: client, sender: sender,
		pendingLimit: opts.PendingLimit, clippers: opts.Clippers}
	if c.pendingLimit <= 0 {
		c.pendingLimit = DefaultPendingLimit
	}
	if c.clippers <= 0 {
		c.clippers = DefaultStatusClippers
	}
	return c, nil
}

// Register adds /status, /pending and the resend button to b. b must serve
// the chat the client and the sender send to.
func (c *StatusCommands) Register(b *Bot) {
	b.HandleCommand("status", func(ctx context.Context, _ *Message, _ string) error {
		snap, err := c.reader.Snapshot(ctx)
		if err != nil {
			_ = b.Send(ctx, "Could not read the status. The error is in the log.")
			return err
		}
		return b.Send(ctx, c.statusText(snap))
	})
	b.HandleCommand("pending", func(ctx context.Context, _ *Message, _ string) error {
		clips, total, err := c.reader.Pending(ctx, c.pendingLimit)
		if err != nil {
			_ = b.Send(ctx, "Could not read the pending clips. The error is in the log.")
			return err
		}
		text, keyboard := pendingText(clips, total, c.reader.Now())
		if keyboard == nil {
			return b.Send(ctx, text)
		}
		_, err = c.client.SendMessageMarkup(ctx, b.OwnerChatID(), text, keyboard)
		return err
	})
	b.HandleCallback(ActionResend, func(ctx context.Context, q *CallbackQuery, payload string) error {
		id, err := ParseClipID(payload)
		if err != nil {
			_ = c.client.AnswerCallbackQuery(ctx, q.ID, "")
			return err
		}
		_, err = c.sender.Resend(ctx, id)
		notice := "Sent again."
		switch {
		case errors.Is(err, ErrNotPending):
			notice = fmt.Sprintf("Clip #%d is no longer awaiting review.", id)
			err = nil
		case errors.Is(err, ErrClipNotFound):
			notice = fmt.Sprintf("Clip #%d does not exist.", id)
			err = nil
		case err != nil:
			notice = fmt.Sprintf("Could not send clip #%d again.", id)
		}
		if answerErr := c.client.AnswerCallbackQuery(ctx, q.ID, notice); answerErr != nil && err == nil {
			err = answerErr
		}
		return err
	})
}

// statusText formats a snapshot as a plain text message.
func (c *StatusCommands) statusText(s status.Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pogfarm · %s UTC\n\n", s.At.UTC().Format("Jan 2 15:04"))
	fmt.Fprintf(&b, "Jobs: %d queued, %d leased, %d done today, %d failed\n",
		s.Jobs.Queued, s.Jobs.Leased, s.Jobs.DoneToday, s.Jobs.Failed)
	fmt.Fprintf(&b, "VODs: %d scanning, %d new, %d retrying, %d failed\n",
		s.VODs.Scanning, s.VODs.New, s.VODs.Retrying, s.VODs.Failed)
	fmt.Fprintf(&b, "Review: %d awaiting", s.Clips.AwaitingReview)
	if s.Clips.Unsent > 0 {
		fmt.Fprintf(&b, " (%d not sent yet)", s.Clips.Unsent)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Uploads: %d pending, %d uploading, %d failed\n",
		s.Uploads.Pending, s.Uploads.Uploading, s.Uploads.Failed)

	online := 0
	for _, cl := range s.Clippers {
		if cl.Online {
			online++
		}
	}
	b.WriteString("\n")
	if len(s.Clippers) == 0 {
		b.WriteString("Clippers: none seen yet")
		return truncateText(b.String(), MaxMessageLength)
	}
	fmt.Fprintf(&b, "Clippers: %d online, %d offline\n", online, len(s.Clippers)-online)
	shown := min(len(s.Clippers), c.clippers)
	for _, cl := range s.Clippers[:shown] {
		state := "online"
		if !cl.Online {
			state = "OFFLINE"
		}
		work := "idle"
		if cl.JobID != 0 {
			work = "job " + strconv.FormatInt(cl.JobID, 10)
		}
		fmt.Fprintf(&b, "%s: %s, seen %s, %s\n",
			statusName(cl.Name), state, ago(s.At.Sub(cl.LastSeen)), work)
	}
	if rest := len(s.Clippers) - shown; rest > 0 {
		fmt.Fprintf(&b, "and %d more\n", rest)
	}
	fmt.Fprintf(&b, "(offline: silent for over %s)", ago(s.OfflineAfter))
	return truncateText(b.String(), MaxMessageLength)
}

// pendingText formats the clips awaiting review as a plain text message and
// the keyboard of resend buttons, one per listed clip. The keyboard is nil
// when no clip is listed.
func pendingText(clips []status.PendingClip, total int, now time.Time) (string, *InlineKeyboardMarkup) {
	if total == 0 {
		return "No clips awaiting review.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d awaiting review, oldest first:\n\n", total)
	keyboard := &InlineKeyboardMarkup{}
	var row []InlineKeyboardButton
	for _, cl := range clips {
		sent := ""
		if !cl.Sent {
			sent = ", not sent yet"
		}
		fmt.Fprintf(&b, "#%d %s, %s ago%s\n", cl.ID, statusName(cl.Streamer), ago(now.Sub(cl.CreatedAt)), sent)
		row = append(row, InlineKeyboardButton{
			Text:         "Resend #" + strconv.FormatInt(cl.ID, 10),
			CallbackData: CallbackData(ActionResend, cl.ID),
		})
		if len(row) == statusResendPerRow {
			keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, row)
			row = nil
		}
	}
	if len(row) > 0 {
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, row)
	}
	if rest := total - len(clips); rest > 0 {
		fmt.Fprintf(&b, "and %d more\n", rest)
	}
	if len(clips) > 0 {
		b.WriteString("\nTap a button to send that clip's review message again.")
	} else {
		keyboard = nil
	}
	return truncateText(strings.TrimRight(b.String(), "\n"), MaxMessageLength), keyboard
}

// statusName makes a name from a clipper or the database safe and short for
// a status line.
func statusName(s string) string {
	s = truncateText(clean(s), statusNameLength)
	if s == "" {
		return "(no name)"
	}
	return s
}

// ago writes a duration the way a status line reads it: "45s", "12m",
// "3h 5m", "2d 4h".
func ago(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	default:
		return fmt.Sprintf("%dd %dh", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
	}
}

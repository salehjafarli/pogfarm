package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// The title a clip is posted with is shown in its review message, and the
// owner replaces it by replying to that message with the new title. The
// replacement is stored on the clip as its title override, which the
// uploader's composer uses instead of the generated title.

// posttextHeader is the part of the review message header that shows the
// title the clip will be posted with. It is empty when the title is not
// known.
func posttextHeader(title string) string {
	title = truncateText(clean(title), maxTitleLength)
	if title == "" {
		return ""
	}
	return "\nPost title: " + title + "\n(Reply to this message to change it.)"
}

// posttextTitle returns the title a clip will be posted with, or "" when the
// sender has no PostTitle or it failed.
func (s *Sender) posttextTitle(ctx context.Context, clipID int64) string {
	if s.PostTitle == nil {
		return ""
	}
	title, err := s.PostTitle(ctx, clipID)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("review: cannot build the post title; the review message goes without it", "clip", clipID, "err", err)
		}
		return ""
	}
	return title
}

// RegisterTitleOverride makes the bot take a reply to a clip's review
// message as the new title of that clip: while the clip is pending review,
// the text of the reply is stored as its title override and the bot
// confirms it; once the clip has been approved or rejected the bot says the
// title can no longer be changed.
//
// It installs the bot's message handler. The handler registered before, if
// any, still gets every message that is not a reply to a review message, so
// call this after HandleMessage, never before: a later HandleMessage
// replaces this one.
func RegisterTitleOverride(b *Bot, st *store.Store) {
	next := b.message
	b.HandleMessage(func(ctx context.Context, m *Message) error {
		handled, err := posttextReply(ctx, b, st, m)
		if handled || next == nil {
			return err
		}
		return next(ctx, m)
	})
}

// posttextReply handles m if it replies to a review message. handled is
// false for any other message.
func posttextReply(ctx context.Context, b *Bot, st *store.Store, m *Message) (handled bool, err error) {
	if m.ReplyToMessage == nil {
		return false, nil
	}
	clip, err := store.GetClipByReviewMessage(ctx, st.Reader(), m.Chat.ID, m.ReplyToMessage.MessageID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("review: find clip of replied message: %w", err)
	}
	if clip.ReviewStatus != store.ReviewPending {
		return true, posttextRefuse(ctx, b, clip.ID, clip.ReviewStatus)
	}
	title := strings.Join(strings.Fields(clean(m.Text)), " ")
	switch {
	case title == "":
		return true, b.Send(ctx, fmt.Sprintf(
			"To change the title of clip #%d, reply to its review message with the new title as text.", clip.ID))
	case utf8.RuneCountInString(title) > store.MaxTitleOverrideLength:
		return true, b.Send(ctx, fmt.Sprintf(
			"That title is too long: at most %d characters. The title of clip #%d is unchanged.",
			store.MaxTitleOverrideLength, clip.ID))
	}
	err = store.SetTitleOverride(ctx, st.Writer(), clip.ID, title)
	if errors.Is(err, store.ErrNotPending) {
		// Decided between the read and the write.
		return true, posttextRefuse(ctx, b, clip.ID, "")
	}
	if err != nil {
		return true, fmt.Errorf("review: store title of clip %d: %w", clip.ID, err)
	}
	b.log.Info("review: title of clip replaced", "clip", clip.ID)
	return true, b.Send(ctx, fmt.Sprintf(
		"Title of clip #%d set to:\n%s\n\nEach platform may shorten it to its own limit.", clip.ID, title))
}

func posttextRefuse(ctx context.Context, b *Bot, clipID int64, status store.ReviewStatus) error {
	decided := "already been decided"
	switch status {
	case store.ReviewApproved:
		decided = "already been approved"
	case store.ReviewRejected:
		decided = "already been rejected"
	}
	return b.Send(ctx, fmt.Sprintf(
		"Sorry, clip #%d has %s, so its title can no longer be changed.", clipID, decided))
}

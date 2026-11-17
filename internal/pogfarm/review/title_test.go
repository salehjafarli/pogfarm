package review

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// posttextBot is a bot on the fixture's fake Bot API with the title override
// registered after a message handler that records what reaches it.
func posttextBot(f *senderFixture) (*Bot, *[]string) {
	b, _ := newTestBot(f.api.client())
	var passed []string
	b.HandleMessage(func(_ context.Context, m *Message) error {
		passed = append(passed, m.Text)
		return nil
	})
	RegisterTitleOverride(b, f.store)
	return b, &passed
}

func posttextReplyUpdate(id, chatID, repliedTo int64, text string) *Update {
	return &Update{UpdateID: id, Message: &Message{
		MessageID: id + 1000, Chat: Chat{ID: chatID, Type: "private"}, Text: text,
		ReplyToMessage: &Message{MessageID: repliedTo, Chat: Chat{ID: chatID, Type: "private"}},
	}}
}

func (f *senderFixture) posttextOverride(id int64) string {
	f.t.Helper()
	title, err := store.GetTitleOverride(context.Background(), f.store.Reader(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return title
}

// posttextSent returns the texts of the sendMessage calls since the last take.
func (f *senderFixture) posttextSent() []string {
	var texts []string
	for _, c := range f.api.take() {
		if c.method != "sendMessage" {
			f.t.Errorf("unexpected call %s", c.method)
			continue
		}
		if c.fields["chat_id"] != "4242" {
			f.t.Errorf("message sent to chat %s, want the owner's", c.fields["chat_id"])
		}
		texts = append(texts, c.fields["text"])
	}
	return texts
}

func TestTitleOverrideReply(t *testing.T) {
	f := newSenderFixture(t)
	ctx := context.Background()
	pending := f.addClip(clipSpec{words: []string{"hi"}, messageID: 500})
	approved := f.addClip(clipSpec{offset: 100, words: []string{"hi"}, messageID: 501, status: store.ReviewApproved})
	rejected := f.addClip(clipSpec{offset: 200, words: []string{"hi"}, messageID: 502, status: store.ReviewRejected})
	b, passed := posttextBot(f)

	t.Run("pending clip: stored and confirmed", func(t *testing.T) {
		b.dispatch(ctx, posttextReplyUpdate(1, ownerChat, 500, "  Best   clutch\never 🔥 "))
		if got := f.posttextOverride(pending); got != "Best clutch ever 🔥" {
			t.Errorf("override = %q", got)
		}
		sent := f.posttextSent()
		if len(sent) != 1 || !strings.Contains(sent[0], "Title of clip #") || !strings.Contains(sent[0], "Best clutch ever 🔥") {
			t.Errorf("confirmation = %q", sent)
		}
		// A second reply replaces it.
		b.dispatch(ctx, posttextReplyUpdate(2, ownerChat, 500, "Second try"))
		if got := f.posttextOverride(pending); got != "Second try" {
			t.Errorf("override = %q", got)
		}
		f.api.take()
	})

	t.Run("decided clip: refused", func(t *testing.T) {
		for id, want := range map[int64]string{501: "already been approved", 502: "already been rejected"} {
			b.dispatch(ctx, posttextReplyUpdate(3, ownerChat, id, "Too late"))
			sent := f.posttextSent()
			if len(sent) != 1 || !strings.Contains(sent[0], want) || !strings.Contains(sent[0], "can no longer be changed") {
				t.Errorf("reply to %d: sent %q, want a refusal saying %q", id, sent, want)
			}
		}
		for _, id := range []int64{approved, rejected} {
			if got := f.posttextOverride(id); got != "" {
				t.Errorf("clip %d got override %q", id, got)
			}
		}
	})

	t.Run("reply without text: a hint", func(t *testing.T) {
		b.dispatch(ctx, posttextReplyUpdate(4, ownerChat, 500, " \n "))
		sent := f.posttextSent()
		if len(sent) != 1 || !strings.Contains(sent[0], "reply to its review message with the new title") {
			t.Errorf("sent %q", sent)
		}
		if got := f.posttextOverride(pending); got != "Second try" {
			t.Errorf("override changed to %q", got)
		}
	})

	t.Run("too long: refused", func(t *testing.T) {
		b.dispatch(ctx, posttextReplyUpdate(5, ownerChat, 500, strings.Repeat("x", store.MaxTitleOverrideLength+1)))
		sent := f.posttextSent()
		if len(sent) != 1 || !strings.Contains(sent[0], "too long") {
			t.Errorf("sent %q", sent)
		}
		if got := f.posttextOverride(pending); got != "Second try" {
			t.Errorf("override changed to %q", got)
		}
	})

	t.Run("reply to an unrelated message: passed on", func(t *testing.T) {
		*passed = nil
		b.dispatch(ctx, posttextReplyUpdate(6, ownerChat, 999, "just chatting"))
		b.dispatch(ctx, &Update{UpdateID: 7, Message: &Message{MessageID: 7, Chat: Chat{ID: ownerChat}, Text: "no reply"}})
		if got := f.posttextSent(); len(got) != 0 {
			t.Errorf("sent %q", got)
		}
		if strings.Join(*passed, "|") != "just chatting|no reply" {
			t.Errorf("earlier handler got %q", *passed)
		}
		if got := f.posttextOverride(pending); got != "Second try" {
			t.Errorf("override changed to %q", got)
		}
	})

	t.Run("other chat: ignored", func(t *testing.T) {
		*passed = nil
		b.dispatch(ctx, posttextReplyUpdate(8, strangerChat, 500, "hijack"))
		if got := f.posttextSent(); len(got) != 0 {
			t.Errorf("sent %q", got)
		}
		if len(*passed) != 0 {
			t.Errorf("earlier handler got %q", *passed)
		}
		if got := f.posttextOverride(pending); got != "Second try" {
			t.Errorf("override changed to %q", got)
		}
	})
}

// Without an earlier message handler, other messages are dropped quietly.
func TestTitleOverrideAlone(t *testing.T) {
	f := newSenderFixture(t)
	b, _ := newTestBot(f.api.client())
	RegisterTitleOverride(b, f.store)
	b.dispatch(context.Background(), &Update{UpdateID: 1, Message: &Message{MessageID: 1, Chat: Chat{ID: ownerChat}, Text: "hello"}})
	if got := f.api.take(); len(got) != 0 {
		t.Errorf("calls: %v", got)
	}
}

// The reply_to_message field of an update is decoded.
func TestMessageDecodesReply(t *testing.T) {
	var u Update
	err := json.Unmarshal([]byte(`{"update_id":1,"message":{"message_id":9,"chat":{"id":4242,"type":"private"},
		"text":"new","reply_to_message":{"message_id":7,"chat":{"id":4242,"type":"private"},"caption":"Clip #1"}}}`), &u)
	if err != nil {
		t.Fatal(err)
	}
	if u.Message.ReplyToMessage == nil || u.Message.ReplyToMessage.MessageID != 7 {
		t.Errorf("reply_to_message = %+v", u.Message.ReplyToMessage)
	}
}

func TestReviewMessageShowsPostTitle(t *testing.T) {
	f := newSenderFixture(t)
	withTitle := f.addClip(clipSpec{words: []string{"hi"}, content: []byte("video")})
	failing := f.addClip(clipSpec{offset: 100, words: []string{"hi"}, content: []byte("video")})
	f.sender.PostTitle = func(_ context.Context, id int64) (string, error) {
		if id == failing {
			return "", errors.New("no template")
		}
		return "Generated\ntitle", nil
	}
	res := f.pass()
	if len(res.Sent) != 2 {
		t.Fatalf("sent %v, want both clips", res.Sent)
	}
	calls := f.api.take()
	if len(calls) != 2 {
		t.Fatalf("%d calls, want 2", len(calls))
	}
	if c := calls[0].fields["caption"]; !strings.Contains(c, "Post title: Generated title\n(Reply to this message to change it.)") {
		t.Errorf("caption of clip %d = %q", withTitle, c)
	}
	if c := calls[1].fields["caption"]; strings.Contains(c, "Post title") || !strings.Contains(c, "Clip #") {
		t.Errorf("caption when the title fails = %q", c)
	}
	if !strings.Contains(f.log.String(), "cannot build the post title") {
		t.Errorf("failure not logged: %s", f.log.String())
	}
}

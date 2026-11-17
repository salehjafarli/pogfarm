package review

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// EditMessageText replaces the text of a text message and its keyboard. A
// nil markup removes the keyboard.
func (c *Client) EditMessageText(ctx context.Context, chatID, messageID int64, text string, markup *InlineKeyboardMarkup) error {
	params := struct {
		ChatID    int64                 `json:"chat_id"`
		MessageID int64                 `json:"message_id"`
		Text      string                `json:"text"`
		Markup    *InlineKeyboardMarkup `json:"reply_markup"`
	}{chatID, messageID, text, keyboardOrNone(markup)}
	return c.Call(ctx, "editMessageText", params, nil)
}

// EditMessageCaption replaces the caption of a media message, such as a
// video, and its keyboard. A nil markup removes the keyboard.
func (c *Client) EditMessageCaption(ctx context.Context, chatID, messageID int64, caption string, markup *InlineKeyboardMarkup) error {
	params := struct {
		ChatID    int64                 `json:"chat_id"`
		MessageID int64                 `json:"message_id"`
		Caption   string                `json:"caption"`
		Markup    *InlineKeyboardMarkup `json:"reply_markup"`
	}{chatID, messageID, caption, keyboardOrNone(markup)}
	return c.Call(ctx, "editMessageCaption", params, nil)
}

// EditMessageReplyMarkup replaces the keyboard of a message and leaves its
// content alone. A nil markup removes the keyboard.
func (c *Client) EditMessageReplyMarkup(ctx context.Context, chatID, messageID int64, markup *InlineKeyboardMarkup) error {
	params := struct {
		ChatID    int64                 `json:"chat_id"`
		MessageID int64                 `json:"message_id"`
		Markup    *InlineKeyboardMarkup `json:"reply_markup"`
	}{chatID, messageID, keyboardOrNone(markup)}
	return c.Call(ctx, "editMessageReplyMarkup", params, nil)
}

// IsMessageNotModified reports whether err is Telegram refusing an edit
// because the message already looks like that. Such an edit has nothing
// left to do.
func IsMessageNotModified(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusBadRequest &&
		strings.Contains(apiErr.Description, "message is not modified")
}

// keyboardOrNone returns markup, or an empty keyboard for nil: an empty
// keyboard is how an edit removes the buttons.
func keyboardOrNone(markup *InlineKeyboardMarkup) *InlineKeyboardMarkup {
	if markup == nil {
		return &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{}}
	}
	return markup
}

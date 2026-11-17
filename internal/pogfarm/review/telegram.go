package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/salehjafarli/pogfarm/internal/config"
)

// DefaultBaseURL is the public Telegram Bot API.
const DefaultBaseURL = "https://api.telegram.org"

const (
	// callTimeout bounds every call except the long poll.
	callTimeout = 30 * time.Second
	// pollGrace is how long past the long-poll timeout the HTTP request may
	// run before it is abandoned.
	pollGrace = 10 * time.Second
	// maxResponseBytes caps how much of a response is read.
	maxResponseBytes = 8 << 20
)

// Update is one incoming event. At most one of the optional fields is set;
// kinds of update this package does not use decode to an Update with neither.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

// Message is a Telegram message.
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from,omitempty"`
	Chat      Chat   `json:"chat"`
	Date      int64  `json:"date"`
	Text      string `json:"text,omitempty"`
	// ReplyToMessage is the message this one replies to, if any.
	ReplyToMessage *Message `json:"reply_to_message,omitempty"`
}

// Chat identifies a conversation.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// User is a Telegram account.
type User struct {
	ID       int64  `json:"id"`
	IsBot    bool   `json:"is_bot"`
	Username string `json:"username,omitempty"`
}

// CallbackQuery is a tap on an inline keyboard button.
type CallbackQuery struct {
	ID string `json:"id"`
	// From is the user who tapped.
	From User `json:"from"`
	// Message is the message the button is attached to.
	Message *Message `json:"message,omitempty"`
	// Data is the callback data the button was created with.
	Data string `json:"data,omitempty"`
}

// APIError is a request that Telegram received and refused.
type APIError struct {
	// Method is the Bot API method that was called.
	Method string
	// Code is Telegram's error code, which mirrors the HTTP status.
	Code int
	// Description is Telegram's explanation.
	Description string
	// RetryAfter is how long Telegram asks the caller to wait before trying
	// again, or zero when it did not say.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

// Client calls the Telegram Bot API. It is safe for concurrent use.
//
// The bot token is part of every request URL, so errors from the HTTP layer
// are stripped of the URL before they are returned: an error from a Client is
// safe to log.
type Client struct {
	// BaseURL is the API root, without a trailing slash. Tests point it at a
	// local server.
	BaseURL string
	// HTTPClient sends the requests. It must not set a timeout shorter than
	// the long poll; each call carries its own deadline.
	HTTPClient *http.Client

	token config.Secret
}

// NewClient returns a client for the bot that token belongs to, talking to
// the public Telegram API.
func NewClient(token config.Secret) *Client {
	return &Client{
		BaseURL:    DefaultBaseURL,
		HTTPClient: &http.Client{},
		token:      token,
	}
}

// Call invokes a Bot API method with params encoded as its JSON body and
// decodes the result into result. Either may be nil. It is the way to reach
// methods that have no typed wrapper here.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	return c.call(ctx, callTimeout, method, params, result)
}

// GetUpdates long-polls for updates with an id of at least offset, waiting up
// to timeout for the first one. Asking for an offset confirms every update
// below it, which Telegram then never delivers again. Only messages and
// callback queries are requested.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	params := struct {
		Offset         int64    `json:"offset,omitempty"`
		Timeout        int      `json:"timeout"`
		AllowedUpdates []string `json:"allowed_updates"`
	}{offset, int(timeout / time.Second), []string{"message", "callback_query"}}
	var updates []Update
	if err := c.call(ctx, timeout+pollGrace, "getUpdates", params, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

// SendMessage sends a plain text message to a chat.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) (*Message, error) {
	params := struct {
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}{chatID, text}
	var sent Message
	if err := c.Call(ctx, "sendMessage", params, &sent); err != nil {
		return nil, err
	}
	return &sent, nil
}

// AnswerCallbackQuery tells Telegram a button tap was handled, which stops
// the progress indicator on the button. A non-empty text is shown to the user
// as a brief notice.
func (c *Client) AnswerCallbackQuery(ctx context.Context, callbackID, text string) error {
	params := struct {
		ID   string `json:"callback_query_id"`
		Text string `json:"text,omitempty"`
	}{callbackID, text}
	return c.Call(ctx, "answerCallbackQuery", params, nil)
}

func (c *Client) call(ctx context.Context, timeout time.Duration, method string, params, result any) error {
	if params == nil {
		params = struct{}{}
	}
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("telegram %s: encode request: %w", method, err)
	}
	return c.do(ctx, timeout, method, bytes.NewReader(body), int64(len(body)), "application/json", result)
}

// do posts body to a Bot API method and decodes the response. body is closed
// by the HTTP client when it is an io.Closer.
func (c *Client) do(ctx context.Context, timeout time.Duration, method string, body io.Reader, length int64,
	contentType string, result any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	endpoint := c.BaseURL + "/bot" + c.token.Reveal() + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		if closer, ok := body.(io.Closer); ok {
			_ = closer.Close()
		}
		return c.safeError(method, err)
	}
	req.ContentLength = length
	req.Header.Set("Content-Type", contentType)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return c.safeError(method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return c.safeError(method, err)
	}

	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		// Not from Telegram itself: a proxy error page, or a cut-off body.
		return fmt.Errorf("telegram %s: HTTP %d with a body that is not a Bot API response", method, resp.StatusCode)
	}
	if !envelope.OK {
		code := envelope.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return &APIError{
			Method:      method,
			Code:        code,
			Description: envelope.Description,
			RetryAfter:  time.Duration(envelope.Parameters.RetryAfter) * time.Second,
		}
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return fmt.Errorf("telegram %s: decode result: %w", method, err)
	}
	return nil
}

// safeError wraps a transport error so that it cannot carry the token. The
// net/http errors quote the request URL, which contains it.
func (c *Client) safeError(method string, err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	if token := c.token.Reveal(); token != "" && strings.Contains(err.Error(), token) {
		// Wrapping is given up here: the wrapped error would still print it.
		err = errors.New(strings.ReplaceAll(err.Error(), token, "[redacted]"))
	}
	return fmt.Errorf("telegram %s: %w", method, err)
}

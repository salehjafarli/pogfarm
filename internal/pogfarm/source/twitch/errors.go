package twitch

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxErrorMessage is how much of a message sent by Twitch is kept in an
// error.
const maxErrorMessage = 300

// APIError is a Helix response outside 2xx. Callers tell the cases apart by
// Status, for example 404 for something that no longer exists.
type APIError struct {
	// Status is the HTTP status code.
	Status int
	// Message is the explanation Twitch sent, or the standard text of the
	// status when it sent none.
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("twitch api: status %d: %s", e.Status, e.Message)
}

// TokenError is the token endpoint refusing to issue an app access token. A
// 400, 401 or 403 means the configured client id or client secret is wrong;
// retrying with the same credentials cannot succeed.
type TokenError struct {
	// Status is the HTTP status code.
	Status int
	// Message is the explanation Twitch sent, or the standard text of the
	// status when it sent none.
	Message string
}

func (e *TokenError) Error() string {
	return fmt.Sprintf("twitch: token request refused: status %d: %s", e.Status, e.Message)
}

// newAPIError builds the error for a failed Helix response and closes it.
// token is the access token the request carried; it is removed from the
// message in case the response echoes it.
func newAPIError(resp *http.Response, token string) *APIError {
	defer discard(resp)
	return &APIError{Status: resp.StatusCode, Message: errorMessage(resp, token)}
}

// errorMessage extracts the "message" field of a Twitch error body, which
// both Helix and the token endpoint send. It falls back to the standard text
// of the status code, never to the raw body. secret is removed from the
// message in case the response echoes it.
func errorMessage(resp *http.Response, secret string) string {
	var body struct {
		Message string `json:"message"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if json.Unmarshal(data, &body) == nil && body.Message != "" {
		msg := redact(body.Message, secret)
		if len(msg) > maxErrorMessage {
			msg = msg[:maxErrorMessage] + "..."
		}
		return msg
	}
	if text := http.StatusText(resp.StatusCode); text != "" {
		return text
	}
	return "no message"
}

// redact removes a secret from a message that came from outside.
func redact(msg, secret string) string {
	if secret == "" {
		return msg
	}
	return strings.ReplaceAll(msg, secret, "[redacted]")
}

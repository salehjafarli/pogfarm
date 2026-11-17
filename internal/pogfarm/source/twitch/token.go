package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/salehjafarli/pogfarm/internal/config"
)

const (
	// refreshMargin is how long before its expiry a token is replaced, so a
	// request never leaves with a token that expires on the way. Tokens
	// that live less than ten times this are replaced after nine tenths of
	// their life instead.
	refreshMargin = 5 * time.Minute

	// tokenFetchTimeout bounds one token fetch. The fetch is shared between
	// callers, so it runs on its own deadline and not on any one caller's
	// context.
	tokenFetchTimeout = 30 * time.Second
)

// tokenSource obtains the app access token and caches it until shortly
// before it expires. Callers that need a token at the same time share one
// fetch.
type tokenSource struct {
	clientID string
	secret   config.Secret
	url      string
	http     *http.Client
	now      func() time.Time

	mu        sync.Mutex
	token     string
	refreshAt time.Time
	pending   *tokenFetch
}

// tokenFetch is one fetch in flight. token and err are set before done is
// closed.
type tokenFetch struct {
	done  chan struct{}
	token string
	err   error
}

// get returns the cached token, fetching a new one when there is none or the
// cached one is due for refresh. When ctx is cancelled it returns the
// context's error at once; a fetch it started carries on for the other
// callers waiting on it.
func (s *tokenSource) get(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	if s.token != "" && s.now().Before(s.refreshAt) {
		token := s.token
		s.mu.Unlock()
		return token, nil
	}
	f := s.pending
	if f == nil {
		f = &tokenFetch{done: make(chan struct{})}
		s.pending = f
		go s.run(context.WithoutCancel(ctx), f)
	}
	s.mu.Unlock()

	select {
	case <-f.done:
		return f.token, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// invalidate drops the cached token if it is still the one given. A caller
// that was told its token is bad calls this; when another caller has already
// replaced that token, the newer one is kept.
func (s *tokenSource) invalidate(token string) {
	s.mu.Lock()
	if s.token == token {
		s.token = ""
	}
	s.mu.Unlock()
}

// run performs the fetch f and publishes its result.
func (s *tokenSource) run(ctx context.Context, f *tokenFetch) {
	ctx, cancel := context.WithTimeout(ctx, tokenFetchTimeout)
	defer cancel()
	token, lifetime, err := s.fetch(ctx)

	s.mu.Lock()
	if err == nil {
		s.token = token
		s.refreshAt = s.now().Add(lifetime - min(refreshMargin, lifetime/10))
	}
	f.token, f.err = token, err
	s.pending = nil
	s.mu.Unlock()
	close(f.done)
}

// fetch requests a new app access token with the client credentials flow and
// returns it with its lifetime.
func (s *tokenSource) fetch(ctx context.Context) (string, time.Duration, error) {
	form := url.Values{
		"client_id":     {s.clientID},
		"client_secret": {s.secret.Reveal()},
		"grant_type":    {"client_credentials"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("twitch: token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	// The credentials travel in the body, so an error from the HTTP client,
	// which names the URL, cannot contain them.
	resp, err := s.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("twitch: token request: %w", err)
	}
	defer discard(resp)

	if resp.StatusCode != http.StatusOK {
		return "", 0, &TokenError{
			Status:  resp.StatusCode,
			Message: errorMessage(resp, s.secret.Reveal()),
		}
	}

	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	// The decode error is not wrapped: it can quote the input, and the
	// input here is a token.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&body); err != nil {
		return "", 0, errors.New("twitch: token response is not valid JSON")
	}
	if body.AccessToken == "" {
		return "", 0, errors.New("twitch: token response has no access token")
	}
	if body.ExpiresIn <= 0 {
		return "", 0, errors.New("twitch: token response has no expiry")
	}
	return body.AccessToken, time.Duration(body.ExpiresIn) * time.Second, nil
}

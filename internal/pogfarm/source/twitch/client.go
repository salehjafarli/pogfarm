// Package twitch is the Twitch implementation of the source boundary.
//
// This file holds the authenticated Helix HTTP client the rest of the package
// is built on. It obtains an app access token with the OAuth client
// credentials flow, caches it, refreshes it shortly before it expires and
// after a 401, attaches the Client-Id and Authorization headers to every
// request, and honours the Helix rate limit.
//
// The client secret and the access token are secrets. Nothing in this package
// logs, and no error it returns contains either of them.
package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/salehjafarli/pogfarm/internal/config"
)

const (
	// DefaultTokenURL is Twitch's OAuth token endpoint.
	DefaultTokenURL = "https://id.twitch.tv/oauth2/token"
	// DefaultBaseURL is the root of the Helix API.
	DefaultBaseURL = "https://api.twitch.tv/helix"
)

const (
	// defaultHTTPTimeout bounds one HTTP exchange when the caller supplies
	// no HTTP client of its own.
	defaultHTTPTimeout = 30 * time.Second

	// maxRateLimitRetries is how many times one call is retried after a 429
	// before the 429 is returned to the caller.
	maxRateLimitRetries = 3
	// baseBackoff is the first wait after a 429 that does not say when the
	// limit resets. It doubles on every further 429 of the same call.
	baseBackoff = time.Second
	// maxRateLimitWait caps any single wait for the rate limit, so a bogus
	// reset time cannot stall a caller for long. The Helix bucket refills
	// within a minute.
	maxRateLimitWait = time.Minute

	// maxErrorBody is how much of an error response is read.
	maxErrorBody = 64 << 10
)

// Rate-limit response headers of the Helix API.
const (
	headerRateRemaining = "Ratelimit-Remaining"
	headerRateReset     = "Ratelimit-Reset"
)

// Options configures a Client.
type Options struct {
	// ClientID is the client id of the registered Twitch application.
	// Required.
	ClientID string
	// ClientSecret is the application's client secret. Required.
	ClientSecret config.Secret

	// TokenURL is the OAuth token endpoint. Empty means DefaultTokenURL.
	// Tests point it at a local server.
	TokenURL string
	// BaseURL is the root of the Helix API. Empty means DefaultBaseURL.
	// Tests point it at a local server.
	BaseURL string
	// HTTPClient performs the requests. Nil means a client with a 30 second
	// timeout.
	HTTPClient *http.Client
}

// Client is an authenticated Helix HTTP client. It is safe for concurrent
// use; concurrent callers share one cached token and one token fetch.
type Client struct {
	clientID string
	baseURL  string
	http     *http.Client
	tokens   *tokenSource

	// now and sleep are the clock. Tests replace them.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error

	// rate is the rate-limit state reported by the latest response.
	rateMu        sync.Mutex
	rateKnown     bool
	rateRemaining int
	rateReset     time.Time
}

// NewClient returns a client for the given application credentials. It makes
// no request; the token is fetched on first use.
func NewClient(opts Options) (*Client, error) {
	if opts.ClientID == "" {
		return nil, errors.New("twitch: client id is required")
	}
	if !opts.ClientSecret.IsSet() {
		return nil, errors.New("twitch: client secret is required")
	}
	tokenURL, err := endpoint("token URL", opts.TokenURL, DefaultTokenURL)
	if err != nil {
		return nil, err
	}
	baseURL, err := endpoint("base URL", opts.BaseURL, DefaultBaseURL)
	if err != nil {
		return nil, err
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}

	c := &Client{
		clientID: opts.ClientID,
		baseURL:  baseURL,
		http:     httpClient,
		now:      time.Now,
		sleep:    sleepContext,
	}
	c.tokens = &tokenSource{
		clientID: opts.ClientID,
		secret:   opts.ClientSecret,
		url:      tokenURL,
		http:     httpClient,
		// Read the clock through the client so replacing c.now in a test
		// also moves the token's clock.
		now: func() time.Time { return c.now() },
	}
	return c, nil
}

// endpoint validates a configured URL, or returns the default when it is
// empty. A trailing slash is dropped.
func endpoint(name, value, fallback string) (string, error) {
	if value == "" {
		return fallback, nil
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("twitch: %s must be an http or https URL with a host: got %q", name, value)
	}
	return strings.TrimRight(value, "/"), nil
}

// Get performs an authenticated GET of a Helix endpoint and decodes the JSON
// response into out. path is relative to the API root, for example "videos";
// out may be nil to discard the response.
//
// A 401 makes Get fetch a new token and retry once. A 429 makes it wait for
// the rate limit to reset and retry, up to three times. Any other response
// outside 2xx, and a 401 or 429 that outlasts its retries, is returned as an
// *APIError. A failure to obtain a token is returned as a *TokenError when
// Twitch refused the request. Get stops early with the context's error when
// ctx is cancelled, including while it waits.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	path = "/" + strings.TrimLeft(path, "/")
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	refreshed := false
	limited := 0
	for {
		if err := c.awaitRateLimit(ctx); err != nil {
			return err
		}
		token, err := c.tokens.get(ctx)
		if err != nil {
			return err
		}
		resp, err := c.send(ctx, target, token)
		if err != nil {
			return fmt.Errorf("twitch: GET %s: %w", path, err)
		}
		c.noteRateLimit(resp.Header)

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			err := decodeBody(resp, out)
			if err != nil {
				return fmt.Errorf("twitch: GET %s: %w", path, err)
			}
			return nil

		case resp.StatusCode == http.StatusUnauthorized && !refreshed:
			// The token was revoked or expired early. Drop it, unless
			// another caller already replaced it, and try once more.
			discard(resp)
			c.tokens.invalidate(token)
			refreshed = true

		case resp.StatusCode == http.StatusTooManyRequests && limited < maxRateLimitRetries:
			wait := c.backoff(resp.Header, limited)
			discard(resp)
			limited++
			if err := c.sleep(ctx, wait); err != nil {
				return err
			}

		default:
			return fmt.Errorf("twitch: GET %s: %w", path, newAPIError(resp, token))
		}
	}
}

// send performs one GET with the credentials attached.
func (c *Client) send(ctx context.Context, target, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Client-Id", c.clientID)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	return c.http.Do(req)
}

// noteRateLimit records the rate-limit state a response reports. A response
// without both headers leaves the state as it was.
func (c *Client) noteRateLimit(h http.Header) {
	remaining, err := strconv.Atoi(h.Get(headerRateRemaining))
	if err != nil {
		return
	}
	reset, ok := resetTime(h)
	if !ok {
		return
	}
	c.rateMu.Lock()
	c.rateKnown, c.rateRemaining, c.rateReset = true, remaining, reset
	c.rateMu.Unlock()
}

// awaitRateLimit waits until the limit resets when the latest response said
// that no requests remain, so the next request is not sent only to be
// rejected.
func (c *Client) awaitRateLimit(ctx context.Context) error {
	c.rateMu.Lock()
	exhausted := c.rateKnown && c.rateRemaining <= 0
	reset := c.rateReset
	c.rateMu.Unlock()
	if !exhausted {
		return nil
	}
	wait := reset.Sub(c.now())
	if wait <= 0 {
		return nil
	}
	return c.sleep(ctx, min(wait, maxRateLimitWait))
}

// backoff is how long to wait after a 429: until the reset time the response
// names, or, when it names none that lies ahead, an exponentially growing
// pause. retries is how many 429s this call has already waited out.
func (c *Client) backoff(h http.Header, retries int) time.Duration {
	if reset, ok := resetTime(h); ok {
		if wait := reset.Sub(c.now()); wait > 0 {
			return min(wait, maxRateLimitWait)
		}
	}
	return min(baseBackoff<<retries, maxRateLimitWait)
}

// resetTime reads Ratelimit-Reset, a Unix timestamp in seconds.
func resetTime(h http.Header) (time.Time, bool) {
	sec, err := strconv.ParseInt(h.Get(headerRateReset), 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}, false
	}
	return time.Unix(sec, 0), true
}

// decodeBody decodes a successful response into out and closes it.
func decodeBody(resp *http.Response, out any) error {
	defer discard(resp)
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// discard drains what is left of a response, so the connection can be
// reused, and closes it.
func discard(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
}

// sleepContext waits for d, or returns the context's error if ctx ends
// first.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

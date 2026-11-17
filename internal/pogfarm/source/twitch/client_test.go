package twitch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/config"
)

// These are made-up values. No test contacts Twitch.
const (
	testClientID = "test-client-id"
	testSecret   = "test-client-secret-value"
)

// fakeTwitch is a local stand-in for both the token endpoint and Helix. It
// counts the calls to each and lets a test replace either handler. Handlers
// receive the 1-based number of the call.
type fakeTwitch struct {
	srv *httptest.Server

	mu         sync.Mutex
	tokenCalls int
	helixCalls int
	bearers    []string // Authorization header of every Helix call
	token      func(w http.ResponseWriter, r *http.Request, n int)
	helix      func(w http.ResponseWriter, r *http.Request, n int)
}

func newFakeTwitch(t *testing.T) *fakeTwitch {
	t.Helper()
	f := &fakeTwitch{
		token: func(w http.ResponseWriter, _ *http.Request, n int) { issueToken(w, n, 3600) },
		helix: func(w http.ResponseWriter, _ *http.Request, _ int) {
			writeJSON(w, http.StatusOK, `{"data":[{"id":"1"}]}`)
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tokenCalls++
		n, h := f.tokenCalls, f.token
		f.mu.Unlock()
		h(w, r, n)
	})
	mux.HandleFunc("/helix/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.helixCalls++
		f.bearers = append(f.bearers, r.Header.Get("Authorization"))
		n, h := f.helixCalls, f.helix
		f.mu.Unlock()
		h(w, r, n)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTwitch) calls() (token, helix int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls, f.helixCalls
}

func (f *fakeTwitch) seenBearers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.bearers)
}

// wantCalls fails the test unless the endpoints were called this many times.
func (f *fakeTwitch) wantCalls(t *testing.T, token, helix int) {
	t.Helper()
	gotToken, gotHelix := f.calls()
	if gotToken != token || gotHelix != helix {
		t.Errorf("calls: token endpoint %d, helix %d; want %d and %d", gotToken, gotHelix, token, helix)
	}
}

// issueToken answers a token request with the token "token-<n>".
func issueToken(w http.ResponseWriter, n int, expiresIn int) {
	writeJSON(w, http.StatusOK, fmt.Sprintf(`{"access_token":"token-%d","expires_in":%d,"token_type":"bearer"}`, n, expiresIn))
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// fakeClock is a clock that only moves when told to. Sleeping returns at
// once, records the duration and moves the clock forward by it.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
}

func (c *fakeClock) Sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.sleeps)
}

// newTestClient returns a client that talks to f and runs on a fake clock.
func newTestClient(t *testing.T, f *fakeTwitch) (*Client, *fakeClock) {
	t.Helper()
	c, err := NewClient(Options{
		ClientID:     testClientID,
		ClientSecret: config.NewSecret(testSecret),
		TokenURL:     f.srv.URL + "/oauth2/token",
		BaseURL:      f.srv.URL + "/helix",
	})
	if err != nil {
		t.Fatal(err)
	}
	clk := newFakeClock()
	c.now = clk.Now
	c.sleep = clk.Sleep
	return c, clk
}

// noSecrets fails the test if err mentions the client secret or a token the
// fake issued.
func noSecrets(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	if strings.Contains(msg, testSecret) {
		t.Errorf("error contains the client secret: %s", msg)
	}
	if strings.Contains(msg, "token-") {
		t.Errorf("error contains an access token: %s", msg)
	}
}

func TestFetchesTokenOnFirstUseAndSendsCredentials(t *testing.T) {
	f := newFakeTwitch(t)
	var tokenReq, helixReq *http.Request
	var form url.Values
	f.token = func(w http.ResponseWriter, r *http.Request, n int) {
		_ = r.ParseForm()
		tokenReq, form = r, r.PostForm
		issueToken(w, n, 3600)
	}
	f.helix = func(w http.ResponseWriter, r *http.Request, _ int) {
		helixReq = r
		writeJSON(w, http.StatusOK, `{"data":[{"id":"99","title":"a stream"}]}`)
	}
	c, _ := newTestClient(t, f)
	f.wantCalls(t, 0, 0) // nothing is fetched until the first request

	var out struct {
		Data []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"data"`
	}
	if err := c.Get(context.Background(), "videos", url.Values{"user_id": {"42"}, "type": {"archive"}}, &out); err != nil {
		t.Fatal(err)
	}
	f.wantCalls(t, 1, 1)

	if tokenReq.Method != http.MethodPost {
		t.Errorf("token request method: %s", tokenReq.Method)
	}
	if got := tokenReq.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Errorf("token request content type: %q", got)
	}
	if tokenReq.URL.RawQuery != "" {
		t.Errorf("credentials must travel in the body, not the URL: %q", tokenReq.URL.RawQuery)
	}
	want := url.Values{"client_id": {testClientID}, "client_secret": {testSecret}, "grant_type": {"client_credentials"}}
	if form.Encode() != want.Encode() {
		t.Error("token request form does not carry exactly client_id, client_secret and grant_type=client_credentials")
	}

	if helixReq.Method != http.MethodGet || helixReq.URL.Path != "/helix/videos" {
		t.Errorf("helix request: %s %s", helixReq.Method, helixReq.URL.Path)
	}
	if got := helixReq.URL.Query(); got.Get("user_id") != "42" || got.Get("type") != "archive" {
		t.Errorf("helix query: %v", got)
	}
	if got := helixReq.Header.Get("Client-Id"); got != testClientID {
		t.Errorf("Client-Id header: %q", got)
	}
	if got := helixReq.Header.Get("Authorization"); got != "Bearer token-1" {
		t.Errorf("Authorization header is not the bearer token")
	}
	if len(out.Data) != 1 || out.Data[0].ID != "99" || out.Data[0].Title != "a stream" {
		t.Errorf("response not decoded: %+v", out)
	}
}

func TestPathAndOutVariants(t *testing.T) {
	f := newFakeTwitch(t)
	var paths []string
	f.helix = func(w http.ResponseWriter, r *http.Request, _ int) {
		paths = append(paths, r.URL.RequestURI())
		writeJSON(w, http.StatusOK, `{"data":[]}`)
	}
	c, _ := newTestClient(t, f)

	// A leading slash is optional, and a nil out discards the response.
	if err := c.Get(context.Background(), "/users", url.Values{"login": {"someone"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), "clips", nil, nil); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/helix/users?login=someone", "/helix/clips"}; !slices.Equal(paths, want) {
		t.Errorf("paths: got %v, want %v", paths, want)
	}
}

func TestTokenIsCached(t *testing.T) {
	f := newFakeTwitch(t)
	c, _ := newTestClient(t, f)

	for range 5 {
		if err := c.Get(context.Background(), "videos", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	f.wantCalls(t, 1, 5)
	for _, b := range f.seenBearers() {
		if b != "Bearer token-1" {
			t.Fatal("a request did not use the cached token")
		}
	}
}

func TestTokenIsRefreshedBeforeExpiry(t *testing.T) {
	tests := []struct {
		name      string
		expiresIn int
		stillGood time.Duration // age at which the token is still used
		refreshed time.Duration // age at which it has been replaced
	}{
		// A long-lived token is replaced five minutes early.
		{"long lived", 3600, 54 * time.Minute, 56 * time.Minute},
		// A short-lived one after nine tenths of its life.
		{"short lived", 100, 89 * time.Second, 91 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeTwitch(t)
			f.token = func(w http.ResponseWriter, _ *http.Request, n int) { issueToken(w, n, tt.expiresIn) }
			c, clk := newTestClient(t, f)
			get := func() {
				t.Helper()
				if err := c.Get(context.Background(), "videos", nil, nil); err != nil {
					t.Fatal(err)
				}
			}

			get()
			f.wantCalls(t, 1, 1)

			clk.Advance(tt.stillGood)
			get()
			f.wantCalls(t, 1, 2)

			// Still before the expiry Twitch announced, but inside the margin.
			clk.Advance(tt.refreshed - tt.stillGood)
			if tt.refreshed >= time.Duration(tt.expiresIn)*time.Second {
				t.Fatal("test bug: the refresh point must lie before the expiry")
			}
			get()
			f.wantCalls(t, 2, 3)
			if got := f.seenBearers(); got[2] != "Bearer token-2" {
				t.Error("the request after the refresh did not use the new token")
			}

			get()
			f.wantCalls(t, 2, 4)
		})
	}
}

func TestUnauthorizedRefreshesTokenOnceAndRetries(t *testing.T) {
	f := newFakeTwitch(t)
	// The first token has been revoked; any later one is good.
	f.helix = func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Header.Get("Authorization") == "Bearer token-1" {
			writeJSON(w, http.StatusUnauthorized, `{"error":"Unauthorized","status":401,"message":"Invalid OAuth token"}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"data":[{"id":"7"}]}`)
	}
	c, clk := newTestClient(t, f)

	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.Get(context.Background(), "videos", nil, &out); err != nil {
		t.Fatal(err)
	}
	f.wantCalls(t, 2, 2)
	if got, want := f.seenBearers(), []string{"Bearer token-1", "Bearer token-2"}; !slices.Equal(got, want) {
		t.Error("the retry did not use a fresh token")
	}
	if len(out.Data) != 1 || out.Data[0].ID != "7" {
		t.Errorf("the retried response was not decoded: %+v", out)
	}
	if len(clk.Sleeps()) != 0 {
		t.Errorf("a 401 must be retried without waiting, slept %v", clk.Sleeps())
	}

	// The fresh token is cached for the next call.
	if err := c.Get(context.Background(), "videos", nil, nil); err != nil {
		t.Fatal(err)
	}
	f.wantCalls(t, 2, 3)
}

func TestPersistentUnauthorizedIsReturnedAfterOneRefresh(t *testing.T) {
	f := newFakeTwitch(t)
	f.helix = func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, http.StatusUnauthorized, `{"error":"Unauthorized","status":401,"message":"Invalid OAuth token"}`)
	}
	c, _ := newTestClient(t, f)

	err := c.Get(context.Background(), "videos", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("want an *APIError with status 401, got %v", err)
	}
	if apiErr.Message != "Invalid OAuth token" {
		t.Errorf("message: %q", apiErr.Message)
	}
	noSecrets(t, err)
	// One refresh and one retry, no more.
	f.wantCalls(t, 2, 2)
}

func TestConcurrentCallersShareOneTokenFetch(t *testing.T) {
	f := newFakeTwitch(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f.token = func(w http.ResponseWriter, r *http.Request, n int) {
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-r.Context().Done():
		}
		issueToken(w, n, 3600)
	}
	c, _ := newTestClient(t, f)

	const callers = 20
	errs := make(chan error, callers)
	for range callers {
		go func() { errs <- c.Get(context.Background(), "videos", nil, nil) }()
	}
	<-started
	// Give every caller time to reach the fetch that is being held open.
	time.Sleep(50 * time.Millisecond)
	close(release)

	for range callers {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	f.wantCalls(t, 1, callers)
	for _, b := range f.seenBearers() {
		if b != "Bearer token-1" {
			t.Fatal("a caller did not use the shared token")
		}
	}
}

func TestConcurrentUnauthorizedCallersShareOneRefresh(t *testing.T) {
	f := newFakeTwitch(t)
	f.helix = func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Header.Get("Authorization") == "Bearer token-1" {
			writeJSON(w, http.StatusUnauthorized, `{"status":401,"message":"Invalid OAuth token"}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"data":[]}`)
	}
	c, _ := newTestClient(t, f)

	const callers = 20
	errs := make(chan error, callers)
	for range callers {
		go func() { errs <- c.Get(context.Background(), "videos", nil, nil) }()
	}
	for range callers {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	// However many callers were told their token is bad, it is replaced once.
	if tokenCalls, _ := f.calls(); tokenCalls != 2 {
		t.Errorf("token endpoint called %d times, want 2", tokenCalls)
	}
}

func TestRateLimitedWaitsForResetAndRetries(t *testing.T) {
	f := newFakeTwitch(t)
	c, clk := newTestClient(t, f)
	reset := clk.Now().Add(7 * time.Second)
	f.helix = func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 1 {
			w.Header().Set("Ratelimit-Limit", "800")
			w.Header().Set("Ratelimit-Remaining", "0")
			w.Header().Set("Ratelimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			writeJSON(w, http.StatusTooManyRequests, `{"error":"Too Many Requests","status":429,"message":"too many requests"}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"data":[]}`)
	}

	if err := c.Get(context.Background(), "videos", nil, nil); err != nil {
		t.Fatal(err)
	}
	f.wantCalls(t, 1, 2)
	if got, want := clk.Sleeps(), []time.Duration{7 * time.Second}; !slices.Equal(got, want) {
		t.Errorf("waits: got %v, want %v", got, want)
	}
}

func TestRateLimitedBacksOffExponentiallyThenGivesUp(t *testing.T) {
	f := newFakeTwitch(t)
	f.helix = func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, http.StatusTooManyRequests, `{"error":"Too Many Requests","status":429,"message":"too many requests"}`)
	}
	c, clk := newTestClient(t, f)

	err := c.Get(context.Background(), "videos", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests {
		t.Fatalf("want an *APIError with status 429, got %v", err)
	}
	noSecrets(t, err)
	// The first attempt and three retries; the token is fetched once.
	f.wantCalls(t, 1, 4)
	if got, want := clk.Sleeps(), []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; !slices.Equal(got, want) {
		t.Errorf("waits: got %v, want %v", got, want)
	}
}

func TestRateLimitWaitIsCapped(t *testing.T) {
	f := newFakeTwitch(t)
	c, clk := newTestClient(t, f)
	farAway := clk.Now().Add(24 * time.Hour)
	f.helix = func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 1 {
			w.Header().Set("Ratelimit-Reset", strconv.FormatInt(farAway.Unix(), 10))
			writeJSON(w, http.StatusTooManyRequests, `{}`)
			return
		}
		writeJSON(w, http.StatusOK, `{}`)
	}

	if err := c.Get(context.Background(), "videos", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := clk.Sleeps(), []time.Duration{maxRateLimitWait}; !slices.Equal(got, want) {
		t.Errorf("waits: got %v, want %v", got, want)
	}
}

func TestExhaustedRateLimitDelaysTheNextRequest(t *testing.T) {
	f := newFakeTwitch(t)
	c, clk := newTestClient(t, f)
	remaining := "0"
	reset := clk.Now().Add(12 * time.Second)
	f.helix = func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Ratelimit-Remaining", remaining)
		w.Header().Set("Ratelimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		writeJSON(w, http.StatusOK, `{}`)
	}
	get := func() {
		t.Helper()
		if err := c.Get(context.Background(), "videos", nil, nil); err != nil {
			t.Fatal(err)
		}
	}

	// The first response says the bucket is empty; it is itself a success.
	get()
	if len(clk.Sleeps()) != 0 {
		t.Fatalf("nothing to wait for yet, slept %v", clk.Sleeps())
	}

	// So the second request waits for the reset before it is sent.
	clk.Advance(2 * time.Second)
	get()
	if got, want := clk.Sleeps(), []time.Duration{10 * time.Second}; !slices.Equal(got, want) {
		t.Fatalf("waits: got %v, want %v", got, want)
	}

	// The reset has passed, so the stale "0" no longer delays anything.
	get()
	// And a response with requests remaining never does.
	remaining = "799"
	reset = clk.Now().Add(30 * time.Second)
	get()
	get()
	if got := clk.Sleeps(); len(got) != 1 {
		t.Errorf("unexpected further waits: %v", got)
	}
	f.wantCalls(t, 1, 5)
}

func TestTokenEndpointErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantStatus int    // of the *TokenError; 0 when another error is expected
		want       string // substring of the error
	}{
		{"wrong credentials", 400, `{"status":400,"message":"invalid client"}`, 400, "invalid client"},
		{"forbidden", 403, `{"status":403,"message":"invalid client secret"}`, 403, "invalid client secret"},
		{"secret echoed back", 400, `{"status":400,"message":"bad secret ` + testSecret + `"}`, 400, "bad secret [redacted]"},
		{"server error without json", 503, `<html>` + testSecret + `</html>`, 503, "Service Unavailable"},
		{"not json", 200, `token-1 is not json`, 0, "not valid JSON"},
		{"no access token", 200, `{"expires_in":3600,"token_type":"bearer"}`, 0, "no access token"},
		{"no expiry", 200, `{"access_token":"token-1","token_type":"bearer"}`, 0, "no expiry"},
		{"negative expiry", 200, `{"access_token":"token-1","expires_in":-5}`, 0, "no expiry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeTwitch(t)
			f.token = func(w http.ResponseWriter, _ *http.Request, n int) {
				if n == 1 {
					writeJSON(w, tt.status, tt.body)
					return
				}
				issueToken(w, n, 3600)
			}
			c, _ := newTestClient(t, f)

			err := c.Get(context.Background(), "videos", nil, nil)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q lacks %q", err, tt.want)
			}
			noSecrets(t, err)
			var tokenErr *TokenError
			if tt.wantStatus != 0 {
				if !errors.As(err, &tokenErr) || tokenErr.Status != tt.wantStatus {
					t.Errorf("want a *TokenError with status %d, got %v", tt.wantStatus, err)
				}
			} else if errors.As(err, &tokenErr) {
				t.Errorf("want a plain error, got a *TokenError: %v", err)
			}
			// Helix is never called without a token.
			f.wantCalls(t, 1, 0)

			// A failed fetch is not cached: the next call tries again.
			if err := c.Get(context.Background(), "videos", nil, nil); err != nil {
				t.Fatalf("second call: %v", err)
			}
			f.wantCalls(t, 2, 1)
		})
	}
}

func TestTokenEndpointUnreachable(t *testing.T) {
	f := newFakeTwitch(t)
	c, _ := newTestClient(t, f)
	f.srv.Close()

	err := c.Get(context.Background(), "videos", nil, nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "twitch: token request") {
		t.Errorf("error does not say what failed: %v", err)
	}
	noSecrets(t, err)
}

func TestHelixErrorResponses(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantMsg string
	}{
		{"not found", 404, `{"error":"Not Found","status":404,"message":"video not found"}`, "video not found"},
		{"bad request", 400, `{"error":"Bad Request","status":400,"message":"Missing required parameter \"id\""}`, `Missing required parameter "id"`},
		{"server error without json", 500, `<html>boom</html>`, "Internal Server Error"},
		{"empty body", 502, ``, "Bad Gateway"},
		{"token echoed back", 403, `{"status":403,"message":"token token-1 lacks scope"}`, "token [redacted] lacks scope"},
		{"long message", 400, `{"message":"` + strings.Repeat("x", 5000) + `"}`, strings.Repeat("x", maxErrorMessage) + "..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeTwitch(t)
			f.helix = func(w http.ResponseWriter, _ *http.Request, _ int) { writeJSON(w, tt.status, tt.body) }
			c, clk := newTestClient(t, f)

			err := c.Get(context.Background(), "videos", url.Values{"id": {"123"}}, nil)
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("want an *APIError, got %v", err)
			}
			if apiErr.Status != tt.status || apiErr.Message != tt.wantMsg {
				t.Errorf("got status %d message %q, want %d %q", apiErr.Status, apiErr.Message, tt.status, tt.wantMsg)
			}
			if !strings.Contains(err.Error(), "GET /videos") {
				t.Errorf("error does not name the request: %v", err)
			}
			noSecrets(t, err)
			// Errors other than 401 and 429 are not retried.
			f.wantCalls(t, 1, 1)
			if len(clk.Sleeps()) != 0 {
				t.Errorf("unexpected waits: %v", clk.Sleeps())
			}
		})
	}
}

func TestMalformedSuccessBodyIsAnError(t *testing.T) {
	f := newFakeTwitch(t)
	f.helix = func(w http.ResponseWriter, _ *http.Request, _ int) { writeJSON(w, http.StatusOK, `{"data":[`) }
	c, _ := newTestClient(t, f)

	var out struct {
		Data []struct{} `json:"data"`
	}
	err := c.Get(context.Background(), "videos", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("want a decode error, got %v", err)
	}
	noSecrets(t, err)
}

func TestCancelledContextMakesNoRequest(t *testing.T) {
	f := newFakeTwitch(t)
	c, _ := newTestClient(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.Get(ctx, "videos", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	f.wantCalls(t, 0, 0)
}

func TestCancelDuringTokenFetchReturnsAtOnceAndKeepsTheFetch(t *testing.T) {
	f := newFakeTwitch(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f.token = func(w http.ResponseWriter, r *http.Request, n int) {
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-r.Context().Done():
		}
		issueToken(w, n, 3600)
	}
	c, _ := newTestClient(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { first <- c.Get(ctx, "videos", nil, nil) }()
	<-started
	cancel()
	// The token endpoint is still holding the request open, so this returns
	// only because the caller stopped waiting.
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled caller is still waiting for the token")
	}

	// The fetch it started was not cancelled with it: the next caller joins
	// that fetch instead of starting another.
	second := make(chan error, 1)
	go func() { second <- c.Get(context.Background(), "videos", nil, nil) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	f.wantCalls(t, 1, 1)
}

func TestCancelDuringHelixRequest(t *testing.T) {
	f := newFakeTwitch(t)
	f.helix = func(_ http.ResponseWriter, r *http.Request, _ int) { <-r.Context().Done() }
	c, _ := newTestClient(t, f)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.Get(ctx, "videos", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	noSecrets(t, err)
}

func TestCancelDuringRateLimitBackoff(t *testing.T) {
	f := newFakeTwitch(t)
	f.helix = func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(w, http.StatusTooManyRequests, `{}`)
	}
	c, _ := newTestClient(t, f)
	c.sleep = sleepContext // really wait, so the wait can be interrupted

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	begin := time.Now()
	err := c.Get(ctx, "videos", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if elapsed := time.Since(begin); elapsed >= baseBackoff {
		t.Errorf("the backoff was not interrupted: returned after %v", elapsed)
	}
	// It was cancelled during the first wait, before any retry.
	f.wantCalls(t, 1, 1)
}

func TestCancelWhileWaitingForExhaustedRateLimit(t *testing.T) {
	f := newFakeTwitch(t)
	c, clk := newTestClient(t, f)
	f.helix = func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Ratelimit-Remaining", "0")
		w.Header().Set("Ratelimit-Reset", strconv.FormatInt(clk.Now().Add(30*time.Second).Unix(), 10))
		writeJSON(w, http.StatusOK, `{}`)
	}
	if err := c.Get(context.Background(), "videos", nil, nil); err != nil {
		t.Fatal(err)
	}

	c.sleep = sleepContext
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Get(ctx, "videos", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	f.wantCalls(t, 1, 1)
}

func TestSleepContext(t *testing.T) {
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Errorf("an uninterrupted sleep should succeed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

func TestNewClientValidation(t *testing.T) {
	secret := config.NewSecret(testSecret)
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"no client id", Options{ClientSecret: secret}, "client id is required"},
		{"no client secret", Options{ClientID: testClientID}, "client secret is required"},
		{"bad token url", Options{ClientID: testClientID, ClientSecret: secret, TokenURL: "id.twitch.tv/oauth2/token"}, "token URL must be an http or https URL"},
		{"bad base url", Options{ClientID: testClientID, ClientSecret: secret, BaseURL: "ftp://api.twitch.tv"}, "base URL must be an http or https URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewClient(tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want an error containing %q, got %v", tt.want, err)
			}
			noSecrets(t, err)
		})
	}
}

func TestNewClientDefaults(t *testing.T) {
	c, err := NewClient(Options{ClientID: testClientID, ClientSecret: config.NewSecret(testSecret)})
	if err != nil {
		t.Fatal(err)
	}
	if c.baseURL != "https://api.twitch.tv/helix" || c.tokens.url != "https://id.twitch.tv/oauth2/token" {
		t.Errorf("defaults: base %q, token %q", c.baseURL, c.tokens.url)
	}
	if c.http.Timeout != defaultHTTPTimeout {
		t.Errorf("default HTTP client timeout: %v", c.http.Timeout)
	}

	// A trailing slash on a configured URL is dropped.
	c, err = NewClient(Options{ClientID: testClientID, ClientSecret: config.NewSecret(testSecret), BaseURL: "http://127.0.0.1:1/helix/"})
	if err != nil {
		t.Fatal(err)
	}
	if c.baseURL != "http://127.0.0.1:1/helix" {
		t.Errorf("base URL: %q", c.baseURL)
	}
}

func TestClientDoesNotPrintSecrets(t *testing.T) {
	f := newFakeTwitch(t)
	c, _ := newTestClient(t, f)
	if err := c.Get(context.Background(), "videos", nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		out := fmt.Sprintf(verb, c)
		if strings.Contains(out, testSecret) {
			t.Errorf("%s of the client prints the client secret", verb)
		}
		if strings.Contains(out, "token-1") {
			t.Errorf("%s of the client prints the access token", verb)
		}
	}
}

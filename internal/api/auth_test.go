package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	authTestSecret  = "correct-horse-battery-staple"
	authOtherSecret = "another-secret-entirely-wrong"
)

// authNextRecorder is the handler behind the middleware. It records whether it
// was reached.
type authNextRecorder struct{ called bool }

func (n *authNextRecorder) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	n.called = true
	w.WriteHeader(http.StatusNoContent)
}

// authServe sends one request through RequireSecret. authValues are the values of
// the Authorization header; none means the header is absent.
func authServe(t *testing.T, secret string, authValues ...string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	next := &authNextRecorder{}
	req := httptest.NewRequest(http.MethodPost, PathClaim, strings.NewReader(`{"clipper":"node-a"}`))
	for _, v := range authValues {
		req.Header.Add("Authorization", v)
	}
	rec := httptest.NewRecorder()
	RequireSecret(secret, next).ServeHTTP(rec, req)
	return rec, next.called
}

// checkUnauthenticated checks that a response is the contract's
// unauthenticated error and that it leaks none of the given secrets.
func checkUnauthenticated(t *testing.T, status int, header http.Header, body []byte, secrets ...string) {
	t.Helper()
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
	if got := header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := header.Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q, want Bearer", got)
	}
	var resp ErrorResponse
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("body is not an ErrorResponse: %v\nbody: %s", err, body)
	}
	if resp.Error.Code != CodeUnauthenticated {
		t.Errorf("code = %q, want %q", resp.Error.Code, CodeUnauthenticated)
	}
	if resp.Error.Message == "" {
		t.Error("message is empty")
	}
	all := string(body) + "\n" + strings.Join(header.Values("WWW-Authenticate"), "\n")
	for _, s := range secrets {
		if s != "" && strings.Contains(all, s) {
			t.Errorf("response leaks a secret: %s", all)
		}
	}
}

func TestRequireSecretAccepts(t *testing.T) {
	tests := []struct {
		name string
		auth string
	}{
		{"canonical scheme", "Bearer " + authTestSecret},
		{"lowercase scheme", "bearer " + authTestSecret},
		{"uppercase scheme", "BEARER " + authTestSecret},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, called := authServe(t, authTestSecret, tt.auth)
			if !called {
				t.Error("next handler was not called")
			}
			if rec.Code != http.StatusNoContent {
				t.Errorf("status = %d, want the next handler's %d", rec.Code, http.StatusNoContent)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != "" {
				t.Errorf("WWW-Authenticate = %q on an accepted request", got)
			}
		})
	}
}

func TestRequireSecretRejects(t *testing.T) {
	tests := []struct {
		name string
		auth []string
	}{
		{"no header", nil},
		{"empty header", []string{""}},
		{"wrong secret", []string{"Bearer " + authOtherSecret}},
		{"wrong secret of the same length", []string{"Bearer " + strings.Repeat("x", len(authTestSecret))}},
		{"prefix of the secret", []string{"Bearer " + authTestSecret[:len(authTestSecret)-1]}},
		{"secret with a suffix", []string{"Bearer " + authTestSecret + "x"}},
		{"secret in another case", []string{"Bearer " + strings.ToUpper(authTestSecret)}},
		{"secret without a scheme", []string{authTestSecret}},
		{"scheme without a secret", []string{"Bearer"}},
		{"scheme with an empty secret", []string{"Bearer "}},
		{"wrong scheme", []string{"Basic " + authTestSecret}},
		{"scheme glued to the secret", []string{"Bearer" + authTestSecret}},
		{"two spaces before the secret", []string{"Bearer  " + authTestSecret}},
		{"space after the secret", []string{"Bearer " + authTestSecret + " "}},
		{"space before the scheme", []string{" Bearer " + authTestSecret}},
		{"tab instead of space", []string{"Bearer\t" + authTestSecret}},
		{"right secret only in a second header", []string{"Bearer " + authOtherSecret, "Bearer " + authTestSecret}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, called := authServe(t, authTestSecret, tt.auth...)
			if called {
				t.Error("next handler was called")
			}
			checkUnauthenticated(t, rec.Code, rec.Header(), rec.Body.Bytes(), authTestSecret, authOtherSecret)
		})
	}
}

// A server that was never given a secret must not accept an empty token, or
// anything else.
func TestRequireSecretEmptySecretRejectsEverything(t *testing.T) {
	for _, auth := range [][]string{nil, {""}, {"Bearer"}, {"Bearer "}, {"Bearer " + authTestSecret}} {
		rec, called := authServe(t, "", auth...)
		if called {
			t.Errorf("Authorization %q: next handler was called", auth)
		}
		checkUnauthenticated(t, rec.Code, rec.Header(), rec.Body.Bytes())
	}
}

func TestRequireSecretDoesNotChangeTheRequest(t *testing.T) {
	const body = `{"clipper":"node-a"}`
	var gotBody, gotAuth, gotPath string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotAuth, gotPath = string(b), r.Header.Get("Authorization"), r.URL.Path
	})
	req := httptest.NewRequest(http.MethodPost, PathHeartbeat("42"), strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+authTestSecret)
	RequireSecret(authTestSecret, next).ServeHTTP(httptest.NewRecorder(), req)

	if gotBody != body || gotPath != PathHeartbeat("42") || gotAuth != "Bearer "+authTestSecret {
		t.Errorf("next saw body %q, path %q, a changed header: %v", gotBody, gotPath, gotAuth != "Bearer "+authTestSecret)
	}
}

// authRoundTripFunc is a base transport that records what it is asked to send.
type authRoundTripFunc func(*http.Request) (*http.Response, error)

func (f authRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBearerTransportAddsHeader(t *testing.T) {
	var sent *http.Request
	base := authRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent = r
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
	})
	tr := &BearerTransport{Secret: authTestSecret, Base: base}

	req := httptest.NewRequest(http.MethodPost, "http://pogfarm.test"+PathClaim, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want the base transport's 200", resp.StatusCode)
	}
	if sent == nil {
		t.Fatal("base transport was not called")
	}
	if got := sent.Header.Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+authTestSecret {
		t.Errorf("sent %d Authorization values, want exactly one carrying the secret", len(got))
	}
	if got := sent.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, other headers must be kept", got)
	}
	if sent.Method != req.Method || sent.URL.String() != req.URL.String() {
		t.Errorf("sent %s %s, want %s %s", sent.Method, sent.URL, req.Method, req.URL)
	}
	// A RoundTripper must not modify the request it is given.
	if got := req.Header.Values("Authorization"); len(got) != 0 {
		t.Error("the caller's request was modified")
	}
}

func TestBearerTransportReplacesExistingHeader(t *testing.T) {
	var sent *http.Request
	base := authRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent = r
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
	})
	req := httptest.NewRequest(http.MethodPost, "http://pogfarm.test"+PathClaim, nil)
	req.Header.Set("Authorization", "Bearer "+authOtherSecret)
	if _, err := (&BearerTransport{Secret: authTestSecret, Base: base}).RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got := sent.Header.Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+authTestSecret {
		t.Errorf("sent %d Authorization values, want exactly one carrying the transport's secret", len(got))
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+authOtherSecret {
		t.Error("the caller's request was modified")
	}
}

func TestBearerTransportPassesBaseError(t *testing.T) {
	wantErr := errors.New("connection refused")
	base := authRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, wantErr })
	req := httptest.NewRequest(http.MethodPost, "http://pogfarm.test"+PathClaim, nil)
	_, err := (&BearerTransport{Secret: authTestSecret, Base: base}).RoundTrip(req)
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want the base transport's error", err)
	}
}

// authCloseRecorder is a request body that records whether it was closed.
type authCloseRecorder struct {
	io.Reader
	closed bool
}

func (c *authCloseRecorder) Close() error { c.closed = true; return nil }

func TestBearerTransportWithoutSecretSendsNothing(t *testing.T) {
	base := authRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("base transport was called without a secret")
		return nil, errors.New("unreachable")
	})
	body := &authCloseRecorder{Reader: strings.NewReader("{}")}
	req := httptest.NewRequest(http.MethodPost, "http://pogfarm.test"+PathClaim, body)

	resp, err := (&BearerTransport{Base: base}).RoundTrip(req)
	if !errors.Is(err, ErrNoSecret) {
		t.Errorf("err = %v, want ErrNoSecret", err)
	}
	if resp != nil {
		t.Error("got a response without a secret")
	}
	if !body.closed {
		t.Error("request body was not closed")
	}

	// A request without a body must not panic.
	if _, err := (&BearerTransport{Base: base}).RoundTrip(&http.Request{Method: http.MethodGet, Header: http.Header{}}); !errors.Is(err, ErrNoSecret) {
		t.Errorf("err = %v, want ErrNoSecret", err)
	}
}

// newAuthTestServer is a pogfarm-shaped server: the /v1 routes behind
// RequireSecret and a health route outside it.
func newAuthTestServer(t *testing.T, secret string) *httptest.Server {
	t.Helper()
	v1 := http.NewServeMux()
	v1.HandleFunc(RouteClaim, func(w http.ResponseWriter, r *http.Request) {
		var req ClaimRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("claim handler: bad body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ClaimResponse{RetryAfterSeconds: 10})
	})
	v1.HandleFunc(RouteFail, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	root := http.NewServeMux()
	root.Handle(PathPrefix+"/", RequireSecret(secret, v1))
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(root)
	t.Cleanup(srv.Close)
	return srv
}

func authPost(t *testing.T, client *http.Client, url, body string) (*http.Response, []byte) {
	t.Helper()
	resp, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp, b
}

// The client helper and the middleware, wired together over real HTTP.
func TestClientAndMiddlewareTogether(t *testing.T) {
	srv := newAuthTestServer(t, authTestSecret)
	const claimBody = `{"clipper":"node-a"}`

	t.Run("right secret is accepted", func(t *testing.T) {
		client := &http.Client{Transport: &BearerTransport{Secret: authTestSecret}}
		resp, body := authPost(t, client, srv.URL+PathClaim, claimBody)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200\nbody: %s", resp.StatusCode, body)
		}
		var claim ClaimResponse
		if err := json.Unmarshal(body, &claim); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if claim.Job != nil || claim.RetryAfterSeconds != 10 {
			t.Errorf("claim = %+v, want the handler's response", claim)
		}

		resp, _ = authPost(t, client, srv.URL+PathFail("42"), `{"clipper":"node-a","reason":"x","retryable":true}`)
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("fail status = %d, want 204", resp.StatusCode)
		}
	})

	t.Run("wrong secret is rejected", func(t *testing.T) {
		client := &http.Client{Transport: &BearerTransport{Secret: authOtherSecret}}
		resp, body := authPost(t, client, srv.URL+PathClaim, claimBody)
		checkUnauthenticated(t, resp.StatusCode, resp.Header, body, authTestSecret, authOtherSecret)
	})

	t.Run("no secret is rejected", func(t *testing.T) {
		resp, body := authPost(t, http.DefaultClient, srv.URL+PathClaim, claimBody)
		checkUnauthenticated(t, resp.StatusCode, resp.Header, body, authTestSecret)
	})

	t.Run("unknown v1 route is rejected before routing", func(t *testing.T) {
		resp, body := authPost(t, http.DefaultClient, srv.URL+PathPrefix+"/nothing-here", claimBody)
		checkUnauthenticated(t, resp.StatusCode, resp.Header, body, authTestSecret)
	})

	t.Run("route outside v1 needs no secret", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatalf("GET /healthz: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})
}

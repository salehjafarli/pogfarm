package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// The shared secret travels in the standard Authorization header, as
// "Authorization: Bearer <secret>".
const (
	authHeader = "Authorization"
	authScheme = "Bearer"
)

// unauthenticatedMessage is the same for a missing and a wrong secret, and
// never includes what the caller sent.
const unauthenticatedMessage = "missing or wrong shared secret"

// ErrNoSecret is returned by BearerTransport when it has no secret to send.
var ErrNoSecret = errors.New("api: no shared secret configured")

// RequireSecret returns middleware that lets a request through to next only
// when it carries "Authorization: Bearer <secret>" with exactly this secret.
// Every other request is answered with 401 and CodeUnauthenticated, and next
// is not called.
//
// It guards every request it sees. pogfarm wraps all routes under PathPrefix
// with it and registers unauthenticated routes, such as a health endpoint,
// outside it.
//
// The comparison takes the same time whatever the caller sent. An empty
// secret authenticates nobody, so a server that was never given one rejects
// every request instead of accepting an empty token. The secret is never
// logged or echoed back.
func RequireSecret(secret string, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(secret))
	configured := secret != ""
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get(authHeader))
		// Both sides are hashed first, so the comparison always runs over
		// 32 bytes and its duration says nothing about the secret's length
		// or about how much of a guess was right.
		got := sha256.Sum256([]byte(token))
		match := subtle.ConstantTimeCompare(got[:], want[:]) == 1
		if !configured || !ok || !match {
			writeUnauthenticated(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken extracts the token from an Authorization header value. The
// scheme name is case-insensitive, as HTTP requires; the token is taken
// exactly as sent.
func bearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, authScheme) || token == "" {
		return "", false
	}
	return token, true
}

func writeUnauthenticated(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", authScheme)
	w.WriteHeader(CodeUnauthenticated.HTTPStatus())
	_ = json.NewEncoder(w).Encode(NewErrorResponse(CodeUnauthenticated, unauthenticatedMessage))
}

// BearerTransport is an http.RoundTripper that presents the shared secret on
// every request it carries. A clipper builds its HTTP client on it:
//
//	client := &http.Client{Transport: &api.BearerTransport{Secret: secret}}
//
// It adds the header to every request, whatever the host, including requests
// that follow a redirect. Use the client only to talk to pogfarm.
type BearerTransport struct {
	// Secret is the shared secret. With an empty Secret every request fails
	// with ErrNoSecret before anything is sent.
	Secret string
	// Base carries the requests. Nil means http.DefaultTransport.
	Base http.RoundTripper
}

// RoundTrip sends a copy of req with the Authorization header set, replacing
// any the caller put there. req itself is not modified.
func (t *BearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.Secret == "" {
		// A RoundTripper must close the body even when it fails.
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, ErrNoSecret
	}
	out := req.Clone(req.Context())
	out.Header.Set(authHeader, authScheme+" "+t.Secret)
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(out)
}

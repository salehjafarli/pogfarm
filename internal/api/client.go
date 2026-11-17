package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"time"
)

// Defaults of the ClientOptions fields left zero.
const (
	// DefaultCallTimeout bounds one attempt of claim, heartbeat or fail.
	DefaultCallTimeout = 15 * time.Second
	// DefaultUploadTimeout bounds one attempt of complete, which carries the
	// whole clip.
	DefaultUploadTimeout = 10 * time.Minute
	// DefaultMaxAttempts is how often a retried call is sent in total.
	DefaultMaxAttempts = 4
	// DefaultRetryBaseDelay is the wait before the first retry. It doubles
	// with every further retry, up to DefaultRetryMaxDelay.
	DefaultRetryBaseDelay = 200 * time.Millisecond
	DefaultRetryMaxDelay  = 5 * time.Second
)

// maxResponseBytes caps how much of a response body the client reads. Every
// response of the contract is a small JSON document.
const maxResponseBytes = 1 << 20

// clipFileName is the file name sent with the clip part. pogfarm does not
// use it; it only marks the part as a file.
const clipFileName = "clip.mp4"

// ClientOptions configures a Client. BaseURL, Secret and Clipper are
// required; every other field has a default.
type ClientOptions struct {
	// BaseURL is where pogfarm listens, for example "http://pogfarm:8080".
	// The contract's paths are appended to it.
	BaseURL string
	// Secret is the shared secret, sent on every request.
	Secret string
	// Clipper is the caller's name, sent in every request. It identifies
	// the lease holder.
	Clipper string
	// Transport carries the requests. Nil means http.DefaultTransport.
	Transport http.RoundTripper
	// CallTimeout bounds one attempt of claim, heartbeat or fail. Zero
	// means DefaultCallTimeout.
	CallTimeout time.Duration
	// UploadTimeout bounds one attempt of complete. Zero means
	// DefaultUploadTimeout.
	UploadTimeout time.Duration
	// MaxAttempts is how often heartbeat, complete and fail are sent in
	// total when they keep failing with a network error or a 5xx status.
	// Zero means DefaultMaxAttempts; 1 disables retries.
	MaxAttempts int
	// RetryBaseDelay is the wait before the first retry; it doubles with
	// every further retry up to RetryMaxDelay. Zero means the defaults.
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
}

// Client calls the four operations of the job contract on pogfarm. It is
// safe for concurrent use, so heartbeats can run while an upload is in
// progress.
//
// Every method takes a context that cancels the call, including the wait
// between retries. Failures are reported as typed errors; see Error and the
// Err variables. A caller never needs to look at HTTP status codes.
//
// Heartbeat, Complete and Fail are retried with backoff after a network
// error, a timeout of one attempt or a 5xx status. A 4xx status is never
// retried. Claim is never retried: it is not idempotent, because a claim
// whose answer was lost has still handed out a job, and its caller asks
// again anyway.
//
// The client does not follow redirects, so the shared secret is never sent
// to a host other than the configured one. A redirect is reported as an
// *Error carrying the 3xx status.
type Client struct {
	base          string
	clipper       string
	http          *http.Client
	callTimeout   time.Duration
	uploadTimeout time.Duration
	maxAttempts   int
	retryBase     time.Duration
	retryMax      time.Duration
}

// NewClient returns a client for the pogfarm at opts.BaseURL. It fails with
// ErrNoSecret when the secret is empty, and with a descriptive error when
// the clipper name is empty or the URL is not an absolute http or https URL.
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.Secret == "" {
		return nil, ErrNoSecret
	}
	if opts.Clipper == "" {
		return nil, errors.New("api: no clipper name configured")
	}
	u, err := url.Parse(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("api: invalid base URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("api: base URL %q is not an absolute http or https URL", opts.BaseURL)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("api: base URL must not carry user info, a query or a fragment")
	}
	if opts.CallTimeout < 0 || opts.UploadTimeout < 0 || opts.MaxAttempts < 0 ||
		opts.RetryBaseDelay < 0 || opts.RetryMaxDelay < 0 {
		return nil, errors.New("api: client timeouts, delays and attempts must not be negative")
	}
	c := &Client{
		base:    strings.TrimRight(u.String(), "/"),
		clipper: opts.Clipper,
		http: &http.Client{
			Transport: &BearerTransport{Secret: opts.Secret, Base: opts.Transport},
			// Never follow a redirect: BearerTransport would present the
			// secret to wherever it points. The 3xx response is returned
			// as it is and reported as an error.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		callTimeout:   orDefault(opts.CallTimeout, DefaultCallTimeout),
		uploadTimeout: orDefault(opts.UploadTimeout, DefaultUploadTimeout),
		maxAttempts:   orDefault(opts.MaxAttempts, DefaultMaxAttempts),
		retryBase:     orDefault(opts.RetryBaseDelay, DefaultRetryBaseDelay),
		retryMax:      orDefault(opts.RetryMaxDelay, DefaultRetryMaxDelay),
	}
	if c.retryMax < c.retryBase {
		c.retryMax = c.retryBase
	}
	return c, nil
}

func orDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

// Claim asks pogfarm for one job. When there is nothing to do it returns a
// *NoJobError, which matches ErrNoJob and says how long to wait before
// asking again. Claim is not retried.
func (c *Client) Claim(ctx context.Context) (Job, error) {
	var resp ClaimResponse
	err := c.call(ctx, "claim", c.callTimeout, 1,
		c.jsonRequest(PathClaim, ClaimRequest{Clipper: c.clipper}), &resp)
	if err != nil {
		return Job{}, err
	}
	if resp.Job == nil {
		return Job{}, &NoJobError{RetryAfter: time.Duration(resp.RetryAfterSeconds) * time.Second}
	}
	if resp.Job.ID == "" {
		return Job{}, fmt.Errorf("api: claim: %w: job without an id", ErrBadResponse)
	}
	return *resp.Job, nil
}

// Heartbeat extends the lease of a job the caller holds and returns the new
// expiry. ErrLeaseLost, ErrAlreadyCompleted and ErrUnknownJob mean the
// caller abandons the job; any other error says nothing about the lease.
func (c *Client) Heartbeat(ctx context.Context, jobID string) (time.Time, error) {
	op := "heartbeat job " + jobID
	if jobID == "" {
		return time.Time{}, errEmptyJobID(op)
	}
	var resp HeartbeatResponse
	err := c.call(ctx, op, c.callTimeout, c.maxAttempts,
		c.jsonRequest(PathHeartbeat(url.PathEscape(jobID)), HeartbeatRequest{Clipper: c.clipper}), &resp)
	if err != nil {
		return time.Time{}, err
	}
	return resp.LeaseExpiresAt, nil
}

// Fail reports that the caller could not finish a job. reason is a short
// explanation for people; retryable says whether another attempt could
// succeed.
func (c *Client) Fail(ctx context.Context, jobID, reason string, retryable bool) error {
	op := "fail job " + jobID
	if jobID == "" {
		return errEmptyJobID(op)
	}
	req := FailRequest{Clipper: c.clipper, Reason: reason, Retryable: retryable}
	return c.call(ctx, op, c.callTimeout, c.maxAttempts,
		c.jsonRequest(PathFail(url.PathEscape(jobID)), req), nil)
}

// Complete uploads the finished clip at clipPath together with its metadata
// and transcript. The file is streamed from disk, never loaded into memory,
// and is opened again for every attempt.
//
// The caller fills meta, including the SHA-256 and size of the file;
// meta.Clipper is set to the client's clipper name. Both ResultAccepted and
// ResultAlreadyCompleted mean the job is done.
//
// A retry is safe because pogfarm keeps the first valid result. A
// checksum mismatch is a 4xx answer and is therefore not retried here; it is
// reported as ErrChecksumMismatch and the caller may upload again.
func (c *Client) Complete(ctx context.Context, jobID string, meta CompleteMetadata, transcript Transcript, clipPath string) (CompleteResult, error) {
	op := "complete job " + jobID
	if jobID == "" {
		return "", errEmptyJobID(op)
	}
	meta.Clipper = c.clipper
	if transcript.Words == nil {
		// The contract says an array, never null.
		transcript.Words = []Word{}
	}
	head, tail, contentType, err := completeEnvelope(meta, transcript)
	if err != nil {
		return "", fmt.Errorf("api: %s: %w", op, err)
	}
	target := c.base + PathComplete(url.PathEscape(jobID))
	newRequest := func(ctx context.Context) (*http.Request, error) {
		f, err := os.Open(clipPath)
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		body := struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), f, bytes.NewReader(tail)), f}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		// From here on the HTTP client closes the body, and with it the file.
		req.ContentLength = int64(len(head)) + info.Size() + int64(len(tail))
		req.Header.Set("Content-Type", contentType)
		return req, nil
	}
	var resp CompleteResponse
	if err := c.call(ctx, op, c.uploadTimeout, c.maxAttempts, newRequest, &resp); err != nil {
		return "", err
	}
	if resp.Result != ResultAccepted && resp.Result != ResultAlreadyCompleted {
		return "", fmt.Errorf("api: %s: %w: unknown result %q", op, ErrBadResponse, resp.Result)
	}
	return resp.Result, nil
}

// completeEnvelope builds everything of the multipart body except the clip
// bytes: head is what precedes them (the metadata and transcript parts and
// the header of the clip part), tail is what follows.
func completeEnvelope(meta CompleteMetadata, transcript Transcript) (head, tail []byte, contentType string, err error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, part := range []struct {
		name string
		v    any
	}{{PartMetadata, meta}, {PartTranscript, transcript}} {
		data, err := json.Marshal(part.v)
		if err != nil {
			return nil, nil, "", err
		}
		pw, err := w.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": {fmt.Sprintf(`form-data; name="%s"`, part.name)},
			"Content-Type":        {"application/json"},
		})
		if err != nil {
			return nil, nil, "", err
		}
		if _, err := pw.Write(data); err != nil {
			return nil, nil, "", err
		}
	}
	_, err = w.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="%s"; filename="%s"`, PartClip, clipFileName)},
		"Content-Type":        {ClipContentType},
	})
	if err != nil {
		return nil, nil, "", err
	}
	head = bytes.Clone(buf.Bytes())
	buf.Reset()
	if err := w.Close(); err != nil {
		return nil, nil, "", err
	}
	return head, bytes.Clone(buf.Bytes()), w.FormDataContentType(), nil
}

func errEmptyJobID(op string) error {
	return fmt.Errorf("api: %s: empty job id", strings.TrimSpace(op))
}

// requestFunc builds the request of one attempt. It is called again for
// every retry, so each attempt gets a fresh body.
type requestFunc func(ctx context.Context) (*http.Request, error)

// jsonRequest returns a requestFunc for a POST of v as JSON to path.
func (c *Client) jsonRequest(path string, v any) requestFunc {
	return func(ctx context.Context) (*http.Request, error) {
		data, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}
}

// call runs one operation: up to attempts exchanges, each bounded by
// timeout, with a growing wait between them. A successful answer is decoded
// into out unless out is nil. Every error is prefixed with op.
func (c *Client) call(ctx context.Context, op string, timeout time.Duration, attempts int, newRequest requestFunc, out any) error {
	for attempt := 1; ; attempt++ {
		retryable, err := c.exchange(ctx, timeout, newRequest, out)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			// The caller gave up; report that and nothing else.
			return fmt.Errorf("api: %s: %w", op, ctx.Err())
		}
		if !retryable || attempt >= attempts {
			return fmt.Errorf("api: %s: %w", op, err)
		}
		timer := time.NewTimer(c.backoff(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("api: %s: %w", op, ctx.Err())
		case <-timer.C:
		}
	}
}

// backoff returns the wait after the given failed attempt (1-based): the
// base delay doubled for every earlier retry, capped, with the lower half
// randomised so that clippers do not retry in step.
func (c *Client) backoff(attempt int) time.Duration {
	d := c.retryBase
	for i := 1; i < attempt && d < c.retryMax; i++ {
		d *= 2
	}
	if d > c.retryMax {
		d = c.retryMax
	}
	half := d / 2
	return half + rand.N(d-half+1)
}

// exchange sends one request and reads its answer. retryable reports
// whether sending the same request again could succeed.
func (c *Client) exchange(ctx context.Context, timeout time.Duration, newRequest requestFunc, out any) (retryable bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := newRequest(ctx)
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return true, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := newError(resp.StatusCode, body)
		return e.Transient(), e
	}
	if out == nil {
		return false, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return false, fmt.Errorf("%w: %v", ErrBadResponse, err)
	}
	return false, nil
}

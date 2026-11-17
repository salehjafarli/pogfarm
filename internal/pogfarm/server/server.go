// Package server is pogfarm's HTTP server: the four operations of the job
// contract (docs/api.md) behind the shared secret, and an unauthenticated
// health endpoint.
//
// The handlers only translate between HTTP and the queue and result intake;
// all job logic lives there.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/intake"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/queue"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// PathHealth is the health endpoint. It needs no secret.
const PathHealth = "/healthz"

// Defaults of the Options fields left zero.
const (
	// DefaultRetryAfter is what a claim with nothing to do tells the
	// clipper to wait before it asks again.
	DefaultRetryAfter = 10 * time.Second
	// DefaultUploadTimeout bounds reading a whole request and writing its
	// response. It must leave room for the largest clip upload, so it is
	// above the client's own upload timeout (api.DefaultUploadTimeout).
	DefaultUploadTimeout = 15 * time.Minute
	// DefaultShutdownTimeout is how long shutdown waits for requests in
	// flight, such as an upload, before it closes their connections.
	DefaultShutdownTimeout = 30 * time.Second
)

// ReadHeaderTimeout bounds reading the request line and headers.
const ReadHeaderTimeout = 10 * time.Second

// IdleTimeout is how long a keep-alive connection may sit unused.
const IdleTimeout = 2 * time.Minute

// MaxJSONBodyBytes caps the body of the claim, heartbeat and fail requests.
const MaxJSONBodyBytes = 64 << 10

// healthTimeout bounds the database check of the health endpoint.
const healthTimeout = 5 * time.Second

// Options configures a Server. Secret, Store, Queue and Intake are required.
type Options struct {
	// Secret is the shared secret every /v1 request must carry.
	Secret string
	// Store is checked by the health endpoint.
	Store *store.Store
	// Queue serves claim, heartbeat and fail.
	Queue *queue.Queue
	// Intake serves complete.
	Intake *intake.Intake
	// Logger receives one line per request and every internal error. Nil
	// discards them.
	Logger *slog.Logger
	// RetryAfter is sent with a claim that found nothing to do. It is
	// rounded up to whole seconds. Zero means DefaultRetryAfter.
	RetryAfter time.Duration
	// UploadTimeout bounds reading a whole request and writing its response.
	// Zero means DefaultUploadTimeout.
	UploadTimeout time.Duration
	// ShutdownTimeout is how long Serve waits for requests in flight once
	// its context is cancelled. Zero means DefaultShutdownTimeout.
	ShutdownTimeout time.Duration
}

// Server serves the job API. Build it with New and run it with Serve.
type Server struct {
	secret          string
	store           *store.Store
	queue           *queue.Queue
	intake          *intake.Intake
	logger          *slog.Logger
	retryAfter      int
	uploadTimeout   time.Duration
	shutdownTimeout time.Duration
	handler         http.Handler

	// onShutdown, when set, is called as soon as a shutdown begins. Tests
	// use it.
	onShutdown func()
}

// New builds a server from opts.
func New(opts Options) (*Server, error) {
	switch {
	case opts.Secret == "":
		return nil, errors.New("server: shared secret is empty")
	case opts.Store == nil:
		return nil, errors.New("server: store is nil")
	case opts.Queue == nil:
		return nil, errors.New("server: queue is nil")
	case opts.Intake == nil:
		return nil, errors.New("server: intake is nil")
	case opts.RetryAfter < 0 || opts.UploadTimeout < 0 || opts.ShutdownTimeout < 0:
		return nil, errors.New("server: durations must not be negative")
	}
	s := &Server{
		secret:          opts.Secret,
		store:           opts.Store,
		queue:           opts.Queue,
		intake:          opts.Intake,
		logger:          opts.Logger,
		uploadTimeout:   orDefault(opts.UploadTimeout, DefaultUploadTimeout),
		shutdownTimeout: orDefault(opts.ShutdownTimeout, DefaultShutdownTimeout),
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	retryAfter := orDefault(opts.RetryAfter, DefaultRetryAfter)
	s.retryAfter = int((retryAfter + time.Second - 1) / time.Second)
	s.handler = s.routes()
	return s, nil
}

func orDefault(v, def time.Duration) time.Duration {
	if v == 0 {
		return def
	}
	return v
}

// routes builds the handler tree. The secret is checked on everything under
// api.PathPrefix before routing, so an unknown /v1 path without the secret
// is answered 401, not 404, as the contract requires.
func (s *Server) routes() http.Handler {
	v1 := http.NewServeMux()
	v1.HandleFunc(api.RouteClaim, s.claim)
	v1.HandleFunc(api.RouteHeartbeat, s.heartbeat)
	v1.Handle(api.RouteComplete, s.intake)
	v1.HandleFunc(api.RouteFail, s.fail)

	root := http.NewServeMux()
	root.Handle(api.PathPrefix+"/", api.RequireSecret(s.secret, v1))
	root.HandleFunc("GET "+PathHealth, s.health)
	return s.logRequests(root)
}

// Handler returns the server's handler, for tests that serve it themselves.
func (s *Server) Handler() http.Handler { return s.handler }

// Serve serves the job API on ln until ctx is cancelled, then shuts down
// gracefully: it stops accepting connections and waits up to the shutdown
// timeout for requests in flight, such as an upload, to finish. Requests
// still running after that are cut off and Serve reports it.
//
// Serve closes ln. It returns nil after a clean shutdown and an error when
// the listener fails or the shutdown timed out.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: ReadHeaderTimeout,
		// Read and write timeouts cover the whole request and response,
		// including the body, so they are generous enough for the largest
		// clip upload instead of short.
		ReadTimeout:  s.uploadTimeout,
		WriteTimeout: s.uploadTimeout,
		IdleTimeout:  IdleTimeout,
		ErrorLog:     slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
		// Requests run on their own context, not on ctx, so that cancelling
		// ctx lets uploads in flight finish instead of aborting them.
		BaseContext: func(net.Listener) context.Context { return context.Background() },
	}
	if s.onShutdown != nil {
		srv.RegisterOnShutdown(s.onShutdown)
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	select {
	case err := <-served:
		return fmt.Errorf("server: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()
	err := srv.Shutdown(shutdownCtx)
	if err != nil {
		// Requests are still running; cut them off.
		_ = srv.Close()
		err = fmt.Errorf("server: shutdown: requests still running after %s: %w", s.shutdownTimeout, err)
	}
	if serveErr := <-served; !errors.Is(serveErr, http.ErrServerClosed) && err == nil {
		err = fmt.Errorf("server: %w", serveErr)
	}
	return err
}

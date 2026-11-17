// Command pogfarm is the main service: it runs once, on the main node.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/intake"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/lock"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/queue"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/review"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/server"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", "", "config file (default: the path in $"+config.EnvConfigPath+")")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("pogfarm %s\n", version)
		return
	}

	cfg, err := config.LoadPogfarm(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pogfarm:", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("pogfarm starting", "version", version)
	if err := run(ctx, cfg, logger, review.DefaultBaseURL); err != nil {
		logger.Error("pogfarm failed", "err", err)
		os.Exit(1)
	}
	logger.Info("pogfarm stopped")
}

// run starts every background loop of the service and returns once ctx is
// cancelled and all of them have stopped. telegramURL is the Bot API root.
//
// It fails at startup when another pogfarm holds the data directory, the
// database cannot be opened or the listen address is unusable, and later
// when the job API server or the reaper fails, or the server cannot shut
// down in time.
func run(ctx context.Context, cfg *config.Pogfarm, logger *slog.Logger, telegramURL string) (err error) {
	// Exactly one pogfarm per data directory. The lock is taken before
	// anything in the directory is touched and released last.
	dirLock, err := lock.Acquire(cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dirLock.Release()) }()

	// Core: the store and the queue, opened once and shared by every loop
	// and handler. The store is closed only after every loop has stopped.
	// Opening is short and not interrupted by a stop signal: a stop during
	// startup lets startup finish and then stops the loops at once.
	st, q, err := openCore(context.WithoutCancel(ctx), cfg)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()

	// Job API: built and bound before any loop starts, so that a bad listen
	// address fails startup.
	jobAPI, ln, err := newJobAPI(cfg, st, q, logger)
	if err != nil {
		return err
	}

	// Every loop stops when ctx is cancelled, or when a loop below fails.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var loops sync.WaitGroup

	if cfg.Telegram.Enabled() {
		client := review.NewClient(cfg.Telegram.BotToken)
		client.BaseURL = telegramURL
		bot := review.NewBot(client, cfg.Telegram.OwnerChatID, logger)
		loops.Go(func() { bot.Run(ctx) })
		logger.Info("telegram bot started", "owner_chat_id", cfg.Telegram.OwnerChatID)
	} else {
		logger.Warn("telegram bot is off: pogfarm.telegram is not configured")
	}

	// Job API server. Cancelling ctx shuts it down gracefully: requests in
	// flight, such as uploads, may finish within its shutdown timeout.
	var serveErr error
	loops.Go(func() {
		if serveErr = jobAPI.Serve(ctx, ln); serveErr != nil {
			cancel()
		}
	})
	logger.Info("job API listening", "addr", ln.Addr().String())

	// Lease reaper: takes back jobs whose clipper stopped sending
	// heartbeats.
	var reaperErr error
	loops.Go(func() {
		if reaperErr = q.RunReaper(ctx, cfg.Queue.ReaperInterval, logger); reaperErr != nil {
			cancel()
		}
	})

	<-ctx.Done()
	loops.Wait()
	return errors.Join(serveErr, reaperErr)
}

// openCore opens the database in the data directory and builds the job
// queue on it from the configuration. The caller closes the store.
func openCore(ctx context.Context, cfg *config.Pogfarm) (*store.Store, *queue.Queue, error) {
	st, err := store.Open(ctx, store.Path(cfg.DataDir))
	if err != nil {
		return nil, nil, err
	}
	q, err := queue.New(st, queue.Options{
		LeaseDuration: cfg.Queue.LeaseDuration,
		MaxAttempts:   cfg.Queue.MaxAttempts,
	})
	if err != nil {
		_ = st.Close()
		return nil, nil, err
	}
	return st, q, nil
}

// newJobAPI builds the job API server, with the result intake behind its
// complete route, and binds its listen address.
func newJobAPI(cfg *config.Pogfarm, st *store.Store, q *queue.Queue, logger *slog.Logger) (*server.Server, net.Listener, error) {
	in, err := intake.New(st, intake.Options{
		DataDir:      cfg.DataDir,
		MaxClipBytes: cfg.MaxClipBytes,
		Logger:       logger,
	})
	if err != nil {
		return nil, nil, err
	}
	srv, err := server.New(server.Options{
		Secret: cfg.APISecret.Reveal(),
		Store:  st,
		Queue:  q,
		Intake: in,
		Logger: logger,
	})
	if err != nil {
		return nil, nil, err
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("job API: %w", err)
	}
	return srv, ln, nil
}

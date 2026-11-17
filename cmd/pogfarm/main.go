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
	"github.com/salehjafarli/pogfarm/internal/pogfarm/scan"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/server"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source/twitch"
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

	// Stream platforms, built before the data directory is used so that
	// bad platform settings fail startup early.
	sources, err := newSources(cfg)
	if err != nil {
		return err
	}

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
	jobAPI, ln, cleaner, err := newJobAPI(cfg, st, q, logger)
	if err != nil {
		return err
	}

	// Uploads that were in flight when pogfarm last stopped are deleted
	// before the job API accepts new ones.
	if removed, freed, err := cleaner.EmptyTemp(); err != nil {
		_ = ln.Close()
		return err
	} else if removed > 0 {
		logger.Info("deleted unfinished uploads", "files", removed, "freed_bytes", freed)
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

		// Review sender: posts each clip awaiting review to the owner's chat.
		sender := review.NewSender(client, st, cfg.Telegram.OwnerChatID, cfg.DataDir, logger)
		loops.Go(func() { sender.Run(ctx) })
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

	// Clip file cleanup: deletes the files of rejected clips and of clips
	// posted everywhere once their retention has passed, and orphaned files.
	var cleanupErr error
	loops.Go(func() {
		if cleanupErr = cleaner.Run(ctx, cfg.Cleanup.Interval); cleanupErr != nil {
			cancel()
		}
	})

	// Discovery and scanning: find new VODs of allowlisted streamers and
	// turn their chat into render jobs. Off without Twitch credentials.
	startScanning(ctx, cfg, st, q, sources, logger, &loops)

	<-ctx.Done()
	loops.Wait()
	return errors.Join(serveErr, reaperErr, cleanupErr)
}

// newSources builds the registry of stream platforms from the
// configuration. It is nil when no platform is configured, which turns
// discovery and scanning off.
func newSources(cfg *config.Pogfarm) (*source.Registry, error) {
	if cfg.Twitch.ClientID == "" || !cfg.Twitch.ClientSecret.IsSet() {
		return nil, nil
	}
	tw, err := twitch.NewSourceFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	return source.NewRegistry(tw)
}

// startScanning starts the discovery and scanner loops on loops, sharing the
// store and queue with the rest of the service. With no sources both loops
// stay off.
func startScanning(ctx context.Context, cfg *config.Pogfarm, st *store.Store, q *queue.Queue, sources *source.Registry, logger *slog.Logger, loops *sync.WaitGroup) {
	if sources == nil {
		logger.Warn("discovery and scanning are off: pogfarm.twitch is not configured")
		return
	}
	discoverer := &scan.Discoverer{
		Store:     st,
		Sources:   sources,
		Allowlist: cfg.Streamers,
		MaxVODAge: cfg.Discovery.MaxVODAge,
		Interval:  cfg.Discovery.Interval,
		Logger:    logger.With("loop", "discovery"),
	}
	scanner := scan.NewScanner(st, q, sources, cfg.Scan, logger.With("loop", "scanner"))
	loops.Go(func() {
		if err := discoverer.Run(ctx); err != nil {
			logger.Error("discovery loop stopped", "error", err)
		}
	})
	loops.Go(func() {
		if err := scanner.Run(ctx); err != nil {
			logger.Error("scanner loop stopped", "error", err)
		}
	})
	logger.Info("discovery and scanner started", "streamers", len(cfg.Streamers),
		"discovery_interval", cfg.Discovery.Interval, "scan_workers", cfg.Scan.Workers)
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
// complete route, and binds its listen address. It also returns the cleaner
// of the clip files, which must belong to the same intake: it removes
// orphaned files under the lock that intake holds while it stores a result.
func newJobAPI(cfg *config.Pogfarm, st *store.Store, q *queue.Queue, logger *slog.Logger) (*server.Server, net.Listener, *intake.Cleaner, error) {
	in, err := intake.New(st, intake.Options{
		DataDir:      cfg.DataDir,
		MaxClipBytes: cfg.MaxClipBytes,
		Logger:       logger,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	srv, err := server.New(server.Options{
		Secret: cfg.APISecret.Reveal(),
		Store:  st,
		Queue:  q,
		Intake: in,
		Logger: logger,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	cleaner, err := in.NewCleaner(intake.CleanupOptions{
		RejectedRetention: cfg.Cleanup.RejectedRetention,
		PostedRetention:   cfg.Cleanup.PostedRetention,
		Logger:            logger,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("job API: %w", err)
	}
	return srv, ln, cleaner, nil
}

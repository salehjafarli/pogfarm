// Command pogfarm is the main service: it runs once, on the main node.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/queue"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/review"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/scan"
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
		logger.Error("pogfarm failed to start", "error", err)
		os.Exit(1)
	}
	logger.Info("pogfarm stopped")
}

// run starts every background loop of the service and returns once ctx is
// cancelled and all of them have stopped. telegramURL is the Bot API root.
// It returns an error, without starting anything, when a loop cannot be set
// up.
func run(ctx context.Context, cfg *config.Pogfarm, logger *slog.Logger, telegramURL string) error {
	var loops sync.WaitGroup

	closeScanning, err := startScanning(ctx, cfg, logger, &loops)
	if err != nil {
		return err
	}
	// Deferred, so the store is closed only after loops.Wait below.
	defer closeScanning()

	if cfg.Telegram.Enabled() {
		client := review.NewClient(cfg.Telegram.BotToken)
		client.BaseURL = telegramURL
		bot := review.NewBot(client, cfg.Telegram.OwnerChatID, logger)
		loops.Go(func() { bot.Run(ctx) })
		logger.Info("telegram bot started", "owner_chat_id", cfg.Telegram.OwnerChatID)
	} else {
		logger.Warn("telegram bot is off: pogfarm.telegram is not configured")
	}

	<-ctx.Done()
	loops.Wait()
	return nil
}

// startScanning opens the store and starts the discovery and scanner loops
// on loops, when the Twitch credentials are set; without them there is no
// source to scan and both loops stay off. The returned function closes the
// store and must be called once the loops have stopped.
func startScanning(ctx context.Context, cfg *config.Pogfarm, logger *slog.Logger, loops *sync.WaitGroup) (func(), error) {
	if cfg.Twitch.ClientID == "" || !cfg.Twitch.ClientSecret.IsSet() {
		logger.Warn("discovery and scanning are off: pogfarm.twitch is not configured")
		return func() {}, nil
	}
	tw, err := twitch.NewSourceFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	sources, err := source.NewRegistry(tw)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(ctx, store.Path(cfg.DataDir))
	if err != nil {
		return nil, err
	}
	// The reaper is not started here; it belongs with the job API.
	q, err := queue.New(st, queue.Options{LeaseDuration: cfg.Queue.LeaseDuration, MaxAttempts: cfg.Queue.MaxAttempts})
	if err != nil {
		_ = st.Close()
		return nil, err
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
	return func() {
		if err := st.Close(); err != nil {
			logger.Error("closing the store failed", "error", err)
		}
	}, nil
}

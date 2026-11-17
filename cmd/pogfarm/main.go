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
	"github.com/salehjafarli/pogfarm/internal/pogfarm/review"
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
	run(ctx, cfg, logger, review.DefaultBaseURL)
	logger.Info("pogfarm stopped")
}

// run starts every background loop of the service and returns once ctx is
// cancelled and all of them have stopped. telegramURL is the Bot API root.
func run(ctx context.Context, cfg *config.Pogfarm, logger *slog.Logger, telegramURL string) {
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

	<-ctx.Done()
	loops.Wait()
}

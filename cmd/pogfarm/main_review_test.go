package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/config"
)

// The /status command is registered on the bot that run starts, and answers
// from the shared store.
func TestRunAnswersStatusCommand(t *testing.T) {
	var polls atomic.Int32
	sent := make(chan map[string]any, 4)
	shutdown := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/bot" + testToken + "/getUpdates":
			if polls.Add(1) == 1 {
				_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":1,"message":
					{"message_id":1,"date":1700000000,"text":"/status","chat":{"id":4242,"type":"private"}}}]}`)
				return
			}
			select {
			case <-r.Context().Done():
			case <-shutdown:
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		case "/bot" + testToken + "/sendMessage":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			sent <- body
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":2,"date":1700000001,"chat":{"id":4242,"type":"private"}}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":404,"description":"Not Found"}`)
		}
	}))
	defer srv.Close()
	defer close(shutdown)

	cfg := testConfig(t)
	cfg.Telegram = config.Telegram{BotToken: config.NewSecret(testToken), OwnerChatID: ownerChat}
	stop := start(t, cfg, newLogBuffer(), srv.URL)

	select {
	case body := <-sent:
		text, _ := body["text"].(string)
		if !strings.Contains(text, "Jobs: 0 queued") || body["chat_id"] != float64(ownerChat) {
			t.Errorf("unexpected /status reply: %v", body)
		}
	case <-time.After(waitLimit):
		t.Fatal("no answer to /status")
	}
	if err := stop(); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// An enabled upload target with no implementation stops startup, before
// anything is started.
func TestRunRefusesUnimplementedUploadTarget(t *testing.T) {
	cfg := testConfig(t)
	cfg.Upload.Targets = []config.UploadTarget{{Name: "youtube", DailyCap: 1}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := run(ctx, cfg, slog.New(slog.DiscardHandler), "http://127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "youtube") {
		t.Fatalf("run = %v, want an error naming the target", err)
	}
	// Nothing was left holding the data directory: a second run on it, set
	// up to stop at once, starts and stops cleanly.
	cfg.Upload.Targets = nil
	cancel()
	if err := run(ctx, cfg, slog.New(slog.DiscardHandler), "http://127.0.0.1:1"); err != nil {
		t.Errorf("second run: %v", err)
	}
}

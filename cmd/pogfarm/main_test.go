package main

import (
	"bytes"
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

const (
	// testToken has the shape of a real token but belongs to no bot.
	testToken = "123456789:TEST-token-that-must-never-be-logged"
	ownerChat = 4242
	waitLimit = 5 * time.Second
)

// TestBotStartsWithPogfarm runs the service against a local stand-in for the
// Bot API: the owner's /ping is answered with pong, and cancelling the context
// stops everything.
func TestBotStartsWithPogfarm(t *testing.T) {
	var polls atomic.Int32
	sent := make(chan map[string]any, 4)
	shutdown := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/bot" + testToken + "/getUpdates":
			if polls.Add(1) == 1 {
				_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":1,"message":
					{"message_id":1,"date":1700000000,"text":"/ping","chat":{"id":4242,"type":"private"}}}]}`)
				return
			}
			// An idle long poll.
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

	cfg := &config.Pogfarm{Telegram: config.Telegram{
		BotToken:    config.NewSecret(testToken),
		OwnerChatID: ownerChat,
	}}
	var log bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx, cfg, slog.New(slog.NewTextHandler(&log, nil)), srv.URL)
	}()

	select {
	case body := <-sent:
		if body["text"] != "pong" || body["chat_id"] != float64(ownerChat) {
			t.Errorf("unexpected reply: %v", body)
		}
	case <-time.After(waitLimit):
		t.Fatal("no answer to /ping")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(waitLimit):
		t.Fatal("run did not return after cancellation")
	}
	if out := log.String(); strings.Contains(out, testToken) || strings.Contains(out, "TEST-token") {
		t.Errorf("the log leaks the token:\n%s", out)
	}
}

func TestRunWithoutTelegram(t *testing.T) {
	var log bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// An address nothing listens on: the bot must not be started.
		run(ctx, &config.Pogfarm{}, slog.New(slog.NewTextHandler(&log, nil)), "http://127.0.0.1:1")
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(waitLimit):
		t.Fatal("run did not return after cancellation")
	}
	if out := log.String(); !strings.Contains(out, "telegram bot is off") || strings.Contains(out, "poll failed") {
		t.Errorf("unexpected log:\n%s", out)
	}
}

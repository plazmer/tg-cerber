package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"tg_block/internal/bot"
	"tg_block/internal/config"
	"tg_block/internal/store"
	"tg_block/internal/telegram"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{}))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("load config failed", "error", err)
		os.Exit(1)
	}

	repo, err := store.NewSQLiteRepository(cfg.DBPath)
	if err != nil {
		logger.Error("init store failed", "error", err)
		os.Exit(1)
	}

	tg := telegram.NewClient(cfg.BotToken)
	service := bot.NewService(cfg, repo, tg, logger)
	if err := service.Bootstrap(context.Background()); err != nil {
		logger.Error("bootstrap bot failed", "error", err)
		os.Exit(1)
	}

	webhookURL := fmt.Sprintf("%s/%s", strings.TrimSuffix(cfg.WebhookPath, "/"), cfg.WebhookSecret)
	publicWebhookURL := strings.TrimSpace(os.Getenv("WEBHOOK_PUBLIC_URL"))
	if publicWebhookURL == "" {
		baseURL := strings.TrimSuffix(strings.TrimSpace(os.Getenv("WEBHOOK_BASE_URL")), "/")
		if baseURL != "" {
			publicWebhookURL = baseURL + webhookURL
		}
	}
	if publicWebhookURL != "" {
		if err := tg.SetWebhook(context.Background(), publicWebhookURL, cfg.WebhookSecret); err != nil {
			logger.Error("set webhook failed", "error", err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc(webhookURL, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if header := r.Header.Get("X-Telegram-Bot-Api-Secret-Token"); header != "" && header != cfg.WebhookSecret {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		defer r.Body.Close()
		var update telegram.Update
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := service.HandleUpdate(r.Context(), update); err != nil {
			logger.Error("handle update failed", "error", err, "update_id", update.UpdateID)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	go func() {
		logger.Info("http server started", "addr", cfg.ListenAddr, "webhook_path", webhookURL)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}

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
	"sync"
	"syscall"
	"time"

	"tg_block/internal/bot"
	"tg_block/internal/config"
	"tg_block/internal/store"
	"tg_block/internal/telegram"
)

const (
	updateQueueSize   = 1024
	updateWorkers     = 4
	updateTimeout     = 3 * time.Minute
	enqueueTimeout    = 5 * time.Second
	shutdownTimeout   = 15 * time.Second
	bootstrapMaxDelay = time.Minute
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{}))
	if err := run(logger); err != nil {
		logger.Error("bot stopped with error", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	repo, err := store.NewSQLiteRepository(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}
	defer func() {
		if err := repo.Close(); err != nil {
			logger.Error("close store failed", "error", err)
		}
	}()

	tg := telegram.NewClient(cfg.BotToken, logger)
	service := bot.NewService(cfg, repo, tg, logger)

	// Раньше неудачный getMe завершал процесс, а супервизор тут же поднимал его
	// заново — получался цикл рестартов, добивавший 429. Теперь ждем с backoff.
	if err := bootstrap(rootCtx, service, logger); err != nil {
		return fmt.Errorf("bootstrap bot: %w", err)
	}
	if me := service.BotUser(); me != nil {
		logger.Info("bot authorized", "bot_id", me.ID, "username", me.Username)
	}

	webhookPath := fmt.Sprintf("%s/%s", strings.TrimSuffix(cfg.WebhookPath, "/"), cfg.WebhookSecret)
	if publicURL := publicWebhookURL(webhookPath); publicURL != "" {
		if err := tg.SetWebhook(rootCtx, publicURL, cfg.WebhookSecret); err != nil {
			logger.Error("set webhook failed", "error", err)
		} else {
			logger.Info("webhook registered", "url", publicURL)
		}
	}

	updates := make(chan telegram.Update, updateQueueSize)
	var workers sync.WaitGroup
	for i := 0; i < updateWorkers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for update := range updates {
				processUpdate(service, logger, update)
			}
		}()
	}

	go service.Run(rootCtx)

	mux := http.NewServeMux()
	mux.HandleFunc(webhookPath, func(w http.ResponseWriter, r *http.Request) {
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

		// Апдейт кладем в очередь и сразу отвечаем 200: обработка может ждать
		// retry_after, а задержанный ответ заставил бы Telegram переслать апдейт.
		enqueueCtx, cancel := context.WithTimeout(r.Context(), enqueueTimeout)
		defer cancel()
		select {
		case updates <- update:
		case <-enqueueCtx.Done():
			// Очередь переполнена: просим Telegram повторить позже,
			// повтор отсеется журналом update_id.
			logger.Error("update queue is full", "update_id", update.UpdateID)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server started", "addr", cfg.ListenAddr, "webhook_path", webhookPath)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	var runErr error
	select {
	case <-rootCtx.Done():
		logger.Info("shutdown signal received")
	case err := <-serverErr:
		runErr = fmt.Errorf("http server: %w", err)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}

	// Сервер больше не принимает запросы, значит в очередь никто не пишет.
	close(updates)
	drained := make(chan struct{})
	go func() {
		workers.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(shutdownTimeout):
		logger.Error("update workers did not finish in time")
	}

	return runErr
}

func processUpdate(service *bot.Service, logger *slog.Logger, update telegram.Update) {
	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()
	if err := service.HandleUpdate(ctx, update); err != nil {
		logger.Error("handle update failed", "error", err, "update_id", update.UpdateID)
	}
}

func bootstrap(ctx context.Context, service *bot.Service, logger *slog.Logger) error {
	delay := time.Second
	for attempt := 1; ; attempt++ {
		err := service.Bootstrap(ctx)
		if err == nil {
			return nil
		}
		if telegram.IsAuthError(err) {
			return fmt.Errorf("telegram rejected the token: %w", err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		logger.Error("bootstrap attempt failed", "attempt", attempt, "wait", delay.String(), "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
		if delay > bootstrapMaxDelay {
			delay = bootstrapMaxDelay
		}
	}
}

func publicWebhookURL(webhookPath string) string {
	if publicURL := strings.TrimSpace(os.Getenv("WEBHOOK_PUBLIC_URL")); publicURL != "" {
		return publicURL
	}
	baseURL := strings.TrimSuffix(strings.TrimSpace(os.Getenv("WEBHOOK_BASE_URL")), "/")
	if baseURL == "" {
		return ""
	}
	return baseURL + webhookPath
}

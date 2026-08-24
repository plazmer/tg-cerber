package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	BotToken      string
	OwnerID       int64
	WebhookSecret string
	WebhookPath   string
	ListenAddr    string
	DBPath        string
	// PendingTTL — через сколько нерассмотренная заявка отклоняется автоматически.
	// 0 (по умолчанию) — авто-отклонение выключено, заявка ждет решения владельца.
	PendingTTL time.Duration
}

func Load() (Config, error) {
	botToken, err := required("BOT_TOKEN")
	if err != nil {
		return Config{}, err
	}
	ownerValue, err := required("OWNER_ID")
	if err != nil {
		return Config{}, err
	}
	webhookSecret, err := required("WEBHOOK_SECRET")
	if err != nil {
		return Config{}, err
	}

	ownerID, err := strconv.ParseInt(ownerValue, 10, 64)
	if err != nil {
		return Config{}, fmt.Errorf("parse OWNER_ID: %w", err)
	}

	pendingTTL, err := optionalDuration("PENDING_TTL", 0)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		BotToken:      botToken,
		OwnerID:       ownerID,
		WebhookSecret: webhookSecret,
		WebhookPath:   optional("WEBHOOK_PATH", "/webhook"),
		ListenAddr:    optional("LISTEN_ADDR", ":8080"),
		DBPath:        optional("DB_PATH", "bot.db"),
		PendingTTL:    pendingTTL,
	}

	return cfg, nil
}

func required(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("env %s is required", name)
	}
	return value, nil
}

func optional(name string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func optionalDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("parse %s: must not be negative", name)
	}
	return parsed, nil
}

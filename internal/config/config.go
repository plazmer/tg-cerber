package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	BotToken        string
	OwnerID         int64
	WebhookSecret   string
	WebhookPath     string
	ListenAddr      string
	DBPath          string
	BotQuestion     string
	GroupPromptText string
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

	cfg := Config{
		BotToken:        botToken,
		OwnerID:         ownerID,
		WebhookSecret:   webhookSecret,
		WebhookPath:     optional("WEBHOOK_PATH", "/webhook"),
		ListenAddr:      optional("LISTEN_ADDR", ":8080"),
		DBPath:          optional("DB_PATH", "bot.db"),
		BotQuestion:     optional("BOT_QUESTION", "Здравствуйте! Напишите, пожалуйста, зачем вы вступили в группу."),
		GroupPromptText: optional("GROUP_PROMPT_TEXT", "Для доступа к чату начните диалог с ботом: %s"),
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

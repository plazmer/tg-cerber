package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Таймаут одной HTTP-попытки. Общий дедлайн логического вызова задается callTimeout.
	defaultAttemptTimeout = 15 * time.Second
	// Общий дедлайн вызова со всеми ретраями и ожиданием retry_after.
	defaultCallTimeout = 90 * time.Second
	defaultMaxAttempts = 5

	// Глобальный лимит Bot API ~30 rps, берем с запасом.
	defaultGlobalRPS   = 25
	defaultGlobalBurst = 25
	// Лимит на отправку в один чат ~1 msg/s.
	defaultChatRPS   = 1
	defaultChatBurst = 2

	backoffBase = 500 * time.Millisecond
	backoffMax  = 30 * time.Second
)

// APIError описывает ответ Telegram с ok=false либо нештатный HTTP-статус.
type APIError struct {
	Method      string
	StatusCode  int
	ErrorCode   int
	Description string
	RetryAfter  time.Duration
}

func (e *APIError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("telegram api error %s: code=%d retry_after=%s: %s", e.Method, e.Code(), e.RetryAfter, e.Description)
	}
	return fmt.Sprintf("telegram api error %s: code=%d: %s", e.Method, e.Code(), e.Description)
}

// Code возвращает код ошибки Telegram, а при его отсутствии — HTTP-статус.
func (e *APIError) Code() int {
	if e.ErrorCode != 0 {
		return e.ErrorCode
	}
	return e.StatusCode
}

func (e *APIError) retryable() bool {
	code := e.Code()
	return code == http.StatusTooManyRequests || code >= 500
}

// IsRateLimit сообщает, что вызов уперся в 429.
func IsRateLimit(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code() == http.StatusTooManyRequests
}

// IsAuthError выделяет ошибки токена: ретраить их бессмысленно.
func IsAuthError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	code := apiErr.Code()
	return code == http.StatusUnauthorized || code == http.StatusNotFound
}

// IsForbidden — бот не может писать пользователю (не нажат Start либо бот заблокирован).
func IsForbidden(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code() == http.StatusForbidden
}

// limiter — token bucket. Своя реализация, чтобы не добавлять зависимость.
type limiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newLimiter(rate float64, burst float64) *limiter {
	return &limiter{rate: rate, burst: burst, tokens: burst, last: time.Now()}
}

// reserve занимает токен и возвращает, сколько нужно подождать перед вызовом.
func (l *limiter) reserve() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	if elapsed := now.Sub(l.last).Seconds(); elapsed > 0 {
		l.tokens += elapsed * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.last = now
	}
	l.tokens--
	if l.tokens >= 0 {
		return 0
	}
	return time.Duration(-l.tokens / l.rate * float64(time.Second))
}

func (l *limiter) wait(ctx context.Context) error {
	delay := l.reserve()
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	return sleepCtx(ctx, delay)
}

type Client struct {
	baseURL    string
	httpClient *http.Client
	logger     *slog.Logger

	attemptTimeout time.Duration
	callTimeout    time.Duration
	maxAttempts    int

	global     *limiter
	chatMu     sync.Mutex
	chatLimits map[int64]*limiter
}

func NewClient(token string, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &Client{
		baseURL: fmt.Sprintf("https://api.telegram.org/bot%s", token),
		httpClient: &http.Client{
			Timeout: defaultAttemptTimeout,
		},
		logger:         logger,
		attemptTimeout: defaultAttemptTimeout,
		callTimeout:    defaultCallTimeout,
		maxAttempts:    defaultMaxAttempts,
		global:         newLimiter(defaultGlobalRPS, defaultGlobalBurst),
		chatLimits:     make(map[int64]*limiter),
	}
}

// NewClientWithEndpoint создает клиента с произвольным адресом Bot API:
// нужен для тестов и для локального сервера Bot API.
func NewClientWithEndpoint(baseURL string, logger *slog.Logger) *Client {
	client := NewClient("", logger)
	client.baseURL = strings.TrimSuffix(baseURL, "/")
	return client
}

func (c *Client) chatLimiter(chatID int64) *limiter {
	c.chatMu.Lock()
	defer c.chatMu.Unlock()

	l, ok := c.chatLimits[chatID]
	if !ok {
		l = newLimiter(defaultChatRPS, defaultChatBurst)
		c.chatLimits[chatID] = l
	}
	return l
}

func (c *Client) ApproveChatJoinRequest(ctx context.Context, chatID int64, userID int64) error {
	return c.call(ctx, "approveChatJoinRequest", map[string]any{
		"chat_id": chatID,
		"user_id": userID,
	}, nil)
}

func (c *Client) DeclineChatJoinRequest(ctx context.Context, chatID int64, userID int64) error {
	return c.call(ctx, "declineChatJoinRequest", map[string]any{
		"chat_id": chatID,
		"user_id": userID,
	}, nil)
}

func (c *Client) Ban(ctx context.Context, chatID int64, userID int64) error {
	return c.call(ctx, "banChatMember", map[string]any{
		"chat_id":         chatID,
		"user_id":         userID,
		"revoke_messages": true,
		"only_if_banned":  false,
	}, nil)
}

func (c *Client) UnbanChatMember(ctx context.Context, chatID int64, userID int64) error {
	return c.call(ctx, "unbanChatMember", map[string]any{
		"chat_id":        chatID,
		"user_id":        userID,
		"only_if_banned": true,
	}, nil)
}

// ClearReplyMarkup убирает inline-клавиатуру при editMessageText (пустой inline_keyboard).
func ClearReplyMarkup() map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{}}
}

func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, markup map[string]any) error {
	_, err := c.SendMessageWithResult(ctx, chatID, text, markup)
	return err
}

func (c *Client) SendMessageWithResult(ctx context.Context, chatID int64, text string, markup map[string]any) (*Message, error) {
	payload := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if markup != nil {
		payload["reply_markup"] = markup
	}
	var message Message
	if err := c.callChat(ctx, "sendMessage", chatID, payload, &message); err != nil {
		return nil, err
	}
	return &message, nil
}

func (c *Client) AnswerCallback(ctx context.Context, callbackID string, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{
		"callback_query_id": callbackID,
		"text":              text,
		"show_alert":        false,
	}, nil)
}

func (c *Client) EditMessageText(ctx context.Context, chatID int64, messageID int64, text string, markup map[string]any) error {
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	}
	if markup != nil {
		payload["reply_markup"] = markup
	}
	return c.callChat(ctx, "editMessageText", chatID, payload, nil)
}

func (c *Client) GetMe(ctx context.Context) (*User, error) {
	var response struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	if err := c.call(ctx, "getMe", map[string]any{}, &response); err != nil {
		return nil, err
	}
	return &User{ID: response.ID, Username: response.Username}, nil
}

func (c *Client) SetWebhook(ctx context.Context, webhookURL string, secret string) error {
	return c.call(ctx, "setWebhook", map[string]any{
		"url":             webhookURL,
		"secret_token":    secret,
		"allowed_updates": []string{"message", "callback_query", "chat_member", "chat_join_request"},
	}, nil)
}

func (c *Client) call(ctx context.Context, method string, payload map[string]any, out any) error {
	return c.callChat(ctx, method, 0, payload, out)
}

// callChat выполняет вызов с учетом лимитов, ретраев и retry_after.
// chatID != 0 включает дополнительный per-chat лимитер (для методов отправки).
func (c *Client) callChat(ctx context.Context, method string, chatID int64, payload map[string]any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	deadline := time.Now().Add(c.callTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if err := c.global.wait(ctx); err != nil {
			return fmt.Errorf("%s: wait global limiter: %w", method, err)
		}
		if chatID != 0 {
			if err := c.chatLimiter(chatID).wait(ctx); err != nil {
				return fmt.Errorf("%s: wait chat limiter: %w", method, err)
			}
		}

		attemptCtx, cancel := context.WithTimeout(ctx, c.attemptTimeout)
		callErr := c.do(attemptCtx, method, body, out)
		cancel()
		if callErr == nil {
			return nil
		}
		lastErr = callErr

		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", method, ctx.Err())
		}

		wait := time.Duration(0)
		retryable := true
		var apiErr *APIError
		if errors.As(callErr, &apiErr) {
			retryable = apiErr.retryable()
			wait = apiErr.RetryAfter
		}
		if !retryable || attempt == c.maxAttempts {
			break
		}
		if wait <= 0 {
			wait = backoffDelay(attempt)
		}
		if time.Now().Add(wait).After(deadline) {
			c.logger.Warn("telegram retry budget exhausted", "method", method, "attempt", attempt, "wait", wait.String(), "error", callErr)
			break
		}

		c.logger.Warn("telegram call failed, retrying", "method", method, "attempt", attempt, "wait", wait.String(), "rate_limited", IsRateLimit(callErr), "error", callErr)
		if err := sleepCtx(ctx, wait); err != nil {
			return fmt.Errorf("%s: %w", method, err)
		}
	}

	return lastErr
}

func (c *Client) do(ctx context.Context, method string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	var envelope struct {
		OK          bool            `json:"ok"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
		Parameters  *struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		// Ответ не от Bot API (например, страница ошибки прокси) — трактуем как ошибку статуса.
		return &APIError{
			Method:      method,
			StatusCode:  resp.StatusCode,
			Description: truncate(strings.TrimSpace(string(respBody)), 200),
		}
	}
	if !envelope.OK {
		apiErr := &APIError{
			Method:      method,
			StatusCode:  resp.StatusCode,
			ErrorCode:   envelope.ErrorCode,
			Description: envelope.Description,
		}
		if envelope.Parameters != nil && envelope.Parameters.RetryAfter > 0 {
			apiErr.RetryAfter = time.Duration(envelope.Parameters.RetryAfter) * time.Second
		} else if header := resp.Header.Get("Retry-After"); header != "" {
			if seconds, convErr := strconv.Atoi(header); convErr == nil && seconds > 0 {
				apiErr.RetryAfter = time.Duration(seconds) * time.Second
			}
		}
		return apiErr
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Result, out); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
	}
	return nil
}

func backoffDelay(attempt int) time.Duration {
	delay := time.Duration(float64(backoffBase) * math.Pow(2, float64(attempt-1)))
	if delay > backoffMax {
		delay = backoffMax
	}
	jitter := time.Duration(rand.Int63n(int64(delay/5) + 1))
	return delay + jitter
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}

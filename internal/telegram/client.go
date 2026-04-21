package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient(token string) *Client {
	return &Client{
		baseURL: fmt.Sprintf("https://api.telegram.org/bot%s", token),
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (c *Client) RestrictAll(ctx context.Context, chatID int64, userID int64) error {
	payload := map[string]any{
		"chat_id": chatID,
		"user_id": userID,
		"permissions": map[string]bool{
			"can_send_messages":       false,
			"can_send_audios":         false,
			"can_send_documents":      false,
			"can_send_photos":         false,
			"can_send_videos":         false,
			"can_send_video_notes":    false,
			"can_send_voice_notes":    false,
			"can_send_polls":          false,
			"can_send_other_messages": false,
			"can_add_web_page_previews": false,
			"can_change_info":           false,
			"can_invite_users":          false,
			"can_pin_messages":          false,
			"can_manage_topics":         false,
		},
	}
	return c.call(ctx, "restrictChatMember", payload, nil)
}

func (c *Client) Unrestrict(ctx context.Context, chatID int64, userID int64) error {
	payload := map[string]any{
		"chat_id": chatID,
		"user_id": userID,
		"permissions": map[string]bool{
			"can_send_messages":       true,
			"can_send_audios":         true,
			"can_send_documents":      true,
			"can_send_photos":         true,
			"can_send_videos":         true,
			"can_send_video_notes":    true,
			"can_send_voice_notes":    true,
			"can_send_polls":          true,
			"can_send_other_messages": true,
			"can_add_web_page_previews": true,
			"can_change_info":           false,
			"can_invite_users":          true,
			"can_pin_messages":          false,
			"can_manage_topics":         false,
		},
	}
	return c.call(ctx, "restrictChatMember", payload, nil)
}

func (c *Client) Ban(ctx context.Context, chatID int64, userID int64) error {
	payload := map[string]any{
		"chat_id":          chatID,
		"user_id":          userID,
		"revoke_messages":  true,
		"only_if_banned":   false,
	}
	return c.call(ctx, "banChatMember", payload, nil)
}

func (c *Client) UnbanChatMember(ctx context.Context, chatID int64, userID int64) error {
	payload := map[string]any{
		"chat_id":         chatID,
		"user_id":         userID,
		"only_if_banned":  true,
	}
	return c.call(ctx, "unbanChatMember", payload, nil)
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
	if err := c.call(ctx, "sendMessage", payload, &message); err != nil {
		return nil, err
	}
	return &message, nil
}

func (c *Client) ForwardMessage(ctx context.Context, toChatID int64, fromChatID int64, messageID int64) error {
	payload := map[string]any{
		"chat_id":      toChatID,
		"from_chat_id": fromChatID,
		"message_id":   messageID,
	}
	return c.call(ctx, "forwardMessage", payload, nil)
}

func (c *Client) DeleteMessage(ctx context.Context, chatID int64, messageID int64) error {
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
	}
	return c.call(ctx, "deleteMessage", payload, nil)
}

func (c *Client) AnswerCallback(ctx context.Context, callbackID string, text string) error {
	payload := map[string]any{
		"callback_query_id": callbackID,
		"text":              text,
		"show_alert":        false,
	}
	return c.call(ctx, "answerCallbackQuery", payload, nil)
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
	return c.call(ctx, "editMessageText", payload, nil)
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
	payload := map[string]any{
		"url":             webhookURL,
		"secret_token":    secret,
		"allowed_updates": []string{"message", "callback_query", "chat_member"},
	}
	return c.call(ctx, "setWebhook", payload, nil)
}

func (c *Client) call(ctx context.Context, method string, payload map[string]any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

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
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if !envelope.OK {
		return fmt.Errorf("telegram api error %s: %s", method, envelope.Description)
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Result, out); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
	}
	return nil
}

func BuildDeepLink(username string, token string) string {
	return fmt.Sprintf("https://t.me/%s?start=%s", strings.TrimPrefix(username, "@"), url.QueryEscape(token))
}

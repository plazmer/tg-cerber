package bot

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"tg_block/internal/config"
	"tg_block/internal/store"
	"tg_block/internal/telegram"
)

type Service struct {
	cfg      config.Config
	store    store.Repository
	tg       *telegram.Client
	logger   *slog.Logger
	botUser  *telegram.User
}

func NewService(cfg config.Config, repo store.Repository, tg *telegram.Client, logger *slog.Logger) *Service {
	return &Service{
		cfg:    cfg,
		store:  repo,
		tg:     tg,
		logger: logger,
	}
}

func (s *Service) Bootstrap(ctx context.Context) error {
	me, err := s.tg.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("get me: %w", err)
	}
	s.botUser = me

	duplicates, err := s.store.SanitizeDuplicatePending(ctx)
	if err != nil {
		return fmt.Errorf("sanitize duplicate pending: %w", err)
	}
	for _, duplicate := range duplicates {
		if duplicate.GroupPromptMessageID == 0 {
			continue
		}
		if err := s.tg.DeleteMessage(ctx, duplicate.GroupChatID, duplicate.GroupPromptMessageID); err != nil {
			s.logger.Debug("failed to delete duplicate prompt during sanitize", "token", duplicate.Token, "chat_id", duplicate.GroupChatID, "message_id", duplicate.GroupPromptMessageID, "error", err)
			continue
		}
	}

	return nil
}

func (s *Service) HandleUpdate(ctx context.Context, update telegram.Update) error {
	if update.ChatMember != nil {
		return s.handleChatMember(ctx, update.ChatMember)
	}
	if update.Message != nil {
		return s.handleMessage(ctx, update.Message)
	}
	if update.CallbackQuery != nil {
		return s.handleCallback(ctx, update.CallbackQuery)
	}
	return nil
}

func (s *Service) handleChatMember(ctx context.Context, cm *telegram.ChatMemberUpdate) error {
	joined := cm.OldChatMember.Status != "member" &&
		cm.OldChatMember.Status != "administrator" &&
		cm.OldChatMember.Status != "creator" &&
		(cm.NewChatMember.Status == "member" || cm.NewChatMember.Status == "restricted")
	if !joined || cm.NewChatMember.User.IsBot {
		return nil
	}

	userID := cm.NewChatMember.User.ID
	chatID := cm.Chat.ID
	if _, err := s.store.GetPendingByChatUser(ctx, chatID, userID); err == nil {
		s.logger.Debug("skip duplicate join event", "chat_id", chatID, "user_id", userID)
		return nil
	} else if err != sql.ErrNoRows {
		return fmt.Errorf("check pending by chat user: %w", err)
	}

	if err := s.tg.RestrictAll(ctx, chatID, userID); err != nil {
		return fmt.Errorf("restrict member: %w", err)
	}
	restricted := true
	defer func() {
		if !restricted {
			return
		}
		if err := s.tg.Unrestrict(context.Background(), chatID, userID); err != nil {
			s.logger.Error("compensation unrestrict failed", "chat_id", chatID, "user_id", userID, "error", err)
		}
	}()

	token, err := generateToken()
	if err != nil {
		return err
	}
	groupLink := buildGroupLink(cm.Chat.Username, cm.Chat.Title)
	if err := s.store.UpsertPending(ctx, store.Verification{
		Token:       token,
		GroupChatID: chatID,
		UserID:      userID,
		GroupLink:   groupLink,
		Status:      store.StatusPending,
	}); err != nil {
		return fmt.Errorf("save pending verification: %w", err)
	}

	deepLink := telegram.BuildDeepLink(s.botUser.Username, token)
	prompt := fmt.Sprintf(s.cfg.GroupPromptText, deepLink)
	promptMsg, err := s.tg.SendMessageWithResult(ctx, chatID, prompt, nil)
	if err != nil {
		return fmt.Errorf("send group prompt: %w", err)
	}
	if err := s.store.SetGroupPromptMessageID(ctx, token, promptMsg.MessageID); err != nil {
		return fmt.Errorf("save group prompt message id: %w", err)
	}
	go s.deletePromptAfterTimeout(chatID, promptMsg.MessageID, time.Minute)
	restricted = false

	s.logger.Info("new user restricted and prompted", "chat_id", chatID, "user_id", userID, "token", token)
	return nil
}

func (s *Service) handleMessage(ctx context.Context, message *telegram.Message) error {
	if message.Chat.Type == "private" {
		return s.handlePrivateMessage(ctx, message)
	}
	return s.handleGroupMessage(ctx, message)
}

func (s *Service) handleGroupMessage(ctx context.Context, message *telegram.Message) error {
	// Идемпотентный контур верификации работает только через chat_member.
	// Сервисные сообщения new_chat_members здесь намеренно игнорируются, чтобы исключить дубли.
	_ = message
	return nil
}

func (s *Service) handlePrivateMessage(ctx context.Context, message *telegram.Message) error {
	if message.From == nil {
		return nil
	}
	if message.From.ID == s.cfg.OwnerID {
		return s.handleOwnerPrivateMessage(ctx, message)
	}

	text := strings.TrimSpace(message.Text)
	if strings.HasPrefix(text, "/start") {
		token := parseStartToken(text)
		return s.handleStart(ctx, message.Chat.ID, token)
	}

	verification, err := s.store.GetPendingByUser(ctx, message.From.ID)
	if err != nil {
		if err == sql.ErrNoRows {
			return s.tg.SendMessage(ctx, message.Chat.ID, "У вас нет активной верификации.", nil)
		}
		return fmt.Errorf("get pending by user: %w", err)
	}

	if err := s.tg.ForwardMessage(ctx, s.cfg.OwnerID, message.Chat.ID, message.MessageID); err != nil {
		return fmt.Errorf("forward user answer: %w", err)
	}

	controls := map[string]any{
		"inline_keyboard": [][]map[string]string{
			{
				{"text": "Пустить", "callback_data": "allow:" + verification.Token},
				{"text": "Кик+бан", "callback_data": "deny:" + verification.Token},
			},
		},
	}
	ownerNote := buildOwnerStatusText("🔴", "ЗАБЛОКИРОВАН", verification.Token, message.From.ID)
	ownerMessage, err := s.tg.SendMessageWithResult(ctx, s.cfg.OwnerID, ownerNote, controls)
	if err != nil {
		return fmt.Errorf("send controls to owner: %w", err)
	}
	if err := s.store.SetOwnerStatusMessageID(ctx, verification.Token, ownerMessage.MessageID); err != nil {
		return fmt.Errorf("save owner status message id: %w", err)
	}

	return s.tg.SendMessage(ctx, message.Chat.ID, "Ответ отправлен администратору. Ожидайте решения.", nil)
}

func (s *Service) handleStart(ctx context.Context, privateChatID int64, token string) error {
	if token == "" {
		return s.tg.SendMessage(ctx, privateChatID, "Для верификации нужно перейти по ссылке из группы.", nil)
	}

	verification, err := s.store.GetByToken(ctx, token)
	if err != nil {
		if err == sql.ErrNoRows {
			return s.tg.SendMessage(ctx, privateChatID, "Ссылка устарела или недействительна.", nil)
		}
		return fmt.Errorf("get token: %w", err)
	}

	if verification.Status != store.StatusPending {
		return s.tg.SendMessage(ctx, privateChatID, "Эта заявка уже обработана.", nil)
	}

	if verification.QuestionSent {
		return s.tg.SendMessage(ctx, privateChatID, "Вопрос уже отправлен, пожалуйста ответьте на него сообщением.", nil)
	}
	if verification.GroupPromptMessageID != 0 {
		if err := s.tg.DeleteMessage(ctx, verification.GroupChatID, verification.GroupPromptMessageID); err != nil {
			s.logger.Debug("failed to delete group prompt on start", "token", token, "error", err)
		}
	}

	if err := s.store.SetQuestionSent(ctx, token, true); err != nil {
		return fmt.Errorf("set question sent: %w", err)
	}
	return s.tg.SendMessage(ctx, privateChatID, s.cfg.BotQuestion, nil)
}

func (s *Service) handleCallback(ctx context.Context, cb *telegram.CallbackQuery) error {
	if cb.From.ID != s.cfg.OwnerID {
		return s.tg.AnswerCallback(ctx, cb.ID, "Недостаточно прав")
	}

	parts := strings.SplitN(cb.Data, ":", 2)
	if len(parts) != 2 {
		return s.tg.AnswerCallback(ctx, cb.ID, "Некорректная команда")
	}
	action := parts[0]
	token := parts[1]

	verification, err := s.store.GetByToken(ctx, token)
	if err != nil {
		if err == sql.ErrNoRows {
			return s.tg.AnswerCallback(ctx, cb.ID, "Заявка не найдена")
		}
		return fmt.Errorf("load verification: %w", err)
	}

	if action == "unban" {
		if verification.Status != store.StatusRejected {
			return s.tg.AnswerCallback(ctx, cb.ID, "Разбанить можно только после бана")
		}
		if err := s.tg.UnbanChatMember(ctx, verification.GroupChatID, verification.UserID); err != nil {
			return fmt.Errorf("unban user: %w", err)
		}
		if err := s.store.SetStatus(ctx, token, store.StatusUnbanned); err != nil {
			return fmt.Errorf("set unbanned status: %w", err)
		}
		if verification.OwnerStatusMessageID != 0 {
			statusText := buildOwnerStatusText("🟢", "РАЗБАНЕН (ошибочный бан отменён)", verification.Token, verification.UserID)
			if err := s.tg.EditMessageText(ctx, s.cfg.OwnerID, verification.OwnerStatusMessageID, statusText, telegram.ClearReplyMarkup()); err != nil {
				return fmt.Errorf("edit owner status message: %w", err)
			}
		}
		return s.tg.AnswerCallback(ctx, cb.ID, "Пользователь разбанен в группе")
	}

	if verification.Status != store.StatusPending {
		return s.tg.AnswerCallback(ctx, cb.ID, "Заявка уже обработана")
	}

	switch action {
	case "allow":
		if err := s.tg.Unrestrict(ctx, verification.GroupChatID, verification.UserID); err != nil {
			return fmt.Errorf("unrestrict user: %w", err)
		}
		if err := s.store.SetStatus(ctx, token, store.StatusApproved); err != nil {
			return fmt.Errorf("set approved status: %w", err)
		}
		if err := s.tg.SendMessage(ctx, verification.UserID, fmt.Sprintf("Всё ок, можете писать в группу %s", verification.GroupLink), nil); err != nil {
			s.logger.Debug("failed to send success message to user", "user_id", verification.UserID, "error", err)
		}
		if verification.OwnerStatusMessageID != 0 {
			statusText := buildOwnerStatusText("🟢", "РАЗБЛОКИРОВАН", verification.Token, verification.UserID)
			if err := s.tg.EditMessageText(ctx, s.cfg.OwnerID, verification.OwnerStatusMessageID, statusText, telegram.ClearReplyMarkup()); err != nil {
				return fmt.Errorf("edit owner status message: %w", err)
			}
		}
		if err := s.tg.AnswerCallback(ctx, cb.ID, "Пользователь допущен"); err != nil {
			return err
		}
	case "deny":
		if err := s.tg.Ban(ctx, verification.GroupChatID, verification.UserID); err != nil {
			return fmt.Errorf("ban user: %w", err)
		}
		if err := s.store.SetStatus(ctx, token, store.StatusRejected); err != nil {
			return fmt.Errorf("set rejected status: %w", err)
		}
		if verification.OwnerStatusMessageID != 0 {
			statusText := buildOwnerStatusText("🔴", "ЗАБЛОКИРОВАН", verification.Token, verification.UserID)
			unbanKeyboard := map[string]any{
				"inline_keyboard": [][]map[string]string{
					{
						{"text": "Разбанить", "callback_data": "unban:" + verification.Token},
					},
				},
			}
			if err := s.tg.EditMessageText(ctx, s.cfg.OwnerID, verification.OwnerStatusMessageID, statusText, unbanKeyboard); err != nil {
				return fmt.Errorf("edit owner status message: %w", err)
			}
		}
		if err := s.tg.AnswerCallback(ctx, cb.ID, "Пользователь заблокирован"); err != nil {
			return err
		}
	default:
		return s.tg.AnswerCallback(ctx, cb.ID, "Неизвестное действие")
	}

	return nil
}

func parseStartToken(text string) string {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func generateToken() (string, error) {
	random := make([]byte, 9)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}

func buildOwnerStatusText(emoji string, status string, token string, userID int64) string {
	return fmt.Sprintf("%s %s\nПользователь: %d\nТокен: %s", emoji, status, userID, token)
}

func buildGroupLink(username string, title string) string {
	if username != "" {
		return fmt.Sprintf("https://t.me/%s", strings.TrimPrefix(username, "@"))
	}
	if title != "" {
		return title
	}
	return "в группу"
}

func (s *Service) deletePromptAfterTimeout(chatID int64, messageID int64, timeout time.Duration) {
	time.Sleep(timeout)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.tg.DeleteMessage(ctx, chatID, messageID); err != nil {
		s.logger.Debug("failed to delete group prompt by timeout", "chat_id", chatID, "message_id", messageID, "error", err)
	}
}

func (s *Service) handleOwnerPrivateMessage(ctx context.Context, message *telegram.Message) error {
	if message.ReplyToMessage == nil || message.ReplyToMessage.ForwardFrom == nil {
		return nil
	}
	targetUserID := message.ReplyToMessage.ForwardFrom.ID
	if targetUserID == 0 {
		return nil
	}

	text := strings.TrimSpace(message.Text)
	if text == "" {
		text = "Администратор отправил сообщение без текста."
	}
	relay := fmt.Sprintf("Сообщение от модератора:\n%s", text)
	if err := s.tg.SendMessage(ctx, targetUserID, relay, nil); err != nil {
		return fmt.Errorf("relay owner message to user: %w", err)
	}
	return nil
}

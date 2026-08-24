package bot

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"tg_block/internal/config"
	"tg_block/internal/store"
	"tg_block/internal/telegram"
)

const (
	janitorInterval           = 5 * time.Minute
	processedUpdatesRetention = 24 * time.Hour
	expiredBatchLimit         = 50

	ownerHelpText = "Бот обрабатывает заявки на вступление в группу.\n" +
		"Заявка не одобряется автоматически: по каждой приходит карточка с кнопками.\n" +
		"Разрешить — впустить в группу, Отклонить — отказать (можно подать заявку снова), Бан — закрыть повторные заявки."
)

type Service struct {
	cfg     config.Config
	store   store.Repository
	tg      *telegram.Client
	logger  *slog.Logger
	botUser *telegram.User
}

func NewService(cfg config.Config, repo store.Repository, tg *telegram.Client, logger *slog.Logger) *Service {
	return &Service{
		cfg:    cfg,
		store:  repo,
		tg:     tg,
		logger: logger,
	}
}

// Bootstrap проверяет токен и запоминает данные бота.
func (s *Service) Bootstrap(ctx context.Context) error {
	me, err := s.tg.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("get me: %w", err)
	}
	s.botUser = me
	return nil
}

func (s *Service) BotUser() *telegram.User {
	return s.botUser
}

// Run ведет фоновое обслуживание: чистка журнала апдейтов и авто-отклонение
// заявок по PENDING_TTL. Возвращается при отмене контекста.
func (s *Service) Run(ctx context.Context) {
	interval := janitorInterval
	if s.cfg.PendingTTL > 0 && s.cfg.PendingTTL < interval {
		interval = s.cfg.PendingTTL
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runMaintenance(ctx)
		}
	}
}

func (s *Service) HandleUpdate(ctx context.Context, update telegram.Update) error {
	if update.UpdateID != 0 {
		fresh, err := s.store.MarkUpdateProcessed(ctx, update.UpdateID)
		if err != nil {
			return fmt.Errorf("dedupe update: %w", err)
		}
		if !fresh {
			s.logger.Debug("skip duplicate update", "update_id", update.UpdateID)
			return nil
		}
	}

	switch {
	case update.ChatJoinRequest != nil:
		return s.handleJoinRequest(ctx, update.ChatJoinRequest)
	case update.ChatMember != nil:
		return s.handleChatMember(ctx, update.ChatMember)
	case update.CallbackQuery != nil:
		return s.handleCallback(ctx, update.CallbackQuery)
	case update.Message != nil:
		return s.handleMessage(ctx, update.Message)
	}
	return nil
}

// handleJoinRequest — единственная точка входа нового участника.
// Заявка НЕ одобряется: она остается в очереди Telegram до решения владельца.
func (s *Service) handleJoinRequest(ctx context.Context, request *telegram.ChatJoinRequest) error {
	if request.From.IsBot {
		return nil
	}

	token, err := generateToken()
	if err != nil {
		return err
	}

	record := store.JoinRequest{
		Token:       token,
		GroupChatID: request.Chat.ID,
		GroupTitle:  groupTitle(request.Chat),
		GroupLink:   buildGroupLink(request.Chat.Username),
		UserID:      request.From.ID,
		UserChatID:  request.UserChatID,
		UserLabel:   userLabel(request.From),
		Bio:         request.Bio,
		Status:      store.StatusPending,
	}
	if err := s.store.Create(ctx, record); err != nil {
		if errors.Is(err, store.ErrDuplicatePending) {
			s.logger.Debug("skip duplicate join request", "chat_id", record.GroupChatID, "user_id", record.UserID)
			return nil
		}
		return fmt.Errorf("save join request: %w", err)
	}
	if request.Date > 0 {
		record.CreatedAt = time.Unix(request.Date, 0).UTC()
	} else {
		record.CreatedAt = time.Now().UTC()
	}

	text := buildOwnerCard("🕓", "ЗАЯВКА НА ВСТУПЛЕНИЕ", record, "Пользователь не допущен в группу до вашего решения.")
	ownerMessage, err := s.tg.SendMessageWithResult(ctx, s.cfg.OwnerID, text, pendingKeyboard(token))
	if err != nil {
		// Владелец не уведомлен — снимаем pending, чтобы повторная заявка снова создала карточку.
		if cancelErr := s.store.SetStatus(context.WithoutCancel(ctx), token, store.StatusCancelled); cancelErr != nil {
			s.logger.Error("cancel unnotified join request failed", "token", token, "error", cancelErr)
		}
		return fmt.Errorf("send owner card: %w", err)
	}
	if err := s.store.SetOwnerStatusMessageID(ctx, token, ownerMessage.MessageID); err != nil {
		return fmt.Errorf("save owner card message id: %w", err)
	}

	s.logger.Info("join request registered", "chat_id", record.GroupChatID, "user_id", record.UserID, "token", token)
	return nil
}

// handleChatMember только наблюдает за состоянием: решение могло быть принято
// в родном интерфейсе Telegram, минуя кнопки бота.
func (s *Service) handleChatMember(ctx context.Context, cm *telegram.ChatMemberUpdate) error {
	if cm.NewChatMember.User.IsBot {
		return nil
	}

	request, err := s.store.GetPendingByChatUser(ctx, cm.Chat.ID, cm.NewChatMember.User.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("lookup pending by chat member: %w", err)
	}

	switch cm.NewChatMember.Status {
	case "member", "administrator", "creator", "restricted":
		claimed, err := s.store.ClaimStatus(ctx, request.Token, []store.Status{store.StatusPending}, store.StatusApproved)
		if err != nil {
			return fmt.Errorf("claim approved by chat member: %w", err)
		}
		if !claimed {
			return nil
		}
		request.Status = store.StatusApproved
		s.updateOwnerCard(ctx, *request, "🟢", "ЗАЯВКА РАЗРЕШЕНА", "Решение принято в интерфейсе Telegram, мимо бота.", telegram.ClearReplyMarkup())
		s.logger.Info("join request approved outside bot", "chat_id", cm.Chat.ID, "user_id", request.UserID, "token", request.Token)
	case "kicked":
		claimed, err := s.store.ClaimStatus(ctx, request.Token, []store.Status{store.StatusPending}, store.StatusRejected)
		if err != nil {
			return fmt.Errorf("claim rejected by chat member: %w", err)
		}
		if !claimed {
			return nil
		}
		request.Status = store.StatusRejected
		s.updateOwnerCard(ctx, *request, "🔴", "ПОЛЬЗОВАТЕЛЬ ЗАБАНЕН", "Бан выставлен в интерфейсе Telegram, мимо бота.", unbanKeyboard(request.Token))
		s.logger.Info("join request banned outside bot", "chat_id", cm.Chat.ID, "user_id", request.UserID, "token", request.Token)
	}
	return nil
}

func (s *Service) handleMessage(ctx context.Context, message *telegram.Message) error {
	if message.From == nil || message.Chat.Type != "private" {
		return nil
	}

	text := strings.TrimSpace(message.Text)
	if message.From.ID == s.cfg.OwnerID {
		if !strings.HasPrefix(text, "/start") && !strings.HasPrefix(text, "/help") {
			return nil
		}
		return s.tg.SendMessage(ctx, message.Chat.ID, ownerHelpText, nil)
	}

	if !strings.HasPrefix(text, "/start") {
		return nil
	}
	if _, err := s.store.GetPendingByUser(ctx, message.From.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return s.tg.SendMessage(ctx, message.Chat.ID, "Бот обрабатывает заявки на вступление в группу. Активной заявки за вами не числится.", nil)
		}
		return fmt.Errorf("get pending by user: %w", err)
	}
	return s.tg.SendMessage(ctx, message.Chat.ID, "Ваша заявка на рассмотрении у администратора. Решение придет в этот чат.", nil)
}

func (s *Service) handleCallback(ctx context.Context, cb *telegram.CallbackQuery) error {
	if cb.From.ID != s.cfg.OwnerID {
		return s.tg.AnswerCallback(ctx, cb.ID, "Недостаточно прав")
	}

	parts := strings.SplitN(cb.Data, ":", 2)
	if len(parts) != 2 {
		return s.tg.AnswerCallback(ctx, cb.ID, "Некорректная команда")
	}
	action, token := parts[0], parts[1]

	request, err := s.store.GetByToken(ctx, token)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return s.tg.AnswerCallback(ctx, cb.ID, "Заявка не найдена")
		}
		return fmt.Errorf("load join request: %w", err)
	}

	switch action {
	case "allow":
		return s.approve(ctx, cb, *request)
	case "decline":
		return s.decline(ctx, cb, *request)
	case "ban":
		return s.ban(ctx, cb, *request)
	case "unban":
		return s.unban(ctx, cb, *request)
	default:
		return s.tg.AnswerCallback(ctx, cb.ID, "Неизвестное действие")
	}
}

func (s *Service) approve(ctx context.Context, cb *telegram.CallbackQuery, request store.JoinRequest) error {
	claimed, err := s.store.ClaimStatus(ctx, request.Token, []store.Status{store.StatusPending}, store.StatusApproved)
	if err != nil {
		return fmt.Errorf("claim approve: %w", err)
	}
	if !claimed {
		return s.tg.AnswerCallback(ctx, cb.ID, "Заявка уже обработана")
	}

	if err := s.tg.ApproveChatJoinRequest(ctx, request.GroupChatID, request.UserID); err != nil {
		s.restoreStatus(ctx, request.Token, store.StatusApproved, store.StatusPending)
		s.logger.Error("approve join request failed", "token", request.Token, "error", err)
		return s.tg.AnswerCallback(ctx, cb.ID, "Не удалось одобрить: "+shortError(err))
	}

	s.notifyUser(ctx, request, approvedUserText(request))
	request.Status = store.StatusApproved
	s.updateOwnerCard(ctx, request, "🟢", "ЗАЯВКА РАЗРЕШЕНА", "", telegram.ClearReplyMarkup())
	s.logger.Info("join request approved", "chat_id", request.GroupChatID, "user_id", request.UserID, "token", request.Token)
	return s.tg.AnswerCallback(ctx, cb.ID, "Пользователь допущен в группу")
}

func (s *Service) decline(ctx context.Context, cb *telegram.CallbackQuery, request store.JoinRequest) error {
	claimed, err := s.store.ClaimStatus(ctx, request.Token, []store.Status{store.StatusPending}, store.StatusDeclined)
	if err != nil {
		return fmt.Errorf("claim decline: %w", err)
	}
	if !claimed {
		return s.tg.AnswerCallback(ctx, cb.ID, "Заявка уже обработана")
	}

	if err := s.tg.DeclineChatJoinRequest(ctx, request.GroupChatID, request.UserID); err != nil {
		s.restoreStatus(ctx, request.Token, store.StatusDeclined, store.StatusPending)
		s.logger.Error("decline join request failed", "token", request.Token, "error", err)
		return s.tg.AnswerCallback(ctx, cb.ID, "Не удалось отклонить: "+shortError(err))
	}

	request.Status = store.StatusDeclined
	s.updateOwnerCard(ctx, request, "⚪", "ЗАЯВКА ОТКЛОНЕНА", "Пользователь может подать заявку повторно. Кнопка Бан закрывает повторные заявки.", banKeyboard(request.Token))
	s.logger.Info("join request declined", "chat_id", request.GroupChatID, "user_id", request.UserID, "token", request.Token)
	return s.tg.AnswerCallback(ctx, cb.ID, "Заявка отклонена")
}

func (s *Service) ban(ctx context.Context, cb *telegram.CallbackQuery, request store.JoinRequest) error {
	previous := request.Status
	claimed, err := s.store.ClaimStatus(ctx, request.Token, []store.Status{
		store.StatusPending, store.StatusDeclined, store.StatusApproved, store.StatusUnbanned,
	}, store.StatusRejected)
	if err != nil {
		return fmt.Errorf("claim ban: %w", err)
	}
	if !claimed {
		return s.tg.AnswerCallback(ctx, cb.ID, "Заявка уже обработана")
	}

	// Висящую заявку снимаем явно: бан не всегда убирает ее из очереди.
	if previous == store.StatusPending {
		if err := s.tg.DeclineChatJoinRequest(ctx, request.GroupChatID, request.UserID); err != nil {
			s.logger.Debug("decline before ban failed", "token", request.Token, "error", err)
		}
	}
	if err := s.tg.Ban(ctx, request.GroupChatID, request.UserID); err != nil {
		s.restoreStatus(ctx, request.Token, store.StatusRejected, previous)
		s.logger.Error("ban user failed", "token", request.Token, "error", err)
		return s.tg.AnswerCallback(ctx, cb.ID, "Не удалось забанить: "+shortError(err))
	}

	request.Status = store.StatusRejected
	s.updateOwnerCard(ctx, request, "🔴", "ПОЛЬЗОВАТЕЛЬ ЗАБАНЕН", "Повторные заявки невозможны, пока бан не снят.", unbanKeyboard(request.Token))
	s.logger.Info("user banned", "chat_id", request.GroupChatID, "user_id", request.UserID, "token", request.Token)
	return s.tg.AnswerCallback(ctx, cb.ID, "Пользователь забанен")
}

func (s *Service) unban(ctx context.Context, cb *telegram.CallbackQuery, request store.JoinRequest) error {
	claimed, err := s.store.ClaimStatus(ctx, request.Token, []store.Status{store.StatusRejected}, store.StatusUnbanned)
	if err != nil {
		return fmt.Errorf("claim unban: %w", err)
	}
	if !claimed {
		return s.tg.AnswerCallback(ctx, cb.ID, "Разбанить можно только после бана")
	}

	if err := s.tg.UnbanChatMember(ctx, request.GroupChatID, request.UserID); err != nil {
		s.restoreStatus(ctx, request.Token, store.StatusUnbanned, store.StatusRejected)
		s.logger.Error("unban user failed", "token", request.Token, "error", err)
		return s.tg.AnswerCallback(ctx, cb.ID, "Не удалось разбанить: "+shortError(err))
	}

	request.Status = store.StatusUnbanned
	s.updateOwnerCard(ctx, request, "⚪", "БАН СНЯТ", "Пользователь может подать заявку заново.", telegram.ClearReplyMarkup())
	s.logger.Info("user unbanned", "chat_id", request.GroupChatID, "user_id", request.UserID, "token", request.Token)
	return s.tg.AnswerCallback(ctx, cb.ID, "Бан снят")
}

func (s *Service) runMaintenance(ctx context.Context) {
	if err := s.store.PurgeProcessedUpdates(ctx, time.Now().UTC().Add(-processedUpdatesRetention)); err != nil {
		s.logger.Error("purge processed updates failed", "error", err)
	}
	if s.cfg.PendingTTL <= 0 {
		return
	}

	expired, err := s.store.ListPendingOlderThan(ctx, time.Now().UTC().Add(-s.cfg.PendingTTL), expiredBatchLimit)
	if err != nil {
		s.logger.Error("list expired join requests failed", "error", err)
		return
	}
	for _, request := range expired {
		if ctx.Err() != nil {
			return
		}
		claimed, err := s.store.ClaimStatus(ctx, request.Token, []store.Status{store.StatusPending}, store.StatusDeclined)
		if err != nil {
			s.logger.Error("claim expired join request failed", "token", request.Token, "error", err)
			continue
		}
		if !claimed {
			continue
		}
		if err := s.tg.DeclineChatJoinRequest(ctx, request.GroupChatID, request.UserID); err != nil {
			s.restoreStatus(ctx, request.Token, store.StatusDeclined, store.StatusPending)
			s.logger.Error("auto decline failed", "token", request.Token, "error", err)
			continue
		}

		request.Status = store.StatusDeclined
		note := fmt.Sprintf("Решения не было дольше %s. Пользователь может подать заявку повторно.", s.cfg.PendingTTL)
		s.updateOwnerCard(ctx, request, "⌛", "ЗАЯВКА ОТКЛОНЕНА ПО ТАЙМАУТУ", note, banKeyboard(request.Token))
		s.logger.Info("join request auto declined", "chat_id", request.GroupChatID, "user_id", request.UserID, "token", request.Token)
	}
}

// updateOwnerCard правит карточку в личке владельца. Решение к этому моменту уже
// применено в Telegram, поэтому ошибка редактирования только логируется.
func (s *Service) updateOwnerCard(ctx context.Context, request store.JoinRequest, emoji string, headline string, note string, markup map[string]any) {
	if request.OwnerStatusMessageID == 0 {
		return
	}
	text := buildOwnerCard(emoji, headline, request, note)
	if err := s.tg.EditMessageText(ctx, s.cfg.OwnerID, request.OwnerStatusMessageID, text, markup); err != nil {
		s.logger.Error("edit owner card failed", "token", request.Token, "error", err)
	}
}

func (s *Service) restoreStatus(ctx context.Context, token string, from store.Status, to store.Status) {
	restoreCtx := context.WithoutCancel(ctx)
	if _, err := s.store.ClaimStatus(restoreCtx, token, []store.Status{from}, to); err != nil {
		s.logger.Error("restore status failed", "token", token, "from", string(from), "to", string(to), "error", err)
	}
}

// notifyUser — best-effort: пользователь мог не начинать диалог с ботом.
func (s *Service) notifyUser(ctx context.Context, request store.JoinRequest, text string) {
	chatID := request.UserChatID
	if chatID == 0 {
		chatID = request.UserID
	}
	if err := s.tg.SendMessage(ctx, chatID, text, nil); err != nil {
		s.logger.Debug("notify user failed", "user_id", request.UserID, "error", err)
	}
}

func approvedUserText(request store.JoinRequest) string {
	text := "Ваша заявка на вступление одобрена."
	if request.GroupLink != "" {
		text += "\n" + request.GroupLink
	}
	return text
}

func pendingKeyboard(token string) map[string]any {
	return map[string]any{
		"inline_keyboard": [][]map[string]string{
			{
				{"text": "✅ Разрешить", "callback_data": "allow:" + token},
				{"text": "🚫 Отклонить", "callback_data": "decline:" + token},
			},
			{
				{"text": "⛔ Бан", "callback_data": "ban:" + token},
			},
		},
	}
}

func banKeyboard(token string) map[string]any {
	return map[string]any{
		"inline_keyboard": [][]map[string]string{
			{
				{"text": "⛔ Бан", "callback_data": "ban:" + token},
			},
		},
	}
}

func unbanKeyboard(token string) map[string]any {
	return map[string]any{
		"inline_keyboard": [][]map[string]string{
			{
				{"text": "♻️ Разбанить", "callback_data": "unban:" + token},
			},
		},
	}
}

func buildOwnerCard(emoji string, headline string, request store.JoinRequest, note string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s %s\n", emoji, headline)
	fmt.Fprintf(&builder, "Группа: %s\n", request.GroupTitle)
	fmt.Fprintf(&builder, "Пользователь: %s\n", request.UserLabel)
	fmt.Fprintf(&builder, "ID: %d\n", request.UserID)
	if request.Bio != "" {
		fmt.Fprintf(&builder, "Bio: %s\n", request.Bio)
	}
	if !request.CreatedAt.IsZero() {
		fmt.Fprintf(&builder, "Подана: %s UTC\n", request.CreatedAt.UTC().Format("2006-01-02 15:04"))
	}
	fmt.Fprintf(&builder, "Токен: %s", request.Token)
	if note != "" {
		fmt.Fprintf(&builder, "\n%s", note)
	}
	return builder.String()
}

func userLabel(user telegram.User) string {
	name := strings.TrimSpace(strings.TrimSpace(user.FirstName) + " " + strings.TrimSpace(user.LastName))
	if name == "" {
		name = "без имени"
	}
	if user.Username != "" {
		return fmt.Sprintf("%s (@%s)", name, strings.TrimPrefix(user.Username, "@"))
	}
	return name + " (без username)"
}

func groupTitle(chat telegram.Chat) string {
	if strings.TrimSpace(chat.Title) != "" {
		return chat.Title
	}
	return strconv.FormatInt(chat.ID, 10)
}

// buildGroupLink возвращает публичную ссылку на группу либо пустую строку
// для приватной группы без username.
func buildGroupLink(username string) string {
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	if username == "" {
		return ""
	}
	return "https://t.me/" + username
}

func generateToken() (string, error) {
	random := make([]byte, 9)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}

// shortError готовит текст для answerCallbackQuery: лимит там 200 символов.
func shortError(err error) string {
	text := err.Error()
	runes := []rune(text)
	if len(runes) > 150 {
		return string(runes[:150])
	}
	return text
}

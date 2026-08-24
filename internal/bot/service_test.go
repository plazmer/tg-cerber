package bot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"tg_block/internal/config"
	"tg_block/internal/store"
	"tg_block/internal/telegram"
)

type apiCall struct {
	Method  string
	Payload map[string]any
}

type fakeAPI struct {
	mu    sync.Mutex
	calls []apiCall
}

func (f *fakeAPI) record(method string, payload map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, apiCall{Method: method, Payload: payload})
}

func (f *fakeAPI) countOf(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	count := 0
	for _, call := range f.calls {
		if call.Method == method {
			count++
		}
	}
	return count
}

func (f *fakeAPI) first(method string) (apiCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, call := range f.calls {
		if call.Method == method {
			return call, true
		}
	}
	return apiCall{}, false
}

func newTestService(t *testing.T) (*Service, *fakeAPI, store.Repository) {
	t.Helper()

	api := &fakeAPI{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/")
		payload := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		api.record(method, payload)

		w.Header().Set("Content-Type", "application/json")
		if method == "sendMessage" {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":777,"chat":{"id":1,"type":"private"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	t.Cleanup(server.Close)

	repo, err := store.NewSQLiteRepository(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	t.Cleanup(func() { repo.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{OwnerID: 1}
	service := NewService(cfg, repo, telegram.NewClientWithEndpoint(server.URL, logger), logger)
	return service, api, repo
}

func joinRequestUpdate(updateID int64) telegram.Update {
	return telegram.Update{
		UpdateID: updateID,
		ChatJoinRequest: &telegram.ChatJoinRequest{
			Chat:       telegram.Chat{ID: -1001, Type: "supergroup", Title: "Test group"},
			From:       telegram.User{ID: 555, FirstName: "Ivan", Username: "ivan"},
			UserChatID: 555,
			Date:       1700000000,
		},
	}
}

func TestJoinRequestDoesNotApproveAndNotifiesOwner(t *testing.T) {
	service, api, repo := newTestService(t)
	ctx := context.Background()

	if err := service.HandleUpdate(ctx, joinRequestUpdate(1)); err != nil {
		t.Fatalf("handle join request: %v", err)
	}

	if got := api.countOf("approveChatJoinRequest"); got != 0 {
		t.Fatalf("join request must not be approved automatically, got %d approve calls", got)
	}
	call, ok := api.first("sendMessage")
	if !ok {
		t.Fatal("owner card was not sent")
	}
	if chatID, _ := call.Payload["chat_id"].(float64); int64(chatID) != 1 {
		t.Fatalf("owner card sent to wrong chat: %v", call.Payload["chat_id"])
	}
	if text, _ := call.Payload["text"].(string); !strings.Contains(text, "@ivan") {
		t.Fatalf("owner card must contain profile info, got %q", text)
	}

	request, err := repo.GetPendingByChatUser(ctx, -1001, 555)
	if err != nil {
		t.Fatalf("pending request must be stored: %v", err)
	}
	if request.OwnerStatusMessageID != 777 {
		t.Fatalf("owner card message id not saved, got %d", request.OwnerStatusMessageID)
	}
}

func TestDuplicateUpdateIsIgnored(t *testing.T) {
	service, api, _ := newTestService(t)
	ctx := context.Background()

	if err := service.HandleUpdate(ctx, joinRequestUpdate(1)); err != nil {
		t.Fatalf("handle join request: %v", err)
	}
	if err := service.HandleUpdate(ctx, joinRequestUpdate(1)); err != nil {
		t.Fatalf("handle repeated update: %v", err)
	}

	if got := api.countOf("sendMessage"); got != 1 {
		t.Fatalf("repeated update_id must not resend the card, got %d sends", got)
	}
}

func TestRepeatedJoinRequestDoesNotDuplicateCard(t *testing.T) {
	service, api, _ := newTestService(t)
	ctx := context.Background()

	if err := service.HandleUpdate(ctx, joinRequestUpdate(1)); err != nil {
		t.Fatalf("handle join request: %v", err)
	}
	// Тот же заявитель, другой update_id: Telegram может прислать заявку повторно.
	if err := service.HandleUpdate(ctx, joinRequestUpdate(2)); err != nil {
		t.Fatalf("handle second join request: %v", err)
	}

	if got := api.countOf("sendMessage"); got != 1 {
		t.Fatalf("second request for the same user must not create a card, got %d sends", got)
	}
}

func TestApproveCallbackApprovesOnceAndClosesCard(t *testing.T) {
	service, api, repo := newTestService(t)
	ctx := context.Background()

	if err := service.HandleUpdate(ctx, joinRequestUpdate(1)); err != nil {
		t.Fatalf("handle join request: %v", err)
	}
	pending, err := repo.GetPendingByChatUser(ctx, -1001, 555)
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}

	callback := func(updateID int64) telegram.Update {
		return telegram.Update{
			UpdateID: updateID,
			CallbackQuery: &telegram.CallbackQuery{
				ID:   "cb",
				From: telegram.User{ID: 1},
				Data: "allow:" + pending.Token,
			},
		}
	}
	if err := service.HandleUpdate(ctx, callback(2)); err != nil {
		t.Fatalf("handle approve callback: %v", err)
	}
	// Повторное нажатие той же кнопки не должно вызывать API второй раз.
	if err := service.HandleUpdate(ctx, callback(3)); err != nil {
		t.Fatalf("handle repeated approve callback: %v", err)
	}

	if got := api.countOf("approveChatJoinRequest"); got != 1 {
		t.Fatalf("expected exactly one approve call, got %d", got)
	}
	if got := api.countOf("editMessageText"); got != 1 {
		t.Fatalf("expected the card to be edited once, got %d", got)
	}

	updated, err := repo.GetByToken(ctx, pending.Token)
	if err != nil {
		t.Fatalf("get by token: %v", err)
	}
	if updated.Status != store.StatusApproved {
		t.Fatalf("expected approved status, got %s", updated.Status)
	}
}

func TestBanCallbackDeclinesPendingRequestFirst(t *testing.T) {
	service, api, repo := newTestService(t)
	ctx := context.Background()

	if err := service.HandleUpdate(ctx, joinRequestUpdate(1)); err != nil {
		t.Fatalf("handle join request: %v", err)
	}
	pending, err := repo.GetPendingByChatUser(ctx, -1001, 555)
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}

	update := telegram.Update{
		UpdateID: 2,
		CallbackQuery: &telegram.CallbackQuery{
			ID:   "cb",
			From: telegram.User{ID: 1},
			Data: "ban:" + pending.Token,
		},
	}
	if err := service.HandleUpdate(ctx, update); err != nil {
		t.Fatalf("handle ban callback: %v", err)
	}

	if got := api.countOf("declineChatJoinRequest"); got != 1 {
		t.Fatalf("pending request must be declined before ban, got %d calls", got)
	}
	if got := api.countOf("banChatMember"); got != 1 {
		t.Fatalf("expected one ban call, got %d", got)
	}

	updated, err := repo.GetByToken(ctx, pending.Token)
	if err != nil {
		t.Fatalf("get by token: %v", err)
	}
	if updated.Status != store.StatusRejected {
		t.Fatalf("expected rejected status, got %s", updated.Status)
	}
}

func TestCallbackFromStrangerIsRejected(t *testing.T) {
	service, api, repo := newTestService(t)
	ctx := context.Background()

	if err := service.HandleUpdate(ctx, joinRequestUpdate(1)); err != nil {
		t.Fatalf("handle join request: %v", err)
	}
	pending, err := repo.GetPendingByChatUser(ctx, -1001, 555)
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}

	update := telegram.Update{
		UpdateID: 2,
		CallbackQuery: &telegram.CallbackQuery{
			ID:   "cb",
			From: telegram.User{ID: 999},
			Data: "allow:" + pending.Token,
		},
	}
	if err := service.HandleUpdate(ctx, update); err != nil {
		t.Fatalf("handle stranger callback: %v", err)
	}

	if got := api.countOf("approveChatJoinRequest"); got != 0 {
		t.Fatalf("stranger must not approve requests, got %d approve calls", got)
	}
}

func TestChatMemberClosesCardWhenApprovedOutsideBot(t *testing.T) {
	service, api, repo := newTestService(t)
	ctx := context.Background()

	if err := service.HandleUpdate(ctx, joinRequestUpdate(1)); err != nil {
		t.Fatalf("handle join request: %v", err)
	}
	pending, err := repo.GetPendingByChatUser(ctx, -1001, 555)
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}

	update := telegram.Update{
		UpdateID: 2,
		ChatMember: &telegram.ChatMemberUpdate{
			Chat:          telegram.Chat{ID: -1001, Type: "supergroup", Title: "Test group"},
			OldChatMember: telegram.ChatMember{User: telegram.User{ID: 555}, Status: "left"},
			NewChatMember: telegram.ChatMember{User: telegram.User{ID: 555}, Status: "member"},
		},
	}
	if err := service.HandleUpdate(ctx, update); err != nil {
		t.Fatalf("handle chat member: %v", err)
	}

	if got := api.countOf("editMessageText"); got != 1 {
		t.Fatalf("card must be closed once, got %d edits", got)
	}
	updated, err := repo.GetByToken(ctx, pending.Token)
	if err != nil {
		t.Fatalf("get by token: %v", err)
	}
	if updated.Status != store.StatusApproved {
		t.Fatalf("expected approved status, got %s", updated.Status)
	}
}

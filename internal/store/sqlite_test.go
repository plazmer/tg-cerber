package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestRepo(t *testing.T) *SQLiteRepository {
	t.Helper()

	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	t.Cleanup(func() { repo.Close() })
	return repo
}

func sampleRequest(token string) JoinRequest {
	return JoinRequest{
		Token:       token,
		GroupChatID: -1001,
		GroupTitle:  "Test group",
		UserID:      555,
		UserChatID:  555,
		UserLabel:   "Test User (@test)",
		Status:      StatusPending,
	}
}

func TestCreateRejectsSecondPendingForSameUser(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.Create(ctx, sampleRequest("first")); err != nil {
		t.Fatalf("create first: %v", err)
	}
	err := repo.Create(ctx, sampleRequest("second"))
	if !errors.Is(err, ErrDuplicatePending) {
		t.Fatalf("expected ErrDuplicatePending, got %v", err)
	}

	// После решения по первой заявке пользователь снова может подать заявку.
	if err := repo.SetStatus(ctx, "first", StatusDeclined); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if err := repo.Create(ctx, sampleRequest("third")); err != nil {
		t.Fatalf("create after decline: %v", err)
	}
}

func TestClaimStatusIsSingleShot(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.Create(ctx, sampleRequest("token")); err != nil {
		t.Fatalf("create: %v", err)
	}

	claimed, err := repo.ClaimStatus(ctx, "token", []Status{StatusPending}, StatusApproved)
	if err != nil || !claimed {
		t.Fatalf("first claim must succeed: claimed=%v err=%v", claimed, err)
	}
	claimed, err = repo.ClaimStatus(ctx, "token", []Status{StatusPending}, StatusRejected)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimed {
		t.Fatal("second claim must be rejected: заявка уже обработана")
	}

	request, err := repo.GetByToken(ctx, "token")
	if err != nil {
		t.Fatalf("get by token: %v", err)
	}
	if request.Status != StatusApproved {
		t.Fatalf("expected status approved, got %s", request.Status)
	}
}

func TestMarkUpdateProcessedDeduplicates(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	fresh, err := repo.MarkUpdateProcessed(ctx, 100)
	if err != nil || !fresh {
		t.Fatalf("first update must be fresh: fresh=%v err=%v", fresh, err)
	}
	fresh, err = repo.MarkUpdateProcessed(ctx, 100)
	if err != nil {
		t.Fatalf("second mark: %v", err)
	}
	if fresh {
		t.Fatal("repeated update_id must be reported as duplicate")
	}

	if err := repo.PurgeProcessedUpdates(ctx, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("purge: %v", err)
	}
	fresh, err = repo.MarkUpdateProcessed(ctx, 100)
	if err != nil || !fresh {
		t.Fatalf("after purge update must be fresh again: fresh=%v err=%v", fresh, err)
	}
}

func TestListPendingOlderThan(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.Create(ctx, sampleRequest("fresh")); err != nil {
		t.Fatalf("create: %v", err)
	}

	expired, err := repo.ListPendingOlderThan(ctx, time.Now().UTC().Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	if len(expired) != 0 {
		t.Fatalf("fresh request must not be expired, got %d", len(expired))
	}

	expired, err = repo.ListPendingOlderThan(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	if len(expired) != 1 || expired[0].Token != "fresh" {
		t.Fatalf("expected one expired request, got %+v", expired)
	}
}

func TestGetByTokenReturnsErrNoRows(t *testing.T) {
	repo := newTestRepo(t)

	_, err := repo.GetByToken(context.Background(), "missing")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}

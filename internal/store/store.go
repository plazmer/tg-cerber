package store

import (
	"context"
	"errors"
	"time"
)

// ErrDuplicatePending возвращается, если по паре (группа, пользователь)
// уже есть заявка в статусе pending.
var ErrDuplicatePending = errors.New("pending join request already exists")

type Status string

const (
	// StatusPending — заявка ждет решения владельца, в группу пользователь не допущен.
	StatusPending Status = "pending"
	// StatusApproved — заявка одобрена, пользователь в группе.
	StatusApproved Status = "approved"
	// StatusDeclined — заявка отклонена, пользователь может подать ее повторно.
	StatusDeclined Status = "declined"
	// StatusRejected — пользователь забанен, повторные заявки невозможны.
	StatusRejected Status = "rejected"
	// StatusUnbanned — бан снят, пользователь снова может подать заявку.
	StatusUnbanned Status = "unbanned"
	// StatusCancelled — заявка снята технически (например, не удалось уведомить владельца).
	StatusCancelled Status = "cancelled"
)

type JoinRequest struct {
	Token                string
	GroupChatID          int64
	GroupTitle           string
	GroupLink            string
	UserID               int64
	UserChatID           int64
	UserLabel            string
	Bio                  string
	OwnerStatusMessageID int64
	Status               Status
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type Repository interface {
	Create(ctx context.Context, r JoinRequest) error
	GetByToken(ctx context.Context, token string) (*JoinRequest, error)
	GetPendingByChatUser(ctx context.Context, chatID int64, userID int64) (*JoinRequest, error)
	GetPendingByUser(ctx context.Context, userID int64) (*JoinRequest, error)
	// ClaimStatus атомарно переводит заявку из одного из статусов from в статус to.
	// Возвращает false, если заявка уже в другом статусе (двойной клик, гонка).
	ClaimStatus(ctx context.Context, token string, from []Status, to Status) (bool, error)
	SetStatus(ctx context.Context, token string, status Status) error
	SetOwnerStatusMessageID(ctx context.Context, token string, messageID int64) error
	ListPendingOlderThan(ctx context.Context, cutoff time.Time, limit int) ([]JoinRequest, error)
	// MarkUpdateProcessed возвращает false, если update с таким id уже обрабатывался.
	MarkUpdateProcessed(ctx context.Context, updateID int64) (bool, error)
	PurgeProcessedUpdates(ctx context.Context, before time.Time) error
	Close() error
}

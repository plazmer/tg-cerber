package store

import (
	"context"
	"time"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusApproved  Status = "approved"
	StatusRejected  Status = "rejected"
	StatusUnbanned  Status = "unbanned"
	StatusCancelled Status = "cancelled"
)

type Verification struct {
	Token                string
	GroupChatID          int64
	UserID               int64
	GroupPromptMessageID int64
	GroupLink            string
	Status               Status
	CreatedAt            time.Time
	UpdatedAt            time.Time
	QuestionSent         bool
	OwnerStatusMessageID int64
}

type Repository interface {
	UpsertPending(ctx context.Context, v Verification) error
	GetByToken(ctx context.Context, token string) (*Verification, error)
	GetPendingByUser(ctx context.Context, userID int64) (*Verification, error)
	GetPendingByChatUser(ctx context.Context, chatID int64, userID int64) (*Verification, error)
	SanitizeDuplicatePending(ctx context.Context) ([]Verification, error)
	SetStatus(ctx context.Context, token string, status Status) error
	SetQuestionSent(ctx context.Context, token string, sent bool) error
	SetOwnerStatusMessageID(ctx context.Context, token string, messageID int64) error
	SetGroupPromptMessageID(ctx context.Context, token string, messageID int64) error
}

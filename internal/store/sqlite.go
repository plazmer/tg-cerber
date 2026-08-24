package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const sqliteTimeLayout = "2006-01-02 15:04:05"

type SQLiteRepository struct {
	db *sql.DB
}

func NewSQLiteRepository(path string) (*SQLiteRepository, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Один писатель: обработчики апдейтов и фоновый janitor ходят в БД параллельно.
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(context.Background(), `PRAGMA busy_timeout = 5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}
	if _, err := db.ExecContext(context.Background(), `PRAGMA journal_mode = WAL;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("set journal_mode: %w", err)
	}

	repo := &SQLiteRepository{db: db}
	if err := repo.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}

	return repo, nil
}

func (r *SQLiteRepository) Close() error {
	return r.db.Close()
}

// migrate создает схему нового контура заявок.
// Таблица pending_verifications от прежнего flow не трогается и не удаляется.
func (r *SQLiteRepository) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS join_requests (
	token TEXT PRIMARY KEY,
	group_chat_id INTEGER NOT NULL,
	group_title TEXT NOT NULL DEFAULT '',
	group_link TEXT NOT NULL DEFAULT '',
	user_id INTEGER NOT NULL,
	user_chat_id INTEGER NOT NULL DEFAULT 0,
	user_label TEXT NOT NULL DEFAULT '',
	bio TEXT NOT NULL DEFAULT '',
	owner_status_message_id INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_join_requests_user_status ON join_requests(user_id, status);
CREATE INDEX IF NOT EXISTS idx_join_requests_status_created ON join_requests(status, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS ux_join_requests_pending ON join_requests(group_chat_id, user_id) WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS processed_updates (
	update_id INTEGER PRIMARY KEY,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_processed_updates_created ON processed_updates(created_at);
`
	if _, err := r.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

const joinRequestColumns = `token, group_chat_id, group_title, group_link, user_id, user_chat_id, user_label, bio, owner_status_message_id, status, created_at, updated_at`

func (r *SQLiteRepository) Create(ctx context.Context, req JoinRequest) error {
	const query = `
INSERT INTO join_requests(token, group_chat_id, group_title, group_link, user_id, user_chat_id, user_label, bio, owner_status_message_id, status, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
ON CONFLICT(group_chat_id, user_id) WHERE status = 'pending' DO NOTHING;
`
	result, err := r.db.ExecContext(ctx, query,
		req.Token, req.GroupChatID, req.GroupTitle, req.GroupLink,
		req.UserID, req.UserChatID, req.UserLabel, req.Bio,
		req.OwnerStatusMessageID, string(req.Status))
	if err != nil {
		return fmt.Errorf("insert join request: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("insert join request rows affected: %w", err)
	}
	if affected == 0 {
		return ErrDuplicatePending
	}
	return nil
}

func (r *SQLiteRepository) GetByToken(ctx context.Context, token string) (*JoinRequest, error) {
	query := `SELECT ` + joinRequestColumns + ` FROM join_requests WHERE token = ?;`
	return scanJoinRequestRow(r.db.QueryRowContext(ctx, query, token))
}

func (r *SQLiteRepository) GetPendingByChatUser(ctx context.Context, chatID int64, userID int64) (*JoinRequest, error) {
	query := `SELECT ` + joinRequestColumns + `
FROM join_requests
WHERE group_chat_id = ? AND user_id = ? AND status = ?
ORDER BY created_at DESC
LIMIT 1;`
	return scanJoinRequestRow(r.db.QueryRowContext(ctx, query, chatID, userID, string(StatusPending)))
}

func (r *SQLiteRepository) GetPendingByUser(ctx context.Context, userID int64) (*JoinRequest, error) {
	query := `SELECT ` + joinRequestColumns + `
FROM join_requests
WHERE user_id = ? AND status = ?
ORDER BY created_at DESC
LIMIT 1;`
	return scanJoinRequestRow(r.db.QueryRowContext(ctx, query, userID, string(StatusPending)))
}

func (r *SQLiteRepository) ClaimStatus(ctx context.Context, token string, from []Status, to Status) (bool, error) {
	if len(from) == 0 {
		return false, errors.New("claim status: empty from list")
	}

	placeholders := make([]string, 0, len(from))
	args := make([]any, 0, len(from)+2)
	args = append(args, string(to), token)
	for _, status := range from {
		placeholders = append(placeholders, "?")
		args = append(args, string(status))
	}

	query := `
UPDATE join_requests
SET status = ?, updated_at = CURRENT_TIMESTAMP
WHERE token = ? AND status IN (` + strings.Join(placeholders, ", ") + `);`

	result, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("claim status: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim status rows affected: %w", err)
	}
	return affected > 0, nil
}

func (r *SQLiteRepository) SetStatus(ctx context.Context, token string, status Status) error {
	const query = `
UPDATE join_requests
SET status = ?, updated_at = CURRENT_TIMESTAMP
WHERE token = ?;
`
	result, err := r.db.ExecContext(ctx, query, string(status), token)
	if err != nil {
		return fmt.Errorf("set status: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set status rows affected: %w", err)
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (r *SQLiteRepository) SetOwnerStatusMessageID(ctx context.Context, token string, messageID int64) error {
	const query = `
UPDATE join_requests
SET owner_status_message_id = ?, updated_at = CURRENT_TIMESTAMP
WHERE token = ?;
`
	result, err := r.db.ExecContext(ctx, query, messageID, token)
	if err != nil {
		return fmt.Errorf("set owner status message id: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set owner status message id rows affected: %w", err)
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (r *SQLiteRepository) ListPendingOlderThan(ctx context.Context, cutoff time.Time, limit int) ([]JoinRequest, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT ` + joinRequestColumns + `
FROM join_requests
WHERE status = ? AND datetime(created_at) <= datetime(?)
ORDER BY created_at
LIMIT ?;`

	rows, err := r.db.QueryContext(ctx, query, string(StatusPending), cutoff.UTC().Format(sqliteTimeLayout), limit)
	if err != nil {
		return nil, fmt.Errorf("list expired pending: %w", err)
	}
	defer rows.Close()

	requests := make([]JoinRequest, 0)
	for rows.Next() {
		req, err := scanJoinRequestRow(rows)
		if err != nil {
			return nil, err
		}
		requests = append(requests, *req)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired pending: %w", err)
	}
	return requests, nil
}

func (r *SQLiteRepository) MarkUpdateProcessed(ctx context.Context, updateID int64) (bool, error) {
	const query = `INSERT INTO processed_updates(update_id) VALUES(?) ON CONFLICT(update_id) DO NOTHING;`
	result, err := r.db.ExecContext(ctx, query, updateID)
	if err != nil {
		return false, fmt.Errorf("mark update processed: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark update processed rows affected: %w", err)
	}
	return affected > 0, nil
}

func (r *SQLiteRepository) PurgeProcessedUpdates(ctx context.Context, before time.Time) error {
	const query = `DELETE FROM processed_updates WHERE datetime(created_at) <= datetime(?);`
	if _, err := r.db.ExecContext(ctx, query, before.UTC().Format(sqliteTimeLayout)); err != nil {
		return fmt.Errorf("purge processed updates: %w", err)
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJoinRequestRow(row rowScanner) (*JoinRequest, error) {
	var req JoinRequest
	var status string
	var createdRaw string
	var updatedRaw string
	if err := row.Scan(
		&req.Token, &req.GroupChatID, &req.GroupTitle, &req.GroupLink,
		&req.UserID, &req.UserChatID, &req.UserLabel, &req.Bio,
		&req.OwnerStatusMessageID, &status, &createdRaw, &updatedRaw,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("scan join request: %w", err)
	}
	req.Status = Status(status)

	createdAt, err := parseSQLiteTime(createdRaw)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	updatedAt, err := parseSQLiteTime(updatedRaw)
	if err != nil {
		return nil, fmt.Errorf("parse updated_at: %w", err)
	}
	req.CreatedAt = createdAt
	req.UpdatedAt = updatedAt
	return &req, nil
}

func parseSQLiteTime(value string) (time.Time, error) {
	layouts := []string{
		sqliteTimeLayout,
		time.RFC3339,
		time.RFC3339Nano,
	}
	for _, layout := range layouts {
		ts, err := time.Parse(layout, value)
		if err == nil {
			return ts, nil
		}
	}
	return time.Time{}, fmt.Errorf("unknown sqlite time format: %s", value)
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type SQLiteRepository struct {
	db *sql.DB
}

func NewSQLiteRepository(path string) (*SQLiteRepository, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	repo := &SQLiteRepository{db: db}
	if err := repo.migrate(context.Background()); err != nil {
		return nil, err
	}

	return repo, nil
}

func (r *SQLiteRepository) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS pending_verifications (
	token TEXT PRIMARY KEY,
	group_chat_id INTEGER NOT NULL,
	user_id INTEGER NOT NULL,
	status TEXT NOT NULL,
	question_sent INTEGER NOT NULL DEFAULT 0,
	owner_status_message_id INTEGER NOT NULL DEFAULT 0,
	group_prompt_message_id INTEGER NOT NULL DEFAULT 0,
	group_link TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_pending_group_user ON pending_verifications(group_chat_id, user_id);
CREATE INDEX IF NOT EXISTS idx_pending_user_status ON pending_verifications(user_id, status);
`
	if _, err := r.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	exists, err := r.columnExists(ctx, "pending_verifications", "owner_status_message_id")
	if err != nil {
		return fmt.Errorf("check owner_status_message_id existence: %w", err)
	}
	if !exists {
		if _, err := r.db.ExecContext(ctx, `ALTER TABLE pending_verifications ADD COLUMN owner_status_message_id INTEGER NOT NULL DEFAULT 0;`); err != nil {
			return fmt.Errorf("add owner_status_message_id: %w", err)
		}
	}
	exists, err = r.columnExists(ctx, "pending_verifications", "group_prompt_message_id")
	if err != nil {
		return fmt.Errorf("check group_prompt_message_id existence: %w", err)
	}
	if !exists {
		if _, err := r.db.ExecContext(ctx, `ALTER TABLE pending_verifications ADD COLUMN group_prompt_message_id INTEGER NOT NULL DEFAULT 0;`); err != nil {
			return fmt.Errorf("add group_prompt_message_id: %w", err)
		}
	}
	exists, err = r.columnExists(ctx, "pending_verifications", "group_link")
	if err != nil {
		return fmt.Errorf("check group_link existence: %w", err)
	}
	if !exists {
		if _, err := r.db.ExecContext(ctx, `ALTER TABLE pending_verifications ADD COLUMN group_link TEXT NOT NULL DEFAULT '';`); err != nil {
			return fmt.Errorf("add group_link: %w", err)
		}
	}
	return nil
}

func (r *SQLiteRepository) UpsertPending(ctx context.Context, v Verification) error {
	const query = `
INSERT INTO pending_verifications(token, group_chat_id, user_id, status, question_sent, owner_status_message_id, group_prompt_message_id, group_link, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
ON CONFLICT(token) DO UPDATE SET
	group_chat_id = excluded.group_chat_id,
	user_id = excluded.user_id,
	status = excluded.status,
	question_sent = excluded.question_sent,
	owner_status_message_id = excluded.owner_status_message_id,
	group_prompt_message_id = excluded.group_prompt_message_id,
	group_link = excluded.group_link,
	updated_at = CURRENT_TIMESTAMP;
`
	_, err := r.db.ExecContext(ctx, query, v.Token, v.GroupChatID, v.UserID, string(v.Status), boolToInt(v.QuestionSent), v.OwnerStatusMessageID, v.GroupPromptMessageID, v.GroupLink)
	if err != nil {
		return fmt.Errorf("upsert pending: %w", err)
	}
	return nil
}

func (r *SQLiteRepository) GetByToken(ctx context.Context, token string) (*Verification, error) {
	const query = `
SELECT token, group_chat_id, user_id, status, question_sent, owner_status_message_id, group_prompt_message_id, group_link, created_at, updated_at
FROM pending_verifications
WHERE token = ?;
`
	row := r.db.QueryRowContext(ctx, query, token)
	ver, err := scanVerification(row)
	if err != nil {
		return nil, err
	}
	return ver, nil
}

func (r *SQLiteRepository) GetPendingByUser(ctx context.Context, userID int64) (*Verification, error) {
	const query = `
SELECT token, group_chat_id, user_id, status, question_sent, owner_status_message_id, group_prompt_message_id, group_link, created_at, updated_at
FROM pending_verifications
WHERE user_id = ? AND status = ?
ORDER BY created_at DESC
LIMIT 1;
`
	row := r.db.QueryRowContext(ctx, query, userID, string(StatusPending))
	ver, err := scanVerification(row)
	if err != nil {
		return nil, err
	}
	return ver, nil
}

func (r *SQLiteRepository) SetStatus(ctx context.Context, token string, status Status) error {
	const query = `
UPDATE pending_verifications
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

func (r *SQLiteRepository) SetQuestionSent(ctx context.Context, token string, sent bool) error {
	const query = `
UPDATE pending_verifications
SET question_sent = ?, updated_at = CURRENT_TIMESTAMP
WHERE token = ?;
`
	result, err := r.db.ExecContext(ctx, query, boolToInt(sent), token)
	if err != nil {
		return fmt.Errorf("set question sent: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set question sent rows affected: %w", err)
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (r *SQLiteRepository) SetOwnerStatusMessageID(ctx context.Context, token string, messageID int64) error {
	const query = `
UPDATE pending_verifications
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

func (r *SQLiteRepository) SetGroupPromptMessageID(ctx context.Context, token string, messageID int64) error {
	const query = `
UPDATE pending_verifications
SET group_prompt_message_id = ?, updated_at = CURRENT_TIMESTAMP
WHERE token = ?;
`
	result, err := r.db.ExecContext(ctx, query, messageID, token)
	if err != nil {
		return fmt.Errorf("set group prompt message id: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set group prompt message id rows affected: %w", err)
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func scanVerification(row *sql.Row) (*Verification, error) {
	var v Verification
	var status string
	var createdRaw string
	var updatedRaw string
	var sentInt int
	if err := row.Scan(&v.Token, &v.GroupChatID, &v.UserID, &status, &sentInt, &v.OwnerStatusMessageID, &v.GroupPromptMessageID, &v.GroupLink, &createdRaw, &updatedRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("scan verification: %w", err)
	}
	v.Status = Status(status)
	v.QuestionSent = sentInt == 1
	createdAt, err := parseSQLiteTime(createdRaw)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	updatedAt, err := parseSQLiteTime(updatedRaw)
	if err != nil {
		return nil, fmt.Errorf("parse updated_at: %w", err)
	}
	v.CreatedAt = createdAt
	v.UpdatedAt = updatedAt
	return &v, nil
}

func parseSQLiteTime(value string) (time.Time, error) {
	layouts := []string{
		"2006-01-02 15:04:05",
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

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (r *SQLiteRepository) columnExists(ctx context.Context, table string, column string) (bool, error) {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notnull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dfltValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS notifications (
	id              TEXT PRIMARY KEY,
	idempotency_key TEXT UNIQUE,
	target_url      TEXT NOT NULL,
	method          TEXT NOT NULL,
	headers         TEXT NOT NULL,
	body            TEXT NOT NULL,
	status          TEXT NOT NULL CHECK (status IN ('pending','delivering','succeeded','dead')),
	attempts        INTEGER NOT NULL DEFAULT 0,
	next_attempt_at INTEGER NOT NULL,
	claimed_at      INTEGER,
	last_error      TEXT NOT NULL DEFAULT '',
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_notifications_due ON notifications (status, next_attempt_at);
`

type SQLite struct {
	db *sql.DB
}

// OpenSQLite 打开（必要时创建）SQLite 存储，WAL 模式（spec §6）。
func OpenSQLite(path string) (*SQLite, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// 单连接串行化所有语句：v1 流量下最简单的零 SQLITE_BUSY 方案。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化 schema: %w", err)
	}
	return &SQLite{db: db}, nil
}

func (s *SQLite) Close() error { return s.db.Close() }

const allColumns = `id, idempotency_key, target_url, method, headers, body,
	status, attempts, next_attempt_at, claimed_at, last_error, created_at, updated_at`

func (s *SQLite) Create(ctx context.Context, n *Notification, now time.Time) (*Notification, bool, error) {
	headers, err := json.Marshal(n.Headers)
	if err != nil {
		return nil, false, fmt.Errorf("序列化 headers: %w", err)
	}

	stored := *n
	stored.ID = newID()
	stored.Status = StatusPending
	stored.Attempts = 0
	stored.NextAttemptAt = now
	stored.CreatedAt = now
	stored.UpdatedAt = now

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO notifications (`+allColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '', ?, ?)`,
		stored.ID, nullable(stored.IdempotencyKey), stored.TargetURL, stored.Method,
		string(headers), stored.Body, stored.Status, stored.Attempts,
		ms(stored.NextAttemptAt), ms(stored.CreatedAt), ms(stored.UpdatedAt))
	if err != nil {
		if isUniqueViolation(err) && stored.IdempotencyKey != "" {
			return s.resolveIdempotencyHit(ctx, n)
		}
		return nil, false, err
	}
	return &stored, false, nil
}

// resolveIdempotencyHit 处理幂等键命中：payload 一致 → 幂等重放；否则冲突（spec §3.1）。
func (s *SQLite) resolveIdempotencyHit(ctx context.Context, n *Notification) (*Notification, bool, error) {
	existing, err := s.getBy(ctx, "idempotency_key = ?", n.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	if existing.TargetURL != n.TargetURL || existing.Method != n.Method ||
		existing.Body != n.Body || !mapsEqual(existing.Headers, n.Headers) {
		return nil, false, ErrIdempotencyConflict
	}
	return existing, true, nil
}

func (s *SQLite) Get(ctx context.Context, id string) (*Notification, error) {
	return s.getBy(ctx, "id = ?", id)
}

func (s *SQLite) ClaimDue(ctx context.Context, now time.Time, limit int) ([]*Notification, error) {
	rows, err := s.db.QueryContext(ctx, `
		UPDATE notifications
		SET status = ?, claimed_at = ?, updated_at = ?
		WHERE id IN (
			SELECT id FROM notifications
			WHERE status = ? AND next_attempt_at <= ?
			ORDER BY next_attempt_at
			LIMIT ?
		)
		RETURNING `+allColumns,
		StatusDelivering, ms(now), ms(now), StatusPending, ms(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var claimed []*Notification
	for rows.Next() {
		n, err := scan(rows)
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, n)
	}
	return claimed, rows.Err()
}

func (s *SQLite) MarkSucceeded(ctx context.Context, id string, now time.Time) error {
	return s.finishAttempt(ctx, id, StatusSucceeded, nil, "", now)
}

func (s *SQLite) MarkRetry(ctx context.Context, id string, nextAttemptAt time.Time, lastError string, now time.Time) error {
	return s.finishAttempt(ctx, id, StatusPending, &nextAttemptAt, lastError, now)
}

func (s *SQLite) MarkDead(ctx context.Context, id string, lastError string, now time.Time) error {
	return s.finishAttempt(ctx, id, StatusDead, nil, lastError, now)
}

func (s *SQLite) finishAttempt(ctx context.Context, id, status string, nextAt *time.Time, lastError string, now time.Time) error {
	next := int64(0)
	if nextAt != nil {
		next = ms(*nextAt)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE notifications
		SET status = ?, attempts = attempts + 1, next_attempt_at = ?,
		    claimed_at = NULL, last_error = ?, updated_at = ?
		WHERE id = ?`,
		status, next, truncate(lastError, 1024), ms(now), id)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

func (s *SQLite) ReclaimStale(ctx context.Context, claimedBefore time.Time, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE notifications
		SET status = ?, claimed_at = NULL, updated_at = ?
		WHERE status = ? AND claimed_at <= ?`,
		StatusPending, ms(now), StatusDelivering, ms(claimedBefore))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *SQLite) Redeliver(ctx context.Context, id string, now time.Time) (*Notification, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE notifications
		SET status = ?, attempts = 0, next_attempt_at = ?, last_error = '', updated_at = ?
		WHERE id = ? AND status = ?`,
		StatusPending, ms(now), ms(now), id, StatusDead)
	if err != nil {
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		if _, err := s.Get(ctx, id); err != nil {
			return nil, err // ErrNotFound
		}
		return nil, ErrNotDead
	}
	return s.Get(ctx, id)
}

// --- 内部工具 ---

type rowScanner interface{ Scan(dest ...any) error }

func (s *SQLite) getBy(ctx context.Context, where string, arg any) (*Notification, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+allColumns+` FROM notifications WHERE `+where, arg)
	n, err := scan(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return n, err
}

func scan(r rowScanner) (*Notification, error) {
	var n Notification
	var idemKey sql.NullString
	var headers string
	var nextAt, createdAt, updatedAt int64
	var claimedAt sql.NullInt64

	err := r.Scan(&n.ID, &idemKey, &n.TargetURL, &n.Method, &headers, &n.Body,
		&n.Status, &n.Attempts, &nextAt, &claimedAt, &n.LastError, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	n.IdempotencyKey = idemKey.String
	if err := json.Unmarshal([]byte(headers), &n.Headers); err != nil {
		return nil, fmt.Errorf("反序列化 headers: %w", err)
	}
	n.NextAttemptAt = fromMS(nextAt)
	n.CreatedAt = fromMS(createdAt)
	n.UpdatedAt = fromMS(updatedAt)
	if claimedAt.Valid {
		t := fromMS(claimedAt.Int64)
		n.ClaimedAt = &t
	}
	return &n, nil
}

func ms(t time.Time) int64     { return t.UnixMilli() }
func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

func requireOneRow(res sql.Result) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

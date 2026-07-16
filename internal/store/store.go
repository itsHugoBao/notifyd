// Package store 提供通知的持久化队列（spec §4）。
// 接口不泄漏 SQL 细节，为演进第 1 步（SQLite → Postgres）留缝（spec §8）。
package store

import (
	"context"
	"errors"
	"time"
)

// 通知状态机（spec §4）。
const (
	StatusPending    = "pending"
	StatusDelivering = "delivering"
	StatusSucceeded  = "succeeded"
	StatusDead       = "dead"
)

var (
	ErrNotFound = errors.New("notification not found")
	// ErrIdempotencyConflict：幂等键重复但 payload 不一致（spec §3.1 → 409）。
	ErrIdempotencyConflict = errors.New("idempotency key reused with different payload")
	// ErrNotDead：redeliver 的目标不处于 dead 状态（spec §3.3 → 409）。
	ErrNotDead = errors.New("notification is not dead")
)

type Notification struct {
	ID             string
	IdempotencyKey string // 空串 = 未提供
	TargetURL      string
	Method         string
	Headers        map[string]string
	Body           string
	Status         string
	Attempts       int
	NextAttemptAt  time.Time
	ClaimedAt      *time.Time
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Store interface {
	// Create 持久化一条新通知。幂等键已存在时：payload 一致返回已有记录
	// （existed=true），不一致返回 ErrIdempotencyConflict。
	Create(ctx context.Context, n *Notification, now time.Time) (stored *Notification, existed bool, err error)

	Get(ctx context.Context, id string) (*Notification, error)

	// ClaimDue 原子认领至多 limit 条到期任务（pending → delivering，spec §6）。
	ClaimDue(ctx context.Context, now time.Time, limit int) ([]*Notification, error)

	// 一次投递尝试的三种落点（均使 attempts+1，spec §5.2）。
	MarkSucceeded(ctx context.Context, id string, now time.Time) error
	MarkRetry(ctx context.Context, id string, nextAttemptAt time.Time, lastError string, now time.Time) error
	MarkDead(ctx context.Context, id string, lastError string, now time.Time) error

	// ReclaimStale 回收认领超时的 delivering 任务（崩溃恢复，spec §5.5）。
	ReclaimStale(ctx context.Context, claimedBefore time.Time, now time.Time) (int64, error)

	// Redeliver 将 dead 通知重置为 pending 并重置重试预算（spec §3.3）。
	Redeliver(ctx context.Context, id string, now time.Time) (*Notification, error)

	Close() error
}

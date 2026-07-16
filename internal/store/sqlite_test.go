package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf8"
)

func newTestStore(t *testing.T) *SQLite {
	t.Helper()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sample(key string) *Notification {
	return &Notification{
		IdempotencyKey: key,
		TargetURL:      "https://vendor.example.com/hook",
		Method:         "POST",
		Headers:        map[string]string{"Content-Type": "application/json"},
		Body:           `{"user_id":42}`,
	}
}

var now = time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

func TestCreateAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	stored, existed, err := s.Create(ctx, sample(""), now)
	if err != nil || existed {
		t.Fatalf("Create: err=%v existed=%v", err, existed)
	}
	if stored.Status != StatusPending || stored.Attempts != 0 || !stored.NextAttemptAt.Equal(now) {
		t.Fatalf("新记录初始状态不对: %+v", stored)
	}

	got, err := s.Get(ctx, stored.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TargetURL != stored.TargetURL || got.Headers["Content-Type"] != "application/json" {
		t.Fatalf("读回不一致: %+v", got)
	}

	if _, err := s.Get(ctx, "no-such-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound, 得到 %v", err)
	}
}

func TestIdempotencyKeyBranches(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	first, _, err := s.Create(ctx, sample("key-1"), now)
	if err != nil {
		t.Fatalf("首次创建: %v", err)
	}

	// 同 key 同 payload → 返回已有记录（spec §3.1 幂等重放）
	replay, existed, err := s.Create(ctx, sample("key-1"), now.Add(time.Minute))
	if err != nil || !existed || replay.ID != first.ID {
		t.Fatalf("幂等重放: err=%v existed=%v id=%s(期望 %s)", err, existed, replay.ID, first.ID)
	}

	// 同 key 不同 payload → 冲突（spec §3.1 → 409）
	changed := sample("key-1")
	changed.Body = `{"user_id":43}`
	if _, _, err := s.Create(ctx, changed, now); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("期望 ErrIdempotencyConflict, 得到 %v", err)
	}

	// 无 key 的记录互不影响（NULL 不参与 UNIQUE）
	if _, existed, err := s.Create(ctx, sample(""), now); err != nil || existed {
		t.Fatalf("无 key 创建: err=%v existed=%v", err, existed)
	}
	if _, existed, err := s.Create(ctx, sample(""), now); err != nil || existed {
		t.Fatalf("第二条无 key 创建: err=%v existed=%v", err, existed)
	}
}

func TestClaimDue(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	due, _, _ := s.Create(ctx, sample(""), now.Add(-time.Minute))
	_, _, _ = s.Create(ctx, sample(""), now.Add(time.Hour)) // 未到期

	claimed, err := s.ClaimDue(ctx, now, 10)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != due.ID {
		t.Fatalf("应只认领到期的 1 条, 得到 %d 条", len(claimed))
	}
	if claimed[0].Status != StatusDelivering || claimed[0].ClaimedAt == nil {
		t.Fatalf("认领后状态不对: %+v", claimed[0])
	}

	// 已认领的不会被重复认领
	again, err := s.ClaimDue(ctx, now, 10)
	if err != nil || len(again) != 0 {
		t.Fatalf("重复认领: err=%v n=%d", err, len(again))
	}
}

func TestAttemptOutcomes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	n, _, _ := s.Create(ctx, sample(""), now)
	s.ClaimDue(ctx, now, 1)

	next := now.Add(10 * time.Second)
	if err := s.MarkRetry(ctx, n.ID, next, "503 from vendor", now); err != nil {
		t.Fatalf("MarkRetry: %v", err)
	}
	got, _ := s.Get(ctx, n.ID)
	if got.Status != StatusPending || got.Attempts != 1 ||
		!got.NextAttemptAt.Equal(next) || got.LastError != "503 from vendor" || got.ClaimedAt != nil {
		t.Fatalf("MarkRetry 后状态不对: %+v", got)
	}

	s.ClaimDue(ctx, next, 1)
	if err := s.MarkSucceeded(ctx, n.ID, next); err != nil {
		t.Fatalf("MarkSucceeded: %v", err)
	}
	got, _ = s.Get(ctx, n.ID)
	if got.Status != StatusSucceeded || got.Attempts != 2 {
		t.Fatalf("MarkSucceeded 后状态不对: %+v", got)
	}

	if err := s.MarkDead(ctx, "no-such-id", "x", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound, 得到 %v", err)
	}

	// 迟到写回守卫：任务已不在 delivering 时 Mark* 必须失败、不得覆盖新状态
	if err := s.MarkRetry(ctx, n.ID, now, "late", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("对 succeeded 的迟到写回应被拒绝, 得到 %v", err)
	}
	got, _ = s.Get(ctx, n.ID)
	if got.Status != StatusSucceeded || got.Attempts != 2 {
		t.Fatalf("迟到写回不应改动记录: %+v", got)
	}
}

func TestTruncateKeepsUTF8(t *testing.T) {
	s := "永久失败: 供应商返回异常"
	cut := truncate(s, 16) // "供"占 [14,17)，16 落在其中间
	if !utf8.ValidString(cut) {
		t.Fatalf("截断产生非法 UTF-8: %q", cut)
	}
	if cut != "永久失败: " { // 应回退到 14（"供"之前）
		t.Fatalf("截断位置不对: %q", cut)
	}
}

func TestReclaimStale(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	n, _, _ := s.Create(ctx, sample(""), now)
	s.ClaimDue(ctx, now, 1)

	// 可见性超时 60s：认领后 30s 不回收，90s 回收（spec §5.5）
	reclaimed, err := s.ReclaimStale(ctx, now.Add(30*time.Second).Add(-60*time.Second), now.Add(30*time.Second))
	if err != nil || reclaimed != 0 {
		t.Fatalf("未超时不应回收: err=%v n=%d", err, reclaimed)
	}

	reclaimed, err = s.ReclaimStale(ctx, now.Add(90*time.Second).Add(-60*time.Second), now.Add(90*time.Second))
	if err != nil || reclaimed != 1 {
		t.Fatalf("超时应回收 1 条: err=%v n=%d", err, reclaimed)
	}
	got, _ := s.Get(ctx, n.ID)
	if got.Status != StatusPending || got.ClaimedAt != nil {
		t.Fatalf("回收后状态不对: %+v", got)
	}
}

func TestRedeliver(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	n, _, _ := s.Create(ctx, sample(""), now)

	// 非 dead → ErrNotDead
	if _, err := s.Redeliver(ctx, n.ID, now); !errors.Is(err, ErrNotDead) {
		t.Fatalf("期望 ErrNotDead, 得到 %v", err)
	}

	s.ClaimDue(ctx, now, 1)
	s.MarkDead(ctx, n.ID, "budget exhausted", now)

	later := now.Add(time.Hour)
	revived, err := s.Redeliver(ctx, n.ID, later)
	if err != nil {
		t.Fatalf("Redeliver: %v", err)
	}
	if revived.Status != StatusPending || revived.Attempts != 0 ||
		!revived.NextAttemptAt.Equal(later) || revived.LastError != "" {
		t.Fatalf("重放后状态不对: %+v", revived)
	}

	if _, err := s.Redeliver(ctx, "no-such-id", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound, 得到 %v", err)
	}
}

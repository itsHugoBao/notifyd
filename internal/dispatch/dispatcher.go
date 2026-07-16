package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"rc_hugobao/internal/config"
	"rc_hugobao/internal/store"
)

type Dispatcher struct {
	store    store.Store
	cfg      config.Config
	client   *http.Client
	logger   *slog.Logger
	now      func() time.Time
	inflight atomic.Int64
}

func New(s store.Store, cfg config.Config, logger *slog.Logger) *Dispatcher {
	return &Dispatcher{
		store: s,
		cfg:   cfg,
		client: &http.Client{
			// 不跟随重定向：3xx 原样返回并按 §5.2 分类为永久失败（spec §3.4）。
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger: logger,
		now:    time.Now,
	}
}

// Run 阻塞运行投递循环，直到 ctx 取消；返回前等待在途投递完成（spec §6 优雅停机）。
func (d *Dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()

	var wg sync.WaitGroup
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-ticker.C:
			d.reclaimStale(ctx)
			for _, n := range d.claim(ctx) {
				wg.Add(1)
				d.inflight.Add(1)
				go func(n *store.Notification) {
					defer wg.Done()
					defer d.inflight.Add(-1)
					d.deliver(ctx, n)
				}(n)
			}
		}
	}
}

// claim 只认领当前有空闲 worker 的量，避免任务在本地排队占着 delivering 状态。
func (d *Dispatcher) claim(ctx context.Context) []*store.Notification {
	free := d.cfg.Workers - int(d.inflight.Load())
	if free <= 0 {
		return nil
	}
	batch, err := d.store.ClaimDue(ctx, d.now().UTC(), free)
	if err != nil {
		if ctx.Err() == nil {
			d.logger.Error("认领任务失败", "err", err)
		}
		return nil
	}
	return batch
}

func (d *Dispatcher) reclaimStale(ctx context.Context) {
	now := d.now().UTC()
	n, err := d.store.ReclaimStale(ctx, now.Add(-d.cfg.VisibilityTimeout), now)
	if err != nil {
		if ctx.Err() == nil {
			d.logger.Error("回收超时任务失败", "err", err)
		}
		return
	}
	if n > 0 {
		d.logger.Warn("回收了认领超时的任务（疑似进程崩溃残留）", "count", n)
	}
}

// writeBackTimeout 是投递结果写库的独立超时。写库绝不能复用 attemptCtx：
// 尝试耗尽 AttemptTimeout 后 attemptCtx 已 DeadlineExceeded，复用会导致结果
// 写不回、任务卡在 delivering、attempts 不消耗——持续超时的供应商将无限重投。
const writeBackTimeout = 5 * time.Second

// deliver 执行一次投递尝试并按结果推进状态机（spec §5.2）。
// 使用 WithoutCancel：停机时在途尝试跑完（受单次超时约束），而不是被腰斩。
func (d *Dispatcher) deliver(ctx context.Context, n *store.Notification) {
	attemptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.cfg.AttemptTimeout)
	defer cancel()

	statusCode, attemptErr := d.attempt(attemptCtx, n)
	outcome := Classify(statusCode, attemptErr)

	writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), writeBackTimeout)
	defer writeCancel()

	errMsg := ""
	if attemptErr != nil {
		errMsg = attemptErr.Error()
	} else if outcome != OutcomeSuccess {
		errMsg = fmt.Sprintf("HTTP %d", statusCode)
	}

	attemptNum := n.Attempts + 1
	now := d.now().UTC()
	var storeErr error
	switch {
	case outcome == OutcomeSuccess:
		storeErr = d.store.MarkSucceeded(writeCtx, n.ID, now)
		d.logger.Info("投递成功", "id", n.ID, "attempt", attemptNum)
	case outcome == OutcomePermanent:
		storeErr = d.store.MarkDead(writeCtx, n.ID, "永久失败: "+errMsg, now)
		d.logger.Warn("永久失败，进入死信", "id", n.ID, "err", errMsg)
	case attemptNum >= d.cfg.MaxAttempts:
		storeErr = d.store.MarkDead(writeCtx, n.ID, "重试预算耗尽: "+errMsg, now)
		d.logger.Warn("重试预算耗尽，进入死信", "id", n.ID, "attempts", attemptNum, "err", errMsg)
	default:
		delay := Backoff(d.cfg.RetryBase, d.cfg.RetryCap, attemptNum)
		storeErr = d.store.MarkRetry(writeCtx, n.ID, now.Add(delay), errMsg, now)
		d.logger.Info("投递失败，安排重试", "id", n.ID, "attempt", attemptNum, "next_in", delay.Round(time.Millisecond), "err", errMsg)
	}
	switch {
	case errors.Is(storeErr, store.ErrNotFound):
		// 任务已不在 delivering（可见性超时被回收并由他人推进）——迟到的写回按无效处理。
		d.logger.Warn("写回被忽略：任务已被回收重投", "id", n.ID)
	case storeErr != nil:
		// 写回失败：任务保持 delivering，由可见性超时回收重投（at-least-once）。
		d.logger.Error("写回投递结果失败", "id", n.ID, "err", storeErr)
	}
}

// attempt 发出实际的 HTTP 请求（spec §3.4：原样透传 + X-Notification-Id）。
func (d *Dispatcher) attempt(ctx context.Context, n *store.Notification) (int, error) {
	req, err := http.NewRequestWithContext(ctx, n.Method, n.TargetURL, strings.NewReader(n.Body))
	if err != nil {
		return 0, err
	}
	for k, v := range n.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Notification-Id", n.ID)

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// 响应体内容无关紧要（调用方不关心返回值），排干以复用连接。
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}

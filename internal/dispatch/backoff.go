// Package dispatch 实现投递引擎：轮询认领、并发投递、退避重试（spec §5、§6）。
package dispatch

import (
	"math/rand/v2"
	"time"
)

// Backoff 计算第 attempt 次失败后的重试延迟（attempt 从 1 起）：
// min(base·2^(attempt-1), cap) ± 20% 抖动（spec §5.3）。
func Backoff(base, cap time.Duration, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt && d < cap; i++ {
		d *= 2
	}
	if d > cap {
		d = cap
	}
	jitter := 0.8 + 0.4*rand.Float64()
	return time.Duration(float64(d) * jitter)
}

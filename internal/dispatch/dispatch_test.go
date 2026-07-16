package dispatch

import (
	"errors"
	"testing"
	"time"
)

func TestBackoffGrowthAndCap(t *testing.T) {
	base, cap := 5*time.Second, time.Hour

	for attempt, want := range map[int]time.Duration{
		1: 5 * time.Second,
		2: 10 * time.Second,
		3: 20 * time.Second,
		9: 1280 * time.Second, // 尚未到 cap
	} {
		got := Backoff(base, cap, attempt)
		lo, hi := time.Duration(float64(want)*0.8), time.Duration(float64(want)*1.2)
		if got < lo || got > hi {
			t.Errorf("attempt %d: %v 不在 [%v, %v] 内", attempt, got, lo, hi)
		}
	}

	// 高次数封顶在 cap ± 20%
	got := Backoff(base, cap, 50)
	if got < time.Duration(float64(cap)*0.8) || got > time.Duration(float64(cap)*1.2) {
		t.Errorf("attempt 50 应封顶于 cap±20%%, 得到 %v", got)
	}

	// 非法 attempt 按 1 处理
	if got := Backoff(base, cap, 0); got > time.Duration(float64(base)*1.2) {
		t.Errorf("attempt 0 应等价于 1, 得到 %v", got)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		status int
		err    error
		want   Outcome
	}{
		{"200", 200, nil, OutcomeSuccess},
		{"204", 204, nil, OutcomeSuccess},
		{"网络错误", 0, errors.New("connection refused"), OutcomeRetryable},
		{"408 请求超时", 408, nil, OutcomeRetryable},
		{"425 too early", 425, nil, OutcomeRetryable},
		{"429 限流", 429, nil, OutcomeRetryable},
		{"500", 500, nil, OutcomeRetryable},
		{"503", 503, nil, OutcomeRetryable},
		{"400 → 永久", 400, nil, OutcomePermanent},
		{"401 → 永久", 401, nil, OutcomePermanent},
		{"404 → 永久", 404, nil, OutcomePermanent},
		{"301 → 永久（不跟随重定向）", 301, nil, OutcomePermanent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.status, tc.err); got != tc.want {
				t.Errorf("Classify(%d, %v) = %v, 期望 %v", tc.status, tc.err, got, tc.want)
			}
		})
	}
}

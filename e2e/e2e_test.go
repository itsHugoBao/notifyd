// Package e2e 对完整服务栈（store + dispatcher + api）做端到端验证（spec §10）。
// 真实组件、真实 HTTP，只有"供应商"是 httptest 假服务；重试参数调小到毫秒级。
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rc_hugobao/internal/api"
	"rc_hugobao/internal/config"
	"rc_hugobao/internal/dispatch"
	"rc_hugobao/internal/store"
)

func testConfig() config.Config {
	return config.Config{
		Workers:           4,
		PollInterval:      20 * time.Millisecond,
		AttemptTimeout:    2 * time.Second,
		RetryBase:         30 * time.Millisecond,
		RetryCap:          200 * time.Millisecond,
		MaxAttempts:       4,
		VisibilityTimeout: 10 * time.Second,
		MaxBodyBytes:      256 * 1024,
	}
}

// vendor 是脚本化的假供应商：依次返回 script 中的状态码，之后重复最后一个。
type vendor struct {
	mu       sync.Mutex
	script   []int
	requests []recordedRequest
	srv      *httptest.Server
}

type recordedRequest struct {
	method, path, body string
	header             http.Header
}

func newVendor(t *testing.T, script ...int) *vendor {
	v := &vendor{script: script}
	v.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		v.mu.Lock()
		v.requests = append(v.requests, recordedRequest{r.Method, r.URL.Path, string(body), r.Header.Clone()})
		idx := len(v.requests) - 1
		if idx >= len(v.script) {
			idx = len(v.script) - 1
		}
		code := v.script[idx]
		v.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *vendor) calls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.requests)
}

type harness struct {
	apiURL string
	store  store.Store
}

// newHarness 组装完整服务栈。startDispatcher=false 用于需要先手工制造中间状态的场景。
func newHarness(t *testing.T, cfg config.Config, startDispatcher bool) *harness {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	apiSrv := httptest.NewServer(api.NewHandler(st, logger, cfg.MaxBodyBytes).Mux())
	t.Cleanup(apiSrv.Close)

	h := &harness{apiURL: apiSrv.URL, store: st}
	if startDispatcher {
		h.startDispatcher(t, cfg)
	}
	return h
}

func (h *harness) startDispatcher(t *testing.T, cfg config.Config) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		dispatch.New(h.store, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func (h *harness) submit(t *testing.T, targetURL string) string {
	t.Helper()
	payload := fmt.Sprintf(`{"target_url":"%s","headers":{"X-Biz":"crm"},"body":"{\"event\":\"paid\"}"}`, targetURL)
	resp, err := http.Post(h.apiURL+"/api/notifications", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("提交通知: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("提交: 期望 202, 得到 %d", resp.StatusCode)
	}
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return m["id"].(string)
}

// waitStatus 轮询直到通知到达期望状态（终态判定都走对外 API）。
func (h *harness) waitStatus(t *testing.T, id, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		resp, err := http.Get(h.apiURL + "/api/notifications/" + id)
		if err != nil {
			t.Fatalf("查询状态: %v", err)
		}
		json.NewDecoder(resp.Body).Decode(&last)
		resp.Body.Close()
		if last["status"] == want {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待状态 %q 超时, 最后状态: %v", want, last)
	return nil
}

func TestRetryThenSucceed(t *testing.T) {
	v := newVendor(t, 500, 503, 200)
	h := newHarness(t, testConfig(), true)

	id := h.submit(t, v.srv.URL+"/hook")
	got := h.waitStatus(t, id, "succeeded")
	if got["attempts"].(float64) != 3 {
		t.Fatalf("期望 3 次尝试, 得到 %v", got["attempts"])
	}
}

func TestPermanent4xxDeadImmediately(t *testing.T) {
	v := newVendor(t, 400)
	h := newHarness(t, testConfig(), true)

	id := h.submit(t, v.srv.URL+"/hook")
	got := h.waitStatus(t, id, "dead")
	if got["attempts"].(float64) != 1 {
		t.Fatalf("4xx 应一次即死信, 得到 %v 次", got["attempts"])
	}
	if !strings.Contains(got["last_error"].(string), "400") {
		t.Fatalf("last_error 应包含状态码: %v", got["last_error"])
	}
}

func TestBudgetExhaustedThenRedeliver(t *testing.T) {
	v := newVendor(t, 500) // 持续 500
	h := newHarness(t, testConfig(), true)

	id := h.submit(t, v.srv.URL+"/hook")
	got := h.waitStatus(t, id, "dead")
	if got["attempts"].(float64) != 4 {
		t.Fatalf("期望耗尽预算 4 次, 得到 %v", got["attempts"])
	}

	// 供应商恢复后人工重放（spec §5.4）
	v.mu.Lock()
	v.script = []int{200}
	v.requests = nil
	v.mu.Unlock()

	resp, err := http.Post(h.apiURL+"/api/notifications/"+id+"/redeliver", "application/json", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("redeliver: err=%v status=%d", err, resp.StatusCode)
	}
	resp.Body.Close()
	h.waitStatus(t, id, "succeeded")
}

func TestIdempotencyEndToEnd(t *testing.T) {
	v := newVendor(t, 200)
	h := newHarness(t, testConfig(), true)

	payload := fmt.Sprintf(`{"target_url":"%s/hook","body":"b","idempotency_key":"evt-1"}`, v.srv.URL)
	first, m1 := postJSON(t, h.apiURL+"/api/notifications", payload)
	replay, m2 := postJSON(t, h.apiURL+"/api/notifications", payload)
	if first.StatusCode != 202 || replay.StatusCode != 200 || m1["id"] != m2["id"] {
		t.Fatalf("幂等重放: %d/%d id %v vs %v", first.StatusCode, replay.StatusCode, m1["id"], m2["id"])
	}

	conflict, _ := postJSON(t, h.apiURL+"/api/notifications",
		fmt.Sprintf(`{"target_url":"%s/hook","body":"different","idempotency_key":"evt-1"}`, v.srv.URL))
	if conflict.StatusCode != 409 {
		t.Fatalf("幂等冲突: 期望 409, 得到 %d", conflict.StatusCode)
	}

	h.waitStatus(t, m1["id"].(string), "succeeded")
	if v.calls() != 1 {
		t.Fatalf("重复提交不应造成重复投递, 供应商收到 %d 次", v.calls())
	}
}

func TestCrashRecovery(t *testing.T) {
	v := newVendor(t, 200)
	cfg := testConfig()
	cfg.VisibilityTimeout = 100 * time.Millisecond
	h := newHarness(t, cfg, false) // 先不启动 dispatcher

	id := h.submit(t, v.srv.URL+"/hook")

	// 模拟 worker 认领后进程崩溃：手工认领，任务卡在 delivering
	claimed, err := h.store.ClaimDue(context.Background(), time.Now().UTC(), 1)
	if err != nil || len(claimed) != 1 || claimed[0].ID != id {
		t.Fatalf("手工认领失败: err=%v n=%d", err, len(claimed))
	}

	// "重启"服务：新 dispatcher 应在可见性超时后回收并完成投递（spec §5.5）
	h.startDispatcher(t, cfg)
	h.waitStatus(t, id, "succeeded")
}

func TestVendorReceivesVerbatimRequest(t *testing.T) {
	v := newVendor(t, 200)
	h := newHarness(t, testConfig(), true)

	id := h.submit(t, v.srv.URL+"/hooks/crm")
	h.waitStatus(t, id, "succeeded")

	v.mu.Lock()
	defer v.mu.Unlock()
	req := v.requests[0]
	if req.method != "POST" || req.path != "/hooks/crm" {
		t.Fatalf("method/path 不对: %s %s", req.method, req.path)
	}
	if req.body != `{"event":"paid"}` {
		t.Fatalf("body 未原样透传: %s", req.body)
	}
	if req.header.Get("X-Biz") != "crm" {
		t.Fatalf("header 未原样透传: %v", req.header)
	}
	if req.header.Get("X-Notification-Id") != id {
		t.Fatalf("缺少 X-Notification-Id 或值不对: %v", req.header.Get("X-Notification-Id"))
	}
}

// TestVendorHangConsumesBudget 回归 Grok CR Critical：供应商挂起耗尽单次超时后，
// 结果必须能写回（写库不得复用已 DeadlineExceeded 的 attemptCtx），
// attempts 正常消耗、后续重试正常进行。
func TestVendorHangConsumesBudget(t *testing.T) {
	cfg := testConfig()
	cfg.AttemptTimeout = 150 * time.Millisecond

	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			time.Sleep(500 * time.Millisecond) // 挂起超过 AttemptTimeout
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	h := newHarness(t, cfg, true)
	id := h.submit(t, srv.URL+"/hook")

	got := h.waitStatus(t, id, "succeeded")
	if got["attempts"].(float64) != 2 {
		t.Fatalf("首次挂起超时应消耗 1 次预算, 期望共 2 次尝试, 得到 %v", got["attempts"])
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	v := newVendor(t, 301)
	h := newHarness(t, testConfig(), true)

	id := h.submit(t, v.srv.URL+"/hook")
	got := h.waitStatus(t, id, "dead")
	if got["attempts"].(float64) != 1 || !strings.Contains(got["last_error"].(string), "301") {
		t.Fatalf("3xx 应一次即死信且记录状态码: attempts=%v err=%v", got["attempts"], got["last_error"])
	}
}

func postJSON(t *testing.T, url, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return resp, m
}

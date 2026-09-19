package api

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
	"testing"
	"time"

	"notifyd/internal/store"
)

func timeNow() time.Time { return time.Now().UTC() }

func newTestServer(t *testing.T) (*httptest.Server, store.Store) {
	t.Helper()
	s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	h := NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)), 256*1024)
	srv := httptest.NewServer(h.Mux())
	t.Cleanup(srv.Close)
	return srv, s
}

func post(t *testing.T, url, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	return resp, m
}

const validBody = `{"target_url":"https://vendor.example.com/hook","headers":{"X-K":"v"},"body":"{\"a\":1}","idempotency_key":"k1"}`

func TestCreate(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, m := post(t, srv.URL+"/api/notifications", validBody)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("期望 202, 得到 %d: %v", resp.StatusCode, m)
	}
	if m["status"] != "pending" || m["id"] == "" || m["method"] != "POST" {
		t.Fatalf("响应不对: %v", m)
	}

	// 幂等重放 → 200 同 id
	resp2, m2 := post(t, srv.URL+"/api/notifications", validBody)
	if resp2.StatusCode != http.StatusOK || m2["id"] != m["id"] {
		t.Fatalf("幂等重放: 期望 200 同 id, 得到 %d id=%v", resp2.StatusCode, m2["id"])
	}

	// 同 key 不同 payload → 409
	conflict := strings.Replace(validBody, `{\"a\":1}`, `{\"a\":2}`, 1)
	resp3, _ := post(t, srv.URL+"/api/notifications", conflict)
	if resp3.StatusCode != http.StatusConflict {
		t.Fatalf("幂等冲突: 期望 409, 得到 %d", resp3.StatusCode)
	}
}

func TestCreateValidation(t *testing.T) {
	srv, _ := newTestServer(t)

	cases := []struct {
		name string
		body string
	}{
		{"非法 JSON", `{`},
		{"缺 target_url", `{"body":"x"}`},
		{"非 http scheme", `{"target_url":"ftp://x.com/a"}`},
		{"非法 method", `{"target_url":"https://x.com/a","method":"DELETE"}`},
		{"body 超限", fmt.Sprintf(`{"target_url":"https://x.com/a","body":"%s"}`, strings.Repeat("A", 256*1024+1))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := post(t, srv.URL+"/api/notifications", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("期望 400, 得到 %d", resp.StatusCode)
			}
		})
	}
}

func TestGet(t *testing.T) {
	srv, _ := newTestServer(t)

	_, m := post(t, srv.URL+"/api/notifications", validBody)
	resp, err := http.Get(srv.URL + "/api/notifications/" + m["id"].(string))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET: err=%v status=%d", err, resp.StatusCode)
	}
	resp.Body.Close()

	resp, _ = http.Get(srv.URL + "/api/notifications/no-such-id")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("期望 404, 得到 %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHealthz(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望 200, 得到 %d", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Fatalf("liveness 应为空 body, 得到 %q", body)
	}
}

func TestReadyz(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer resp.Body.Close()
	var ok map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&ok); err != nil {
		t.Fatalf("解析 /readyz: %v", err)
	}
	if resp.StatusCode != http.StatusOK || ok["status"] != "ready" {
		t.Fatalf("期望 200 ready, 得到 %d %v", resp.StatusCode, ok)
	}

	failing := NewHandler(pingFailStore{err: fmt.Errorf("db unreachable")}, slog.New(slog.NewTextHandler(io.Discard, nil)), 256*1024)
	failSrv := httptest.NewServer(failing.Mux())
	t.Cleanup(failSrv.Close)

	resp2, err := http.Get(failSrv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz (fail): %v", err)
	}
	defer resp2.Body.Close()
	var notReady map[string]string
	if err := json.NewDecoder(resp2.Body).Decode(&notReady); err != nil {
		t.Fatalf("解析 /readyz fail: %v", err)
	}
	if resp2.StatusCode != http.StatusServiceUnavailable ||
		notReady["status"] != "not_ready" || notReady["error"] == "" {
		t.Fatalf("期望 503 not_ready+error, 得到 %d %v", resp2.StatusCode, notReady)
	}
}

func TestStats(t *testing.T) {
	srv, s := newTestServer(t)

	resp, err := http.Get(srv.URL + "/api/stats")
	if err != nil {
		t.Fatalf("GET /api/stats: %v", err)
	}
	got := decodeStats(t, resp)
	if resp.StatusCode != http.StatusOK || got != (store.StatusCounts{}) {
		t.Fatalf("空队列: 期望 200 全 0, 得到 %d %+v", resp.StatusCode, got)
	}

	_, m := post(t, srv.URL+"/api/notifications", validBody)
	id := m["id"].(string)
	ctx := t.Context()
	claimed, err := s.ClaimDue(ctx, timeNow(), 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("认领失败: err=%v n=%d", err, len(claimed))
	}
	if err := s.MarkDead(ctx, id, "test", timeNow()); err != nil {
		t.Fatalf("MarkDead: %v", err)
	}

	_, m2 := post(t, srv.URL+"/api/notifications", strings.Replace(validBody, `"k1"`, `"k2"`, 1))
	if m2["id"] == "" {
		t.Fatalf("第二条通知创建失败: %v", m2)
	}

	resp, err = http.Get(srv.URL + "/api/stats")
	if err != nil {
		t.Fatalf("GET /api/stats: %v", err)
	}
	got = decodeStats(t, resp)
	want := store.StatusCounts{Pending: 1, Dead: 1}
	if resp.StatusCode != http.StatusOK || got != want {
		t.Fatalf("期望 %+v, 得到 %d %+v", want, resp.StatusCode, got)
	}
}

func decodeStats(t *testing.T, resp *http.Response) store.StatusCounts {
	t.Helper()
	defer resp.Body.Close()
	var got store.StatusCounts
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("解析 /api/stats: %v", err)
	}
	return got
}

// pingFailStore 只覆盖 Ping，用于 /readyz 503 分支。
type pingFailStore struct {
	store.Store
	err error
}

func (p pingFailStore) Ping(context.Context) error { return p.err }

func TestRedeliver(t *testing.T) {
	srv, s := newTestServer(t)

	_, m := post(t, srv.URL+"/api/notifications", validBody)
	id := m["id"].(string)

	// pending 状态重放 → 409
	resp, _ := post(t, srv.URL+"/api/notifications/"+id+"/redeliver", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("期望 409, 得到 %d", resp.StatusCode)
	}

	// 置为 dead 后重放 → 200 pending
	ctx := t.Context()
	claimed, _ := s.ClaimDue(ctx, timeNow(), 1)
	if len(claimed) != 1 {
		t.Fatalf("认领失败")
	}
	s.MarkDead(ctx, id, "test", timeNow())

	resp, m2 := post(t, srv.URL+"/api/notifications/"+id+"/redeliver", "")
	if resp.StatusCode != http.StatusOK || m2["status"] != "pending" {
		t.Fatalf("重放: 期望 200 pending, 得到 %d %v", resp.StatusCode, m2["status"])
	}

	resp, _ = post(t, srv.URL+"/api/notifications/no-such-id/redeliver", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("期望 404, 得到 %d", resp.StatusCode)
	}
}

package api

import (
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

	"rc_hugobao/internal/store"
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

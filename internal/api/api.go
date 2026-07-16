// Package api 实现内部 HTTP API（spec §3）。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"rc_hugobao/internal/store"
)

var allowedMethods = map[string]bool{"POST": true, "PUT": true, "PATCH": true}

type Handler struct {
	store        store.Store
	logger       *slog.Logger
	maxBodyBytes int64
	now          func() time.Time
}

func NewHandler(s store.Store, logger *slog.Logger, maxBodyBytes int64) *Handler {
	return &Handler{store: s, logger: logger, maxBodyBytes: maxBodyBytes, now: time.Now}
}

func (h *Handler) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/notifications", h.create)
	mux.HandleFunc("GET /api/notifications/{id}", h.get)
	mux.HandleFunc("POST /api/notifications/{id}/redeliver", h.redeliver)
	return mux
}

type createRequest struct {
	TargetURL      string            `json:"target_url"`
	Method         string            `json:"method"`
	Headers        map[string]string `json:"headers"`
	Body           string            `json:"body"`
	IdempotencyKey string            `json:"idempotency_key"`
}

// notificationDTO 是对外的 JSON 形状（spec §3.2）。
type notificationDTO struct {
	ID             string            `json:"id"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	TargetURL      string            `json:"target_url"`
	Method         string            `json:"method"`
	Headers        map[string]string `json:"headers"`
	Body           string            `json:"body"`
	Status         string            `json:"status"`
	Attempts       int               `json:"attempts"`
	NextAttemptAt  *time.Time        `json:"next_attempt_at,omitempty"`
	LastError      string            `json:"last_error,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

func toDTO(n *store.Notification) notificationDTO {
	if n.Headers == nil {
		n.Headers = map[string]string{}
	}
	dto := notificationDTO{
		ID:             n.ID,
		IdempotencyKey: n.IdempotencyKey,
		TargetURL:      n.TargetURL,
		Method:         n.Method,
		Headers:        n.Headers,
		Body:           n.Body,
		Status:         n.Status,
		Attempts:       n.Attempts,
		LastError:      n.LastError,
		CreatedAt:      n.CreatedAt,
		UpdatedAt:      n.UpdatedAt,
	}
	if n.Status == store.StatusPending {
		t := n.NextAttemptAt
		dto.NextAttemptAt = &t
	}
	return dto
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes*2) // 整个 JSON 的粗上限
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求不是合法 JSON 或超限: "+err.Error())
		return
	}
	if req.Method == "" {
		req.Method = "POST"
	}
	if err := validate(req, h.maxBodyBytes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	n := &store.Notification{
		IdempotencyKey: req.IdempotencyKey,
		TargetURL:      req.TargetURL,
		Method:         req.Method,
		Headers:        req.Headers,
		Body:           req.Body,
	}
	stored, existed, err := h.store.Create(r.Context(), n, h.now().UTC())
	if errors.Is(err, store.ErrIdempotencyConflict) {
		writeError(w, http.StatusConflict, "idempotency_key 已被不同的 payload 使用")
		return
	}
	if err != nil {
		h.internalError(w, "创建通知", err)
		return
	}

	status := http.StatusAccepted // 202：已持久化，投递责任移交本服务（spec §3.1）
	if existed {
		status = http.StatusOK // 200：幂等重放
	} else {
		h.logger.Info("通知已受理", "id", stored.ID, "target", stored.TargetURL)
	}
	writeJSON(w, status, toDTO(stored))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	n, err := h.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "通知不存在")
		return
	}
	if err != nil {
		h.internalError(w, "查询通知", err)
		return
	}
	writeJSON(w, http.StatusOK, toDTO(n))
}

func (h *Handler) redeliver(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := h.store.Redeliver(r.Context(), id, h.now().UTC())
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "通知不存在")
	case errors.Is(err, store.ErrNotDead):
		writeError(w, http.StatusConflict, "只有 dead 状态的通知可以重放")
	case err != nil:
		h.internalError(w, "重放通知", err)
	default:
		h.logger.Info("死信已重放", "id", id)
		writeJSON(w, http.StatusOK, toDTO(n))
	}
}

func validate(req createRequest, maxBody int64) error {
	u, err := url.Parse(req.TargetURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("target_url 必须是合法的 http(s) 地址")
	}
	if !allowedMethods[req.Method] {
		return fmt.Errorf("method 仅支持 POST / PUT / PATCH")
	}
	if int64(len(req.Body)) > maxBody {
		return fmt.Errorf("body 超过上限 %d 字节", maxBody)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handler) internalError(w http.ResponseWriter, op string, err error) {
	h.logger.Error(op+"失败", "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

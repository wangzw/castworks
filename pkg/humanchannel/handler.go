// Package humanchannel 提供控制面 HTTP API 的处理器。
//
// 处理器把 humanchannel 的线上 JSON 类型（humanchannel/api）与
// runcontroller.RunService 对接：校验输入，把请求转交给 RunService，再把
// RunService 的哨兵错误映射成 HTTP 状态码。
package humanchannel

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strings"

	"github.com/wangzw/castworks/pkg/humanchannel/api"
	"github.com/wangzw/castworks/pkg/runcontroller"
)

const (
	// maxBodyBytes 是请求体上限，超出为 400。
	maxBodyBytes = 1 << 20
	// contentTypeJSON 是所有响应（含错误响应）的 Content-Type。
	contentTypeJSON = "application/json"
	// 400、404、409 的错误文案固定，不取哨兵错误原文（后者带 runcontroller: 前缀）。
	errTextInvalid      = "text empty or longer than 64 KiB"
	errRunNotFound      = "run not found"
	errRunEndedOrFailed = "run ended or failed"
	// 浏览器跨站请求防护的固定文案。
	errForbiddenHost        = "forbidden host"
	errUnsupportedMediaType = "unsupported media type"
)

// NewHandler 返回处理 humanchannel HTTP API 五个路径的 http.Handler。
func NewHandler(svc runcontroller.RunService) http.Handler {
	h := &handler{svc: svc}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/runs", h.create)
	mux.HandleFunc("GET /v1/runs", h.list)
	mux.HandleFunc("GET /v1/runs/{run_id}", h.get)
	mux.HandleFunc("POST /v1/runs/{run_id}/messages", h.send)
	mux.HandleFunc("POST /v1/runs/{run_id}/end", h.end)
	return guard(mux)
}

// guard 拦下浏览器可以伪造的跨站请求：Host 必须是回环地址，所有 POST 必须
// 声明 application/json。两条检查都在路由之前，因此未知路径与错误方法也要
// 先通过它们。
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			writeError(w, http.StatusForbidden, errForbiddenHost)
			return
		}
		if r.Method == http.MethodPost && !isJSONContentType(r.Header.Get("Content-Type")) {
			writeError(w, http.StatusUnsupportedMediaType, errUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackHost 报告 Host（可带端口）是否指名回环地址。接受 localhost、
// 127.0.0.1 与 [::1]，端口不限，大小写不敏感。
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	switch {
	case strings.EqualFold(host, "localhost"):
		return true
	case host == "127.0.0.1", host == "::1":
		return true
	}
	return false
}

// isJSONContentType 报告 Content-Type 的媒体类型是否为 application/json，
// 允许带 charset 等参数。
func isJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	return strings.EqualFold(mediaType, contentTypeJSON)
}

type handler struct {
	svc runcontroller.RunService
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	var req api.CreateRunRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if msg := validateText(req.Prompt); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	runID, err := h.svc.Create(r.Context(), req.Prompt)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, api.CreateRunResponse{RunID: runID})
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	summaries, err := h.svc.List(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]api.RunSummary, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, toAPISummary(s))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	detail, err := h.svc.Get(r.Context(), r.PathValue("run_id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIDetail(detail))
}

func (h *handler) send(w http.ResponseWriter, r *http.Request) {
	var req api.SendMessageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if msg := validateText(req.Text); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	inboxSeq, _, err := h.svc.AppendInbox(r.Context(), r.PathValue("run_id"), runcontroller.InboxEvent{
		Kind: runcontroller.KindHumanMessage,
		Text: req.Text,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.SendMessageResponse{InboxSeq: inboxSeq})
}

func (h *handler) end(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.End(r.Context(), r.PathValue("run_id")); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

// decodeJSON 解析请求体：上限 1 MiB、禁止未知字段、拒绝多余内容。
// 解析失败时写出 400 并返回 false。
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

// validateText 按 UTF-8 字节校验文本，空或超过 MaxTextBytes 时返回固定说明。
func validateText(s string) string {
	if len(s) == 0 || len(s) > runcontroller.MaxTextBytes {
		return errTextInvalid
	}
	return ""
}

// writeServiceError 把 RunService 的哨兵错误映射成状态码。其余错误记日志，
// 响应体不暴露内部错误文本。
func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runcontroller.ErrInvalidEvent):
		writeError(w, http.StatusBadRequest, errTextInvalid)
	case errors.Is(err, runcontroller.ErrNotFound):
		writeError(w, http.StatusNotFound, errRunNotFound)
	case errors.Is(err, runcontroller.ErrTerminated):
		writeError(w, http.StatusConflict, errRunEndedOrFailed)
	default:
		slog.Default().Error("humanchannel: run service call failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, api.ErrorResponse{Error: message})
}

// writeJSON 关闭 HTML 转义并写出单行 JSON。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func toAPISummary(s runcontroller.RunSummary) api.RunSummary {
	return api.RunSummary{
		RunID:     s.RunID,
		State:     string(s.State),
		Substate:  string(s.Substate),
		CreatedAt: api.Time{Time: s.CreatedAt},
	}
}

func toAPITurns(turns []runcontroller.Turn) []api.Turn {
	out := make([]api.Turn, 0, len(turns))
	for _, t := range turns {
		var ended *api.Time
		if t.EndedAt != nil {
			ended = &api.Time{Time: *t.EndedAt}
		}
		out = append(out, api.Turn{
			TurnID:    t.TurnID,
			StartedAt: api.Time{Time: t.StartedAt},
			EndedAt:   ended,
			Output:    t.Output,
			IsError:   t.IsError,
		})
	}
	return out
}

func toAPISuspensions(suspensions []runcontroller.Suspension) []api.Suspension {
	out := make([]api.Suspension, 0, len(suspensions))
	for _, s := range suspensions {
		out = append(out, api.Suspension{
			StartedAt:  api.Time{Time: s.StartedAt},
			SnapshotMS: s.SnapshotMS,
			RestoreMS:  s.RestoreMS,
			HelloMS:    s.HelloMS,
			Error:      s.Error,
		})
	}
	return out
}

func toAPIDetail(d runcontroller.RunDetail) api.RunDetail {
	return api.RunDetail{
		RunSummary:    toAPISummary(d.RunSummary),
		FailureReason: d.FailureReason,
		Turns:         toAPITurns(d.Turns),
		Suspensions:   toAPISuspensions(d.Suspensions),
	}
}

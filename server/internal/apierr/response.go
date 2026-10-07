package apierr

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
)

// Envelope 是统一响应结构。
type Envelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId,omitempty"`
	Data      any    `json:"data"`
}

// Details 是校验失败明细项。
type Details struct {
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// WriteJSON 写入成功响应（code=OK）。data 为 nil 时输出 null。
func WriteJSON(ctx context.Context, w http.ResponseWriter, status int, data any) {
	writeEnvelope(ctx, w, status, &Envelope{
		Code:      string(CodeOK),
		Message:   "ok",
		RequestID: RequestIDFromContext(ctx),
		Data:      data,
	})
}

// WriteError 将错误转换为统一 envelope 写入响应。
func WriteError(ctx context.Context, w http.ResponseWriter, err error) {
	ae := AsAppError(err)
	for k, v := range ae.Headers {
		w.Header().Set(k, v)
	}
	if ae.Status >= http.StatusInternalServerError {
		slog.Error("request failed",
			slog.String("requestId", RequestIDFromContext(ctx)),
			slog.String("code", string(ae.Code)),
			slog.Int("status", ae.Status),
			slog.String("err", errText(ae)),
		)
	}
	writeEnvelope(ctx, w, ae.Status, &Envelope{
		Code:      string(ae.Code),
		Message:   ae.Message,
		RequestID: RequestIDFromContext(ctx),
		Data:      ae.Data,
	})
}

func writeEnvelope(ctx context.Context, w http.ResponseWriter, status int, env *Envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(env); err != nil {
		slog.WarnContext(ctx, "write response failed", slog.String("err", err.Error()))
	}
}

func errText(ae *AppError) string {
	if ae.Err != nil {
		return ae.Err.Error()
	}
	return ae.Message
}

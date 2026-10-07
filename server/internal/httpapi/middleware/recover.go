package middleware

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/eznews/eznews/internal/apierr"
)

// Recover 捕获 handler 中的 panic，避免单个请求打挂整个进程。
//
// 对于客户端断连导致的写入错误（ECONNRESET / EPIPE）不做 500 记录，仅 debug 级别输出，
// 避免日志噪音。
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if isBrokenPipe(rec) {
				slog.DebugContext(r.Context(), "client disconnected", slog.Any("panic", rec))
				return
			}
			slog.Error("panic recovered",
				slog.String("requestId", apierr.RequestIDFromContext(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Any("panic", rec),
				slog.String("stack", string(debug.Stack())),
			)
			// panic 时响应可能已部分写入，这里只能尽力而为
			apierr.WriteError(r.Context(), w, apierr.Internal(errors.New("内部错误")))
		}()
		next.ServeHTTP(w, r)
	})
}

// isBrokenPipe 判断 panic 是否来自客户端断连。
func isBrokenPipe(rec any) bool {
	err, ok := rec.(error)
	if !ok {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	msg := err.Error()
	for _, sub := range []string{"broken pipe", "connection reset by peer", "http2: stream closed"} {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}

package middleware

import (
	"net/http"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/util"
)

// RequestIDHeader 是请求 ID 的对外头名；客户端可自带以便串联日志。
const RequestIDHeader = "X-Request-Id"

// RequestID 为每个请求分配一个 requestId：优先沿用可信的入站头，否则生成新的。
//
// requestId 会写入响应头，并注入 context，供 apierr 统一响应体与日志使用。
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" {
			id = util.RequestID()
		} else if len(id) > 64 {
			// 防止恶意超长头污染日志
			id = id[:64]
		}
		w.Header().Set(RequestIDHeader, id)
		ctx := apierr.WithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

package middleware

import (
	"net/http"

	"github.com/eznews/eznews/internal/apierr"
)

// BodyLimit 限制请求体大小，防止超大 payload 打爆内存（默认 4 MiB，见 config.IngestConf.MaxBodyBytes）。
func BodyLimit(maxBytes int64) Middleware {
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body == nil {
				next.ServeHTTP(w, r)
				return
			}
			if r.ContentLength > maxBytes {
				apierr.WriteError(r.Context(), w, apierr.PayloadTooLarge("请求体过大"))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}

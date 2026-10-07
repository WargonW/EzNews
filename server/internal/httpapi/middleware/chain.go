// Package middleware 提供 EZNews HTTP 服务端的中间件链（ARCHITECTURE.md §7.2）。
//
// 中间件顺序（由外到内）：
//
//	RequestID → Recover → BodyLimit → Logging → RateLimit → APIKey/Authn → Handler
//
// 说明：Recover 必须包在 Logging 之外，确保 panic 也能被记录为一条访问日志。
package middleware

import "net/http"

// Middleware 是标准的中间件函数类型。
type Middleware func(http.Handler) http.Handler

// Chain 将中间件按参数顺序由外到内包裹到 h 上。
//
// Chain(h, a, b, c) 的实际执行顺序为 a → b → c → h。
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		if mws[i] == nil {
			continue
		}
		h = mws[i](h)
	}
	return h
}

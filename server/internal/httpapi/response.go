package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/eznews/eznews/internal/apierr"
)

// contentTypeJSON 是本服务统一的响应 Content-Type（openapi 约定的 JSON 媒体类型）。
const contentTypeJSON = "application/json; charset=utf-8"

// writeJSON 输出统一 envelope 的成功响应（code=OK）。
//
// Content-Type 由 apierr.WriteJSON 统一设置，此处仅做语义包装，
// 保证所有 handler 的响应形态一致。
func writeJSON(ctx context.Context, w http.ResponseWriter, status int, data any) {
	apierr.WriteJSON(ctx, w, status, data)
}

// writeError 输出统一 envelope 的错误响应。
func writeError(ctx context.Context, w http.ResponseWriter, err error) {
	apierr.WriteError(ctx, w, err)
}

// writeNoContent 输出 204 No Content（无响应体）。
//
// 用于 openapi 中显式声明 204 的端点：
// POST /api/v1/auth/logout 与 DELETE /api/v1/me。
func writeNoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// writeJSONWithETag 输出带 ETag 的 JSON 响应，并在 If-None-Match 命中时短路为 304。
//
// ETag 由响应体 SHA-256 前 16 字节派生（加引号包裹，符合 RFC 9110）。
// 用于内容不可变、可被浏览器/客户端缓存的读接口（文章详情、音频元数据）。
func writeJSONWithETag(ctx context.Context, w http.ResponseWriter, r *http.Request,
	status int, data any, cacheControl string) {

	envelope := &apierr.Envelope{
		Code:      string(apierr.CodeOK),
		Message:   "ok",
		RequestID: apierr.RequestIDFromContext(ctx),
		Data:      data,
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		writeError(ctx, w, apierr.Internal(err))
		return
	}

	tag := computeETag(body)
	if cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	w.Header().Set("ETag", tag)

	// 条件请求：If-None-Match 命中（弱比较即可）则返回 304，不写响应体。
	if matchETag(r.Header.Get("If-None-Match"), tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", contentTypeJSON)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if _, werr := w.Write(body); werr != nil {
		slog.WarnContext(ctx, "write response failed", slog.String("err", werr.Error()))
	}
}

// computeETag 由响应体内容派生强 ETag（形如 "ab12..."）。
func computeETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// matchETag 判断 If-None-Match 是否命中当前 ETag（支持 * 与逗号分隔列表、弱比较）。
func matchETag(header, tag string) bool {
	header = strings.TrimSpace(header)
	if header == "" || tag == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		// 剥离 W/ 前缀做弱比较：内容未变即视为命中。
		candidate = strings.TrimPrefix(candidate, "W/")
		candidate = strings.TrimPrefix(candidate, "w/")
		if candidate == tag {
			return true
		}
	}
	return false
}

// decodeJSON 严格解析请求体到 dst。
//
// 错误映射：
//   - 空体 / 非法 JSON → 400 BAD_REQUEST
//   - 超出 BodyLimit 限制 → 413 PAYLOAD_TOO_LARGE
//   - 尾随多余内容 → 400 BAD_REQUEST
func decodeJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return apierr.BadRequest("请求体为空")
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	// 拒绝 `{}{} ` 这类尾随内容，避免静默吞掉客户端 bug。
	if dec.More() {
		return apierr.BadRequest("请求体包含多余内容")
	}
	return nil
}

// decodeError 将 json 解码错误映射为 AppError。
func decodeError(err error) error {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return apierr.PayloadTooLarge("请求体过大")
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return apierr.BadRequest("请求体不是合法 JSON")
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return apierr.Validation("请求体字段类型不符", []apierr.Details{
			{Field: typeErr.Field, Message: fmt.Sprintf("类型必须是 %s", typeErr.Type.String())},
		})
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return apierr.Internal(err)
	}
	return apierr.BadRequest("请求体不是合法 JSON")
}

// readBody 读取完整请求体（受 BodyLimit 中间件的 MaxBytesReader 约束）。
func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, apierr.BadRequest("请求体为空")
	}
	buf := make([]byte, 0, 1024)
	chunk := make([]byte, 4096)
	for {
		n, err := r.Body.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				return nil, apierr.PayloadTooLarge("请求体过大")
			}
			if errors.Is(err, io.EOF) {
				return buf, nil
			}
			return nil, decodeError(err)
		}
		if len(buf) > 64<<20 {
			// 兜底上限：BodyLimit 通常已拦截，这里防御配置误设。
			return nil, apierr.PayloadTooLarge("请求体过大")
		}
	}
}

// ---------------- 查询参数解析 ----------------

// queryString 返回去空白后的查询参数；缺省返回 fallback。
func queryString(r *http.Request, key, fallback string) string {
	v := strings.TrimSpace(r.URL.Query().Get(key))
	if v == "" {
		return fallback
	}
	return v
}

// queryBool 解析布尔查询参数；缺省返回 fallback。
func queryBool(r *http.Request, key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback, nil
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, apierr.Validation("查询参数非法", []apierr.Details{
		{Field: key, Message: "必须是布尔值（true/false）"},
	})
}

// queryInt 解析整数查询参数；缺省返回 fallback。超出 [min, max] 返回 400。
func queryInt(r *http.Request, key string, fallback, min, max int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, apierr.Validation("查询参数非法", []apierr.Details{
			{Field: key, Message: "必须是整数"},
		})
	}
	if v < min || v > max {
		return 0, apierr.Validation("查询参数非法", []apierr.Details{
			{Field: key, Message: fmt.Sprintf("取值范围必须是 %d–%d", min, max)},
		})
	}
	return v, nil
}

// queryInt64 解析 int64 查询参数；缺省返回 fallback。必须 > 0。
func queryInt64(r *http.Request, key string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, apierr.Validation("查询参数非法", []apierr.Details{
			{Field: key, Message: "必须是整数"},
		})
	}
	if v <= 0 {
		return 0, apierr.Validation("查询参数非法", []apierr.Details{
			{Field: key, Message: "必须是正整数"},
		})
	}
	return v, nil
}

// queryFloat64 解析浮点查询参数；缺省返回 fallback。必须 > 0（区间校验交给 service 归一）。
func queryFloat64(r *http.Request, key string, fallback float64) (float64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, apierr.Validation("查询参数非法", []apierr.Details{
			{Field: key, Message: "必须是数字"},
		})
	}
	return v, nil
}

// pathInt64 解析路径参数为正 int64。
func pathInt64(r *http.Request, name string) (int64, error) {
	raw := strings.TrimSpace(r.PathValue(name))
	if raw == "" {
		return 0, apierr.BadRequest("路径参数缺失：" + name)
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return 0, apierr.Validation("路径参数非法", []apierr.Details{
			{Field: name, Message: "必须是正整数"},
		})
	}
	return v, nil
}

// clientIP 提取客户端 IP：优先取反向代理写入的 X-Forwarded-For 首跳，
// 其次 X-Real-IP，最后回落到 TCP 对端地址。
//
// 仅用于进程内限流分桶，不参与任何安全判定。
func clientIP(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if first, _, found := strings.Cut(xff, ","); found {
			return strings.TrimSpace(first)
		}
		return xff
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// itoa 是 strconv.Itoa 的短别名，用于拼接校验提示文案。
func itoa(n int) string {
	return strconv.Itoa(n)
}

// Package apierr 定义统一的业务错误码、错误类型与 HTTP 响应 envelope。
//
// 统一响应结构（ARCHITECTURE.md §17.1）：
//
//	{"code":"...", "message":"...", "data":{...}, "requestId":"..."}
//
// HTTP 状态码表示传输层结果，code 表示业务结果。
package apierr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Code 是业务错误码。
type Code string

// 全部业务错误码（§3.6 及账号体系增补）。
const (
	CodeOK              Code = "OK"
	CodeBadRequest      Code = "BAD_REQUEST"
	CodeValidation      Code = "VALIDATION_FAILED"
	CodeUnauthorized    Code = "UNAUTHORIZED"
	CodeInvalidAPIKey   Code = "INVALID_API_KEY"
	CodeInvalidCreds    Code = "INVALID_CREDENTIALS"
	CodeTokenExpired    Code = "TOKEN_EXPIRED"
	CodeForbidden       Code = "FORBIDDEN"
	CodeNotFound        Code = "NOT_FOUND"
	CodeConflict        Code = "CONFLICT"
	CodePayloadTooLarge Code = "PAYLOAD_TOO_LARGE"
	CodeUnprocessable   Code = "UNPROCESSABLE"
	CodeRateLimited     Code = "RATE_LIMITED"
	CodeDBBusy          Code = "DB_BUSY"
	// CodeServiceBusy 表示服务端并发资源被占满（如 Argon2id 并发槽排队超时），
	// 属于"稍后重试可能成功"的临时态，与 500 性质不同，必须可区分。
	CodeServiceBusy Code = "SERVICE_BUSY"
	// CodeTTSUnavailable / CodeTTSQueueFull 为合成链路的两种临时态，
	// 已在 openapi.yaml 的 Envelope.code 中列出。
	CodeTTSUnavailable Code = "TTS_UNAVAILABLE"
	CodeTTSQueueFull   Code = "TTS_QUEUE_FULL"
	CodeInternal       Code = "INTERNAL_ERROR"
)

// AppError 是携带 HTTP 状态与业务码的错误。
type AppError struct {
	Status  int               // HTTP 状态码
	Code    Code              // 业务错误码
	Message string            // 面向调用方的可读信息
	Data    any               // 可选的附加数据（如 details[]）
	Headers map[string]string // 需要写入响应的头（如 Retry-After）
	Err     error             // 底层错误，仅用于日志，不外泄
}

// Error 实现 error 接口。
func (e *AppError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap 支持 errors.Is/As 链式解包。
func (e *AppError) Unwrap() error { return e.Err }

// WithData 返回带附加数据的错误副本。
func (e *AppError) WithData(data any) *AppError {
	clone := *e
	clone.Data = data
	return &clone
}

// WithHeader 返回带响应头的错误副本。
func (e *AppError) WithHeader(k, v string) *AppError {
	clone := *e
	if clone.Headers == nil {
		clone.Headers = map[string]string{}
	}
	clone.Headers[k] = v
	return &clone
}

// New 构造一个自定义错误。
func New(status int, code Code, message string) *AppError {
	return &AppError{Status: status, Code: code, Message: message}
}

// Wrap 用底层错误包装一个 AppError。
func Wrap(status int, code Code, message string, err error) *AppError {
	return &AppError{Status: status, Code: code, Message: message, Err: err}
}

// BadRequest JSON 解析失败或结构不符。
func BadRequest(message string) *AppError {
	return New(http.StatusBadRequest, CodeBadRequest, message)
}

// Validation 字段/语义校验失败，可携带 details[]。
func Validation(message string, details any) *AppError {
	e := New(http.StatusBadRequest, CodeValidation, message)
	if details != nil {
		e.Data = map[string]any{"details": details}
	}
	return e
}

// Unauthorized 未认证（签名无效 / tv 不符 / refresh 失败）。
func Unauthorized(message string) *AppError {
	return New(http.StatusUnauthorized, CodeUnauthorized, message)
}

// InvalidAPIKey API Key 无效（不区分"不存在"与"错误"）。
func InvalidAPIKey() *AppError {
	return New(http.StatusUnauthorized, CodeInvalidAPIKey, "API Key 无效")
}

// InvalidCredentials 用户名或密码错误（不区分用户是否存在）。
func InvalidCredentials() *AppError {
	return New(http.StatusUnauthorized, CodeInvalidCreds, "用户名或密码错误")
}

// TokenExpired Access Token 过期（客户端应静默 refresh 后重放）。
func TokenExpired() *AppError {
	return New(http.StatusUnauthorized, CodeTokenExpired, "登录已过期")
}

// Forbidden 已认证但被拒绝（如注册已关闭）。
func Forbidden(message string) *AppError {
	return New(http.StatusForbidden, CodeForbidden, message)
}

// NotFound 资源不存在。
func NotFound(message string) *AppError {
	return New(http.StatusNotFound, CodeNotFound, message)
}

// Conflict 冲突（删除默认源、唯一约束冲突等）。
func Conflict(message string) *AppError {
	return New(http.StatusConflict, CodeConflict, message)
}

// PayloadTooLarge 请求体过大或条目数超限。
func PayloadTooLarge(message string) *AppError {
	return New(http.StatusRequestEntityTooLarge, CodePayloadTooLarge, message).WithHeader("Retry-After", "1")
}

// Unprocessable 语义校验失败（如 publishedAt 超出允许区间）。
func Unprocessable(message string) *AppError {
	return New(http.StatusUnprocessableEntity, CodeUnprocessable, message)
}

// RateLimited 触发限流。
func RateLimited(message string) *AppError {
	return New(http.StatusTooManyRequests, CodeRateLimited, message).WithHeader("Retry-After", "1")
}

// DBBusy SQLite 写锁争用超时（可安全重试）。
func DBBusy() *AppError {
	return New(http.StatusServiceUnavailable, CodeDBBusy, "数据库繁忙，请重试").WithHeader("Retry-After", "2")
}

// ServiceBusy 并发资源占满（可安全重试）。
//
// 与 DBBusy 的区别：DBBusy 是磁盘写锁争用，ServiceBusy 是进程内并发槽（如 Argon2id 内存池）耗尽。
// 两者都返回 503 + Retry-After，但分开成不同 code，客户端才能给出"稍后重试"而非"服务异常"的提示。
func ServiceBusy(message string) *AppError {
	return New(http.StatusServiceUnavailable, CodeServiceBusy, message).WithHeader("Retry-After", "2")
}

// TTSUnavailable 未配置/不可用的 TTS provider。
func TTSUnavailable(message string) *AppError {
	return New(http.StatusServiceUnavailable, CodeTTSUnavailable, message)
}

// TTSQueueFull 合成队列已满。
func TTSQueueFull() *AppError {
	return New(http.StatusTooManyRequests, CodeTTSQueueFull, "合成队列已满，请稍后重试").WithHeader("Retry-After", "1")
}

// Internal 未预期错误；原始错误仅写日志。
func Internal(err error) *AppError {
	return Wrap(http.StatusInternalServerError, CodeInternal, "服务器内部错误", err)
}

// IsDBBusy 判断底层错误是否为 SQLite 锁争用。
func IsDBBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "database table is locked")
}

// AsAppError 将任意错误转换为 *AppError（非 AppError 视为 500）。
func AsAppError(err error) *AppError {
	var ae *AppError
	if errors.As(err, &ae) {
		return ae
	}
	if IsDBBusy(err) {
		return DBBusy()
	}
	return Internal(err)
}

type requestIDKey struct{}

// WithRequestID 将 requestId 注入 context。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFromContext 从 context 取出 requestId（缺失时返回空串）。
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}

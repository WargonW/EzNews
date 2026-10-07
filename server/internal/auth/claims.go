package auth

import (
	"context"
	"strconv"

	"github.com/golang-jwt/jwt/v5"
)

// Claims 是 Access Token 的声明集合。
type Claims struct {
	jwt.RegisteredClaims
	// TokenVersion 对应 user.token_version；+1 可使全部已签发 Access Token 失效（替代 Redis 黑名单）。
	TokenVersion int32 `json:"tv"`
	// TokenType 恒为 "access"。
	TokenType string `json:"typ"`
	// SessionID 对应 session.id（DEC-14）。
	//
	// 登出按 sid 定位会话行，而不是"依赖浏览器同一条连接"：
	// 若不带 sid，服务端无法知道要作废哪一行 refresh token，登出就形同虚设
	// （前端清了本地 token，但服务端 refresh token 仍然有效）。
	SessionID int64 `json:"sid,omitempty"`
}

// UserID 返回 sub 声明对应的用户 ID。
func (c *Claims) UserID() (int64, error) {
	return strconv.ParseInt(c.Subject, 10, 64)
}

// HasSessionID 报告 JWT 是否携带 sid（旧版本签发的 token 为 false，视为无会话）。
func (c *Claims) HasSessionID() bool { return c.SessionID > 0 }

type userIDKey struct{}
type keyIDKey struct{}
type sessionIDKey struct{}

// WithUserID 将已认证用户 ID 注入 context（仅由 RequireAuth/OptionalAuth 中间件写入）。
func WithUserID(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, userIDKey{}, userID)
}

// UserIDFromContext 取出已认证用户 ID；未认证返回 (0, false)。
func UserIDFromContext(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(userIDKey{}).(int64)
	return v, ok
}

// WithKeyID 将采集器 keyID 注入 context（仅由 APIKeyAuth 中间件写入）。
func WithKeyID(ctx context.Context, keyID string) context.Context {
	return context.WithValue(ctx, keyIDKey{}, keyID)
}

// KeyIDFromContext 取出采集器 keyID。
func KeyIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(keyIDKey{}).(string)
	return v, ok
}

// WithSessionID 将当前会话 ID 注入 context（用于 /me/sessions 标记 current）。
func WithSessionID(ctx context.Context, sessionID int64) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

// SessionIDFromContext 取出当前会话 ID。
func SessionIDFromContext(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(sessionIDKey{}).(int64)
	return v, ok
}

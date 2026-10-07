package httpapi

import (
	"context"
	"strings"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/auth"
	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/util"
)

// 本文件定义 auth / me 两组端点的请求与响应载体。
//
// ★ 重要边界：这些结构体是 **handler 层的传输契约**，不是持久化实体。
// service 层可以有自己的入参结构，但必须能用这些结构组装，
// 以保证 openapi.yaml 的字段名不被下层实现漂移。
//
// 时间字段一律 ISO 8601 UTC（util.FormatTime），内部 epoch 毫秒不外泄；
// 唯独 openapi 明确规定为整数的字段（StateItem.updatedAt、MergeItem.updatedAt、
// expiresIn、StatePage.serverTimeMs、ArticlePage.serverTimeMs）才用 int64。

// ---------------- 通用 DTO ----------------

// UserDTO 是账号对外视图（openapi UserDTO）。
type UserDTO struct {
	ID          int64   `json:"id"`
	Username    string  `json:"username"`
	Email       *string `json:"email"`
	Role        string  `json:"role"`
	CreatedAt   string  `json:"createdAt"`
	LastLoginAt *string `json:"lastLoginAt"`
}

// newUserDTO 由实体构造对外视图。
func newUserDTO(u *model.User) *UserDTO {
	if u == nil {
		return nil
	}
	dto := &UserDTO{
		ID:        u.ID,
		Username:  u.Username,
		Role:      u.Role,
		CreatedAt: util.FormatTime(u.CreatedAt),
	}
	if email := strings.TrimSpace(u.Email); email != "" {
		dto.Email = &email
	}
	if u.LastLoginAt > 0 {
		last := util.FormatTime(u.LastLoginAt)
		dto.LastLoginAt = &last
	}
	return dto
}

// TokenResponse 是登录/注册/刷新的统一响应（openapi TokenResponse）。
type TokenResponse struct {
	AccessToken string `json:"accessToken"`
	// RefreshToken 仅在 auth.refreshTokenInBody=true 时非空；
	// 同源 Cookie 部署下由服务端写入 httpOnly Cookie，此处为 null。
	RefreshToken *string `json:"refreshToken"`
	// ExpiresIn 是 accessToken 剩余秒数。
	ExpiresIn int64    `json:"expiresIn"`
	User      *UserDTO `json:"user"`
	// SessionID 是本次签发的会话行 ID（DEC-14 登出按它定位）。
	SessionID int64 `json:"sessionId"`
	// NeedsLogin=true 表示账号已创建但**未**签发会话，客户端须再调一次 /auth/login。
	//
	// 正常路径下为 false：service.Register 注册即建会话并签发令牌，
	// 客户端无需二次登录。仅当建会话失败（极少见）时才为 true，
	// 客户端据此提示用户手动登录，而不是拿着空 accessToken 去发请求。
	NeedsLogin bool `json:"needsLogin"`
}

// SessionDTO 是会话对外视图（openapi SessionDTO）。
type SessionDTO struct {
	ID         int64   `json:"id"`
	DeviceName *string `json:"deviceName"`
	ClientID   string  `json:"clientId"`
	LastSeenAt string  `json:"lastSeenAt"`
	ExpiresAt  string  `json:"expiresAt"`
	CreatedAt  string  `json:"createdAt"`
	// Current 标记是否为当前请求所使用的会话。
	Current bool `json:"current"`
}

// ---------------- Auth 请求体 ----------------

// RegisterRequest 是 POST /api/v1/auth/register 的请求体。
type RegisterRequest struct {
	Username   string  `json:"username"`
	Password   string  `json:"password"`
	Email      *string `json:"email"`
	DeviceName *string `json:"deviceName"`
	ClientID   *string `json:"clientId"`
}

// LoginRequest 是 POST /api/v1/auth/login 的请求体。
type LoginRequest struct {
	Username   string  `json:"username"`
	Password   string  `json:"password"`
	DeviceName *string `json:"deviceName"`
	ClientID   *string `json:"clientId"`
}

// RefreshRequest 是 POST /api/v1/auth/refresh 的请求体。
//
// RefreshToken 可为空：此时由 handler 从 Cookie 通道读取。
//
// ★ ClientID / DeviceName 是 openapi 未声明的**必要扩展**：
// DEC-7 的宽限重放判定依赖 client_id 一致性（异设备重放一律判为泄露），
// 若刷新时不带 client_id，弱网重试会被误判为泄露并踢掉用户全部设备。
type RefreshRequest struct {
	RefreshToken *string `json:"refreshToken"`
	ClientID     *string `json:"clientId"`
	DeviceName   *string `json:"deviceName"`
}

// ChangePasswordRequest 是 PATCH /api/v1/me/password 的请求体。
type ChangePasswordRequest struct {
	OldPassword string `json:"oldPassword"`
	NewPassword string `json:"newPassword"`
}

// DeleteAccountRequest 是 DELETE /api/v1/me 的请求体（密码二次确认，DEC-8）。
type DeleteAccountRequest struct {
	Password string `json:"password"`
}

// ---------------- 校验助手 ----------------

// validateRegister 校验注册入参（长度约束以 openapi 为准）。
//
// 只做**传输层**的长度/形态校验；用户名字符集与密码强度由
// service.AuthService.Register 内的 validateUsername/validatePassword 判定，
// 避免两处规则漂移（service 的规则更严格，且与 Argon2 参数一致）。
func (r *RegisterRequest) validate() error {
	var details []apierr.Details
	if n := len([]rune(strings.TrimSpace(r.Username))); n < 3 || n > 32 {
		details = append(details, apierr.Details{Field: "username", Message: "长度必须是 3–32"})
	}
	if n := len([]rune(r.Password)); n < 8 || n > 128 {
		details = append(details, apierr.Details{Field: "password", Message: "长度必须是 8–128"})
	}
	if r.Email != nil && looksLikeInvalidEmail(*r.Email) {
		details = append(details, apierr.Details{Field: "email", Message: "邮箱格式非法"})
	}
	details = appendOptionalText(details, "deviceName", r.DeviceName, 64)
	details = appendOptionalText(details, "clientId", r.ClientID, 64)
	if len(details) > 0 {
		return apierr.Validation("注册参数校验失败", details)
	}
	return nil
}

// validateLogin 校验登录入参。
func (r *LoginRequest) validate() error {
	if strings.TrimSpace(r.Username) == "" || r.Password == "" {
		return apierr.Validation("登录参数校验失败", []apierr.Details{
			{Field: "username", Message: "username 与 password 不能为空"},
		})
	}
	details := appendOptionalText(nil, "deviceName", r.DeviceName, 64)
	details = appendOptionalText(details, "clientId", r.ClientID, 64)
	if len(details) > 0 {
		return apierr.Validation("登录参数校验失败", details)
	}
	return nil
}

// validateChangePassword 校验改密入参。
func (r *ChangePasswordRequest) validate() error {
	if r.OldPassword == "" {
		return apierr.Validation("改密参数校验失败", []apierr.Details{
			{Field: "oldPassword", Message: "oldPassword 不能为空"},
		})
	}
	if n := len([]rune(r.NewPassword)); n < 8 || n > 128 {
		return apierr.Validation("改密参数校验失败", []apierr.Details{
			{Field: "newPassword", Message: "长度必须是 8–128"},
		})
	}
	return nil
}

// appendOptionalText 校验可选字符串长度，超限则追加一条 details。
func appendOptionalText(details []apierr.Details, field string, v *string, max int) []apierr.Details {
	if v == nil || len([]rune(*v)) <= max {
		return details
	}
	return append(details, apierr.Details{
		Field: field, Message: "长度不能超过 " + itoa(max),
	})
}

// looksLikeInvalidEmail 做最小邮箱形态校验（openapi 明确不验证送达）。
func looksLikeInvalidEmail(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return true
	}
	at := strings.IndexByte(s, '@')
	return at <= 0 || at == len(s)-1 || !strings.Contains(s[at+1:], ".")
}

// deref 返回可选字符串的值（nil → 空串）。
func deref(v *string) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(*v)
}

// userIDFromContext 取出已认证用户 ID。
//
// 仅在 RequireAuth 包裹的 handler 内调用；缺失说明路由装配有误，直接 500。
func userIDFromContext(ctx context.Context) (int64, error) {
	if id, ok := auth.UserIDFromContext(ctx); ok {
		return id, nil
	}
	return 0, apierr.Internal(apierr.New(500, apierr.CodeInternal,
		"路由装配错误：handler 缺少 RequireAuth"))
}

// optionalUserID 返回已认证用户 ID；游客态返回 (0, false)。
func optionalUserID(ctx context.Context) (int64, bool) {
	return auth.UserIDFromContext(ctx)
}

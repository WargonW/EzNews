// Package auth 提供账号体系所需的密码学原语：JWT(HS256)、Argon2id PHC、
// 不透明 Refresh Token 与并发信号量，以及与认证相关的 context 存取。
package auth

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// JWT 相关错误（由上层映射为 TOKEN_EXPIRED / UNAUTHORIZED）。
var (
	// ErrTokenExpired 表示签名有效但已过期（客户端应静默 refresh）。
	ErrTokenExpired = errors.New("access token 已过期")
	// ErrTokenInvalid 表示签名无效、类型不符或声明缺失。
	ErrTokenInvalid = errors.New("access token 无效")
	// ErrSecretMissing 表示 JWT 密钥为空。
	ErrSecretMissing = errors.New("JWT 密钥为空")
)

// Issuer 与 TokenType 常量，用于声明校验。
const (
	// Issuer 是 JWT 的 iss 声明固定值。
	Issuer = "eznews"
	// TokenTypeAccess 是访问令牌的 typ 声明值。
	TokenTypeAccess = "access"
)

// JWTManager 负责 Access Token 的签发与验签（HS256，本地验签，零 DB 查询）。
type JWTManager struct {
	secret []byte
	ttl    time.Duration
}

// NewJWTManager 创建 JWT 管理器；secret 为空返回 ErrSecretMissing。
func NewJWTManager(secret string, ttl time.Duration) (*JWTManager, error) {
	if secret == "" {
		return nil, ErrSecretMissing
	}
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	return &JWTManager{secret: []byte(secret), ttl: ttl}, nil
}

// Sign 签发一个 Access Token，返回 token 字符串与过期时间。
// payload 含 sub(user_id) / sid(session_id) / tv(token_version) / iat / exp / iss / typ。
//
// sid 是 DEC-14 的落点：登出按 sid 精确定位会话行。若省略（<=0），
// token 仍可签发，但只能用于纯读接口，登出将退化为"按 user 全量作废"。
func (m *JWTManager) Sign(userID, sessionID int64, tokenVersion int32) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(m.ttl)
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatInt(userID, 10),
			Issuer:    Issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
		TokenVersion: tokenVersion,
		TokenType:    TokenTypeAccess,
		SessionID:    sessionID,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(m.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("签发 access token 失败: %w", err)
	}
	return signed, exp, nil
}

// Parse 验签并解析 Access Token。过期返回 ErrTokenExpired，其余失败返回 ErrTokenInvalid。
func (m *JWTManager) Parse(tokenString string) (*Claims, error) {
	if tokenString == "" {
		return nil, ErrTokenInvalid
	}
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(tokenString, claims, func(*jwt.Token) (any, error) {
		return m.secret, nil
	},
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(Issuer),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, ErrTokenInvalid
	}
	if claims.Subject == "" || claims.TokenType != TokenTypeAccess {
		return nil, ErrTokenInvalid
	}
	return claims, nil
}

// TTL 返回 Access Token 有效期。
func (m *JWTManager) TTL() time.Duration { return m.ttl }

// GenerateSecret 生成 32 字节随机 JWT 密钥（base64url，用于缺省时落盘）。
func GenerateSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("生成 JWT 密钥失败: %w", err)
	}
	return encodeRawURL(buf), nil
}

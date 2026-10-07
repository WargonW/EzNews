package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// RefreshTokenBytes 是 Refresh Token 的随机字节数（32 字节，PRD §3.5）。
const RefreshTokenBytes = 32

// NewRefreshToken 生成一个 32 字节随机、base64url 编码的不透明 Refresh Token。
func NewRefreshToken() (string, error) {
	buf := make([]byte, RefreshTokenBytes)
	if err := randomFill(buf); err != nil {
		return "", err
	}
	return encodeRawURL(buf), nil
}

// HashToken 计算 Token 的 SHA-256 十六进制摘要（库内仅存哈希，绝不明文）。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// encodeRawURL 以无 padding 的 base64url 编码。
func encodeRawURL(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

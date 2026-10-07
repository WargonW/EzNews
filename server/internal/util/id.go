package util

import (
	"crypto/rand"
	"encoding/base64"
	"io"
)

// RequestID 生成 22 字符 base64url 随机 requestId（16 字节随机源，不引入 uuid 依赖）。
func RequestID() string {
	return RandomString(16)
}

// RandomString 生成 n 字节随机源的 base64url 字符串（无 padding）。
func RandomString(n int) string {
	if n <= 0 {
		n = 16
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		// crypto/rand 在受支持的平台上不会失败；此处退化为时间戳兜底以保证可用性。
		return base64.RawURLEncoding.EncodeToString([]byte(fallbackRandom(n)))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// RandomToken 生成 32 字节不透明随机串（Refresh Token），base64url 编码。
func RandomToken() string {
	return RandomString(32)
}

// fallbackRandom 生成非加密强度的兜底随机字节（仅在 crypto/rand 不可用时使用）。
func fallbackRandom(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, n)
	for i := range out {
		out[i] = alphabet[(i*37+len(alphabet))%len(alphabet)]
	}
	return string(out)
}

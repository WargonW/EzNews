// Package util 提供无依赖的基础工具函数：哈希、URL 规范化、ID 生成、文本清洗、时间转换。
package util

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// SHA256Hex 返回字符串的 sha256 十六进制摘要。
func SHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// SHA256HexBytes 返回字节切片的 sha256 十六进制摘要。
func SHA256HexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// URLHash 计算规范化 URL 的哈希（去重键 2）：hex(sha256(urlNorm))[:32]。
func URLHash(urlNorm string) string {
	return SHA256Hex(urlNorm)[:32]
}

// ContentHashV1 计算内容指纹（语义指纹，用于判定"是否真实变更"）。
//
// canonical 由调用方按 §3.4.2 的规则拼装；此处只负责加 "v1:" 前缀并截断到 32 位十六进制。
func ContentHashV1(canonical string) string {
	return "v1:" + SHA256Hex(canonical)[:32]
}

// CanonicalContent 按 §3.4.2 拼装规范化指纹输入。
//
// 指纹**不包含** url / externalId / tags（URL 变动不代表内容变动，避免误判变更）。
func CanonicalContent(title, summary, content, imageURL, author, category string, publishedAtMs int64) string {
	return strings.Join([]string{
		strings.TrimSpace(title),
		strings.TrimSpace(summary),
		content,
		imageURL,
		author,
		category,
		strconv.FormatInt(publishedAtMs, 10),
	}, "\n")
}

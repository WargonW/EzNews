package util

import (
	"strconv"
	"strings"
	"time"
)

// TimeLayout 是 API 输出的统一时间格式：ISO 8601 UTC，毫秒精度。
const TimeLayout = "2006-01-02T15:04:05.000Z"

// NowMs 返回当前 UTC epoch 毫秒。
func NowMs() int64 {
	return time.Now().UnixMilli()
}

// FormatTime 将 epoch 毫秒格式化为 ISO 8601 UTC 字符串（"...Z"，毫秒精度）。
// 0 或负数返回空串，便于指针字段输出 null。
func FormatTime(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(TimeLayout)
}

// PtrTime 返回可用于 *string 的时间字符串指针（0 值返回 nil）。
func PtrTime(ms int64) *string {
	if ms <= 0 {
		return nil
	}
	s := FormatTime(ms)
	return &s
}

// ParseTime 解析客户端时间输入，支持：
//   - epoch 毫秒/秒纯数字
//   - RFC 3339（带时区）
//   - ISO 8601 无时区（按 defLoc 解析）
//   - "2006-01-02 15:04:05" 等常见格式
//
// 返回 UTC epoch 毫秒。
func ParseTime(s string, defLoc *time.Location) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrEmptyTime
	}
	if defLoc == nil {
		defLoc = time.UTC
	}
	// 纯数字：按 epoch 毫秒（长度 > 10）或秒处理
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if len(s) > 10 {
			return n, nil
		}
		return n * 1000, nil
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999Z0700",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, defLoc); err == nil {
			return t.UnixMilli(), nil
		}
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli(), nil
		}
	}
	return 0, ErrInvalidTime
}

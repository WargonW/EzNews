package util

import "errors"

// URL 规范化相关错误。
var (
	// ErrEmptyURL 表示输入 URL 为空。
	ErrEmptyURL = errors.New("url 为空")
	// ErrInvalidURL 表示 URL 无法解析或缺少 host。
	ErrInvalidURL = errors.New("url 非法")
)

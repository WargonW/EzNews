package util

import "errors"

// 时间解析相关错误。
var (
	// ErrEmptyTime 表示时间输入为空。
	ErrEmptyTime = errors.New("时间为空")
	// ErrInvalidTime 表示时间格式无法解析。
	ErrInvalidTime = errors.New("时间格式非法")
)

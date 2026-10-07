// Package model 定义领域实体（纯 struct，不依赖任何内部包）。
package model

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// Cursor 是复合键游标 (updatedAt/id) 或 (publishedAt/id) 的解码结果。
type Cursor struct {
	TS int64 // epoch 毫秒
	ID int64 // 行 ID，用于打破同一毫秒的平局
}

// ZeroCursor 表示"无游标"（首屏）。
var ZeroCursor = Cursor{TS: 0, ID: 0}

// EncodeCursor 将游标编码为对客户端不透明的 base64url 字符串："<epochMs>:<id>"。
func EncodeCursor(ts, id int64) string {
	raw := strconv.FormatInt(ts, 10) + ":" + strconv.FormatInt(id, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor 解码游标；空串返回零值游标且不报错（视为"未提供游标"）。
func DecodeCursor(s string) (Cursor, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ZeroCursor, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		// 兼容带 padding 的标准 base64url。
		if raw2, err2 := base64.URLEncoding.DecodeString(s); err2 == nil {
			raw = raw2
		} else {
			return ZeroCursor, fmt.Errorf("游标格式非法")
		}
	}
	tsStr, idStr, found := strings.Cut(string(raw), ":")
	if !found {
		return ZeroCursor, fmt.Errorf("游标格式非法")
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return ZeroCursor, fmt.Errorf("游标时间戳非法")
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return ZeroCursor, fmt.Errorf("游标 ID 非法")
	}
	return Cursor{TS: ts, ID: id}, nil
}

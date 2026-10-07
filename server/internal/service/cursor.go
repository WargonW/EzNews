// Package service 是业务层：去重判定、游标语义、TTS 编排、账号与同步。
// service 不感知 HTTP，也不直接写 SQL（SQL 一律经 repo 层）。
package service

import (
	"github.com/eznews/eznews/internal/model"
)

// CursorOverlapMs 是增量水位的重叠窗口（2 秒）：
// 水位取 "请求处理时刻 now - 2s"，容忍写入延迟，避免漏数据（ARCHITECTURE.md §5.1）。
const CursorOverlapMs = 2000

// WaterMark 计算服务端当前水位：now - overlap（ID 固定为 0，表示从此毫秒起全部纳入）。
func WaterMark(nowMs int64) model.Cursor {
	ts := nowMs - CursorOverlapMs
	if ts < 0 {
		ts = 0
	}
	return model.Cursor{TS: ts, ID: 0}
}

// LastCursor 从结果集末行推导下一页游标；items 为空时返回传入的起始游标。
func LastCursor(items []model.ArticleListItem, delta bool, fallback model.Cursor) model.Cursor {
	if len(items) == 0 {
		return fallback
	}
	last := items[len(items)-1]
	if delta {
		return model.Cursor{TS: last.UpdatedAt, ID: last.ID}
	}
	return model.Cursor{TS: last.PublishedAt, ID: last.ID}
}

// StateLastCursor 从收藏/已读结果集末行推导下一页游标。
func StateLastCursor(items []model.Favorite, fallback model.Cursor) model.Cursor {
	if len(items) == 0 {
		return fallback
	}
	last := items[len(items)-1]
	return model.Cursor{TS: last.UpdatedAt, ID: last.ID}
}

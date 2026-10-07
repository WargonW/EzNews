package model

// SourceType 是新闻源类型。
type SourceType string

// 源类型枚举。
const (
	SourceTypeRSS    SourceType = "rss"
	SourceTypeAtom   SourceType = "atom"
	SourceTypeAPI    SourceType = "api"
	SourceTypeManual SourceType = "manual"
)

// IsValid 判断源类型是否合法。
func (t SourceType) IsValid() bool {
	switch t {
	case SourceTypeRSS, SourceTypeAtom, SourceTypeAPI, SourceTypeManual:
		return true
	}
	return false
}

// Source 是新闻源实体（对应表 source）。
type Source struct {
	ID              int64
	Key             string // 稳定业务键，采集器引用
	Name            string
	URL             string
	Type            SourceType
	Category        string
	IsDefault       bool // 系统默认源，不可删除
	Enabled         bool // 停用后其文章不进入默认列表
	SuggestInterval int  // 建议采集间隔（秒），供采集器参考
	IconURL         string
	Language        string
	Remark          string
	OwnerUserID     int64 // v1.1 预留：NULL（0）= 全局共享
	CreatedAt       int64 // epoch ms
	UpdatedAt       int64
	ArticleCount    int64 // 非表字段：列表接口可选回填
}

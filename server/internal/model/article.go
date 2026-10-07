package model

import (
	"encoding/json"
	"strings"
)

// Categories 是全部合法分类（顺序即展示顺序）。
var Categories = []string{
	"tech", "finance", "sports", "world", "china",
	"ent", "life", "auto", "military", "science", "health", "other",
}

// CategoryLabels 是分类的中文展示名。
var CategoryLabels = map[string]string{
	"tech":     "科技",
	"finance":  "财经",
	"sports":   "体育",
	"world":    "国际",
	"china":    "国内",
	"ent":      "娱乐",
	"life":     "生活",
	"auto":     "汽车",
	"military": "军事",
	"science":  "科学",
	"health":   "健康",
	"other":    "其他",
}

// NormalizeCategory 将未知分类回落为 "other"。
func NormalizeCategory(c string) string {
	c = strings.TrimSpace(strings.ToLower(c))
	if c == "" {
		return "other"
	}
	for _, v := range Categories {
		if v == c {
			return c
		}
	}
	return "other"
}

// Article 是文章实体（对应表 article）。所有时间戳为 epoch 毫秒（UTC）。
type Article struct {
	ID          int64
	SourceID    int64
	ExternalID  string // 去重键 1（可空）
	URL         string // 原始 URL（展示/跳转用）
	URLNorm     string // 规范化 URL
	URLHash     string // hex(sha256(urlNorm))[:32]，去重键 2
	Title       string
	Summary     string
	Content     string // 默认不落库；空串表示无正文
	ContentHash string // "v1:"+hex(sha256(canonical))[:32]
	Category    string
	Author      string
	ImageURL    string
	Language    string
	Tags        []string
	PublishedAt int64
	CreatedAt   int64
	UpdatedAt   int64 // ★ 增量游标依据：仅内容真实变更时刷新
	LastSeenAt  int64 // 每次被提交时刷新，不参与游标
}

// TagsJSON 将标签序列化为入库的 JSON 数组字符串。
func (a *Article) TagsJSON() string {
	if len(a.Tags) == 0 {
		return "[]"
	}
	b, err := json.Marshal(a.Tags)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// ParseTags 解析入库的 JSON 数组字符串为标签切片。
func ParseTags(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

// ArticleListItem 是列表查询的返回行：文章 + 源信息 + 可选音频状态。
type ArticleListItem struct {
	Article
	SourceKey  string
	SourceName string
}

// ArticleBrief 是收藏/已读增量流附带的文章摘要（省去前端逐条 GET /articles/{id}）。
//
// 刻意只取 5 列：标题、摘要、来源名、发布时间。够渲染一行列表卡片，
// 又不把 content / tags 拖进这条路径（增量页最多 1000 条，正文会放大到 MB 级）。
type ArticleBrief struct {
	Title       string
	Summary     string
	SourceName  string
	PublishedAt int64 // epoch 毫秒，由调用方格式化为 ISO 8601
}

// AudioBrief 是列表接口附带的音频合成状态（withAudio=true 时回填）。
type AudioBrief struct {
	Status  string   `json:"status"` // none|pending|processing|ready|failed
	AudioID *int64   `json:"audioId"`
	Voice   *string  `json:"voice"`
	Speed   *float64 `json:"speed"`
}

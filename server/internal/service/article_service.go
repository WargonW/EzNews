package service

import (
	"context"
	"strings"
	"time"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/repo"
	"github.com/eznews/eznews/internal/store"
	"github.com/eznews/eznews/internal/util"
)

// 分页默认值与上限（§8.2）：首屏/翻页默认 20（最大 100）；增量模式默认 100（最大 200）。
const (
	defaultPageLimit  = 20
	maxPageLimit      = 100
	defaultDeltaLimit = 100
	maxDeltaLimit     = 200
)

// ArticleService 提供文章列表/增量/详情/分类统计。
type ArticleService struct {
	db       *store.DB
	articles *repo.ArticleRepo
	sources  *repo.SourceRepo
	audio    *repo.AudioRepo
	fts      bool
	// DefaultVoice / DefaultSpeed 用于 withAudio 的合成状态回填。
	DefaultVoice string
	DefaultSpeed float64
}

// NewArticleService 创建 ArticleService。
func NewArticleService(db *store.DB, articles *repo.ArticleRepo, sources *repo.SourceRepo,
	audio *repo.AudioRepo, fts bool, defaultVoice string, defaultSpeed float64) *ArticleService {
	return &ArticleService{
		db: db, articles: articles, sources: sources, audio: audio, fts: fts,
		DefaultVoice: defaultVoice, DefaultSpeed: defaultSpeed,
	}
}

// ArticleListParams 是列表接口的查询参数（HTTP 层已做字符串解析前的原始值）。
type ArticleListParams struct {
	Cursor          string // 增量水位（有值 = 增量模式）
	PageCursor      string // 分页游标
	Limit           int
	SourceID        int64
	SourceKey       string
	Category        string
	From            string
	To              string
	Keyword         string
	IncludeDisabled bool
	WithAudio       bool
}

// ArticlePage 是列表响应体（游标语义见 §5）。
type ArticlePage struct {
	Items          []model.ArticleListItem
	Audio          map[int64]model.AudioBrief
	NextCursor     *string // 增量模式
	NextPageCursor *string // 翻页模式
	SyncCursor     string  // 服务端水位（now - 2s）
	HasMore        bool
	ServerTimeMs   int64
}

// CategoryCount 是分类枚举与文章计数。
type CategoryCount struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Count int64  `json:"count"`
}

// List 执行首屏/翻页或增量查询。
func (s *ArticleService) List(ctx context.Context, p ArticleListParams) (*ArticlePage, error) {
	now := util.NowMs()
	delta := strings.TrimSpace(p.Cursor) != ""

	// 解析游标
	var cursor, pageCursor model.Cursor
	var err error
	if delta {
		if cursor, err = model.DecodeCursor(p.Cursor); err != nil {
			return nil, apierr.Validation("游标非法", []apierr.Details{{Field: "cursor", Message: "cursor 格式非法"}})
		}
	} else if strings.TrimSpace(p.PageCursor) != "" {
		if pageCursor, err = model.DecodeCursor(p.PageCursor); err != nil {
			return nil, apierr.Validation("游标非法", []apierr.Details{{Field: "pageCursor", Message: "pageCursor 格式非法"}})
		}
	}

	// limit 归一
	limit := p.Limit
	if delta {
		if limit <= 0 {
			limit = defaultDeltaLimit
		}
		if limit > maxDeltaLimit {
			limit = maxDeltaLimit
		}
	} else {
		if limit <= 0 {
			limit = defaultPageLimit
		}
		if limit > maxPageLimit {
			limit = maxPageLimit
		}
	}

	// 源筛选：sourceKey → sourceID
	sourceID := p.SourceID
	if sourceID <= 0 && strings.TrimSpace(p.SourceKey) != "" {
		src, err := s.sources.GetByKey(ctx, strings.TrimSpace(p.SourceKey))
		if err != nil {
			// 未知源 → 空结果，不报错（避免源被删除导致列表 500）
			return &ArticlePage{
				Items:        []model.ArticleListItem{},
				SyncCursor:   model.EncodeCursor(WaterMark(now).TS, 0),
				HasMore:      false,
				ServerTimeMs: now,
			}, nil
		}
		sourceID = src.ID
	}

	// 时间区间
	var from, to int64
	if strings.TrimSpace(p.From) != "" {
		if v, err := util.ParseTime(p.From, time.UTC); err == nil {
			from = v
		} else {
			return nil, apierr.Validation("时间参数非法", []apierr.Details{{Field: "from", Message: "from 格式非法"}})
		}
	}
	if strings.TrimSpace(p.To) != "" {
		if v, err := util.ParseTime(p.To, time.UTC); err == nil {
			to = v
		} else {
			return nil, apierr.Validation("时间参数非法", []apierr.Details{{Field: "to", Message: "to 格式非法"}})
		}
	}
	category := ""
	if strings.TrimSpace(p.Category) != "" {
		category = model.NormalizeCategory(p.Category)
	}

	// 多取 1 条用于判断 hasMore
	q := repo.ArticleQuery{
		Cursor:          cursor,
		PageCursor:      pageCursor,
		Delta:           delta,
		Limit:           limit + 1,
		SourceID:        sourceID,
		Category:        category,
		From:            from,
		To:              to,
		Keyword:         strings.TrimSpace(p.Keyword),
		IncludeDisabled: p.IncludeDisabled,
		FTS:             s.fts,
	}
	rows, err := s.articles.List(ctx, q)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}

	page := &ArticlePage{
		Items:        rows,
		HasMore:      hasMore,
		SyncCursor:   model.EncodeCursor(WaterMark(now).TS, 0),
		ServerTimeMs: now,
	}
	if hasMore {
		next := LastCursor(rows, delta, cursor)
		encoded := model.EncodeCursor(next.TS, next.ID)
		if delta {
			page.NextCursor = &encoded
		} else {
			page.NextPageCursor = &encoded
		}
	}

	// withAudio：一次 IN 查询回填默认音色下的合成状态，避免 N+1
	if p.WithAudio && s.audio != nil && len(rows) > 0 {
		ids := make([]int64, 0, len(rows))
		for _, it := range rows {
			ids = append(ids, it.ID)
		}
		brief, err := s.audio.BatchTaskStatus(ctx, ids, s.DefaultVoice, s.DefaultSpeed)
		if err == nil {
			page.Audio = brief
		}
	}
	return page, nil
}

// Get 查询文章详情。
func (s *ArticleService) Get(ctx context.Context, id int64) (*model.Article, error) {
	a, err := s.articles.GetByID(ctx, id)
	if err != nil {
		return nil, mapRepoError(err, "文章不存在")
	}
	return a, nil
}

// FindBriefs 批量取回文章摘要（收藏/已读增量流附加标题用）。
//
// 一次 IN 查询覆盖整页，返回 map 供调用方按 articleId 取用；
// article 行已不存在（归档物理删除）时该 key 缺失，调用方按缺字段处理。
func (s *ArticleService) FindBriefs(ctx context.Context, ids []int64) (map[int64]model.ArticleBrief, error) {
	briefs, err := s.articles.FindBriefsByIDs(ctx, ids)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	return briefs, nil
}

// Categories 返回分类枚举与启用源下的文章计数。
func (s *ArticleService) Categories(ctx context.Context) ([]CategoryCount, error) {
	counts, err := s.articles.CountByCategory(ctx)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	out := make([]CategoryCount, 0, len(model.Categories))
	for _, key := range model.Categories {
		out = append(out, CategoryCount{Key: key, Label: model.CategoryLabels[key], Count: counts[key]})
	}
	return out, nil
}

// mapRepoError 将 repository 层的 ErrNotFound 映射为 404。
func mapRepoError(err error, message string) error {
	if err == repo.ErrNotFound {
		return apierr.NotFound(message)
	}
	return apierr.Internal(err)
}

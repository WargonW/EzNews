package httpapi

import (
	"net/http"
	"strings"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/service"
	"github.com/eznews/eznews/internal/util"
)

// listArticles 查询参数上限（openapi §Limit）。
const (
	maxKeywordLen = 64
	// articleDetailCacheControl 让浏览器/客户端缓存文章详情（内容变更不频繁）。
	articleDetailCacheControl = "public, max-age=60"
)

// ArticleDTO 是文章对外视图（openapi ArticleDTO）。
//
// ★ 注意：列表接口**不返回 content**（ARCHITECTURE.md §8.3 明确要求），
// 只有详情接口且该文章确实存了正文时才带出。
type ArticleDTO struct {
	ID          int64    `json:"id"`
	SourceID    int64    `json:"sourceId"`
	SourceKey   string   `json:"sourceKey"`
	SourceName  string   `json:"sourceName"`
	Category    string   `json:"category"`
	Title       string   `json:"title"`
	Summary     string   `json:"summary"`
	ImageURL    *string  `json:"imageUrl"`
	Author      *string  `json:"author"`
	URL         string   `json:"url"`
	PublishedAt string   `json:"publishedAt"`
	UpdatedAt   string   `json:"updatedAt"`
	Tags        []string `json:"tags"`

	// Content 仅详情接口且实际存了正文时出现。
	Content *string `json:"content,omitempty"`

	// Audio 仅 withAudio=true 时出现（省去 N 次请求）。
	Audio *AudioBriefDTO `json:"audio,omitempty"`

	// IsFavorited / IsRead 仅在 **带合法 JWT** 时出现。
	// 游客态（含 token 非法/过期的降级情形）一律不输出这两个字段，
	// 客户端据此判断"无用户态"而非"未收藏"。
	IsFavorited *bool `json:"isFavorited,omitempty"`
	IsRead      *bool `json:"isRead,omitempty"`
}

// AudioBriefDTO 是合成状态摘要（openapi AudioBrief）。
type AudioBriefDTO struct {
	Status  string   `json:"status"` // none|pending|processing|ready|failed
	AudioID *int64   `json:"audioId"`
	Voice   *string  `json:"voice"`
	Speed   *float64 `json:"speed"`
}

// ArticlePageDTO 是文章列表响应（openapi ArticlePage）。
type ArticlePageDTO struct {
	Items []ArticleDTO `json:"items"`
	// NextCursor 是增量模式下一页水位；hasMore=false 时为 null。
	NextCursor *string `json:"nextCursor"`
	// NextPageCursor 是翻页模式下一页游标；无更多时为 null。
	NextPageCursor *string `json:"nextPageCursor"`
	// SyncCursor 是服务端水位（now-2s 重叠窗口），客户端保存用于下次增量。
	SyncCursor string `json:"syncCursor"`
	HasMore    bool   `json:"hasMore"`
	// ServerTime 为 ISO 8601，ServerTimeMs 为 epoch 毫秒（openapi 同时声明两者）。
	ServerTime   string `json:"serverTime"`
	ServerTimeMs int64  `json:"serverTimeMs"`
}

// CategoriesDTO 是 GET /api/v1/categories 的响应。
type CategoriesDTO struct {
	Items []service.CategoryCount `json:"items"`
}

// handleListArticles 处理 GET /api/v1/articles。
//
// 两种模式互斥（openapi 描述）：
//   - 不传 cursor → 首屏/翻页模式：published_at DESC, id DESC，返回 syncCursor。
//   - 传 cursor   → 增量同步模式：updated_at ASC, id ASC，返回 nextCursor。
func (s *Server) handleListArticles(w http.ResponseWriter, r *http.Request) {
	if s.deps.Articles == nil {
		writeError(r.Context(), w, errArticlesUnavailable)
		return
	}
	params, err := parseArticleListParams(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	page, err := s.deps.Articles.List(r.Context(), params)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if page == nil {
		writeError(r.Context(), w, apierr.Internal(errNilArticlePage))
		return
	}

	dto := ArticlePageDTO{
		Items:          make([]ArticleDTO, 0, len(page.Items)),
		NextCursor:     page.NextCursor,
		NextPageCursor: page.NextPageCursor,
		SyncCursor:     page.SyncCursor,
		HasMore:        page.HasMore,
		ServerTimeMs:   page.ServerTimeMs,
		ServerTime:     util.FormatTime(page.ServerTimeMs),
	}

	// 用户态附加：仅在带合法 JWT 时查询并附加 isFavorited / isRead。
	states := UserStates{}
	if userID, ok := optionalUserID(r.Context()); ok {
		ids := make([]int64, 0, len(page.Items))
		for _, it := range page.Items {
			ids = append(ids, it.ID)
		}
		states = s.loadUserStates(r, userID, ids)
	}

	for _, item := range page.Items {
		article := newArticleDTO(item, params.WithAudio, page.Audio)
		// 仅在登录态才输出这两个字段：游客态下省略而非输出 false，
		// 这样客户端能区分"未收藏"与"未登录不知道"。
		if states.Attached {
			fav := states.Favorited[item.ID]
			read := states.Read[item.ID]
			article.IsFavorited = &fav
			article.IsRead = &read
		}
		dto.Items = append(dto.Items, article)
	}
	writeJSON(r.Context(), w, http.StatusOK, dto)
}

// handleGetArticle 处理 GET /api/v1/articles/{id}：文章详情。
func (s *Server) handleGetArticle(w http.ResponseWriter, r *http.Request) {
	if s.deps.Articles == nil {
		writeError(r.Context(), w, errArticlesUnavailable)
		return
	}
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	article, err := s.deps.Articles.Get(r.Context(), id)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}

	item := model.ArticleListItem{Article: *article}
	dto := newArticleDTO(item, false, nil)
	// 详情接口：仅当该文章确实存了正文时才带 content（默认服务端不存正文）。
	if article.Content != "" {
		content := article.Content
		dto.Content = &content
	}
	// 用户态附加（同列表接口）。
	if userID, ok := optionalUserID(r.Context()); ok {
		if states := s.loadUserStates(r, userID, []int64{id}); states.Attached {
			fav := states.Favorited[id]
			read := states.Read[id]
			dto.IsFavorited = &fav
			dto.IsRead = &read
		}
	}
	writeJSONWithETag(r.Context(), w, r, http.StatusOK, dto, articleDetailCacheControl)
}

// handleCategories 处理 GET /api/v1/categories：分类枚举及文章计数。
func (s *Server) handleCategories(w http.ResponseWriter, r *http.Request) {
	if s.deps.Articles == nil {
		writeError(r.Context(), w, errArticlesUnavailable)
		return
	}
	items, err := s.deps.Articles.Categories(r.Context())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if items == nil {
		items = []service.CategoryCount{}
	}
	// 分类枚举变化极少，缓存 5 分钟。
	writeJSONWithETag(r.Context(), w, r, http.StatusOK, CategoriesDTO{Items: items},
		"public, max-age=300")
}

// handleGetArticleAudio 处理 GET /api/v1/articles/{id}/audio：缓存直查。
//
// **不触发合成**：未合成返回 404，客户端需先 POST /api/v1/audio/tasks。
func (s *Server) handleGetArticleAudio(w http.ResponseWriter, r *http.Request) {
	if s.deps.Audio == nil {
		writeError(r.Context(), w, errAudioUnavailable)
		return
	}
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	// speed 缺省传 0，由 service 的 NormalizeSpeed 回落到服务端默认语速。
	speed, err := queryFloat64(r, "speed", 0)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	audio, err := s.deps.Audio.ArticleAudio(r.Context(), id,
		queryString(r, "voice", ""), speed)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSONWithETag(r.Context(), w, r, http.StatusOK, newAudioDTO(audio),
		"public, max-age=300")
}

// ---------------- 内部助手 ----------------

// parseArticleListParams 解析 GET /api/v1/articles 的查询参数。
func parseArticleListParams(r *http.Request) (service.ArticleListParams, error) {
	var p service.ArticleListParams

	p.Cursor = queryString(r, "cursor", "")
	p.PageCursor = queryString(r, "pageCursor", "")

	// limit 上限放宽到 200：增量模式 service 允许到200，翻页模式归一为 100。
	limit, err := queryInt(r, "limit", 0, 1, 200)
	if err != nil {
		return p, err
	}
	p.Limit = limit

	sourceID, err := queryInt64(r, "sourceId", 0)
	if err != nil {
		return p, err
	}
	p.SourceID = sourceID
	p.SourceKey = queryString(r, "sourceKey", "")
	p.Category = queryString(r, "category", "")
	p.From = queryString(r, "from", "")
	p.To = queryString(r, "to", "")

	keyword := queryString(r, "q", "")
	if n := len([]rune(keyword)); n > maxKeywordLen {
		return p, apierr.Validation("查询参数非法", []apierr.Details{
			{Field: "q", Message: "关键词长度不能超过 " + itoa(maxKeywordLen)},
		})
	}
	p.Keyword = keyword

	includeDisabled, err := queryBool(r, "includeDisabled", false)
	if err != nil {
		return p, err
	}
	p.IncludeDisabled = includeDisabled

	withAudio, err := queryBool(r, "withAudio", false)
	if err != nil {
		return p, err
	}
	p.WithAudio = withAudio

	return p, nil
}

// newArticleDTO 由实体构造对外视图。
//
// withAudio=false 时不输出 audio 字段（openapi：仅 withAudio=true 时出现）。
func newArticleDTO(item model.ArticleListItem, withAudio bool,
	audioMap map[int64]model.AudioBrief) ArticleDTO {

	dto := ArticleDTO{
		ID:          item.ID,
		SourceID:    item.SourceID,
		SourceKey:   item.SourceKey,
		SourceName:  item.SourceName,
		Category:    item.Category,
		Title:       item.Title,
		Summary:     item.Summary,
		URL:         item.URL,
		PublishedAt: util.FormatTime(item.PublishedAt),
		UpdatedAt:   util.FormatTime(item.UpdatedAt),
		Tags:        item.Tags,
	}
	if dto.Tags == nil {
		dto.Tags = []string{}
	}
	if image := strings.TrimSpace(item.ImageURL); image != "" {
		dto.ImageURL = &image
	}
	if author := strings.TrimSpace(item.Author); author != "" {
		dto.Author = &author
	}
	// ★ withAudio=true 时**每篇都要有 audio 字段**，没合成过的也要显式给
	// status="none"，而不是整个字段缺席。
	//
	// 原因：audioMap 来自 `audio_task` 表，只收录「已提交过合成任务」的文章，
	// 从未合成的文章根本不在 map 里。若此时省略字段，客户端拿到的是
	// 「audio 不存在」这个二义状态 —— 无法区分「没合成过」与「服务端没查」。
	// openapi 的 AudioBrief.status 枚举里本就定义了 none，前端 types.ts 的
	// AudioBriefStatus 也照抄了这个 none，两侧都等着服务端真的产出它。
	//
	// 曾经靠前端 `article.audio?.status !== 'ready'` 的可选链侥幸不出错，
	// 但那是把契约缺口转嫁给调用方：任何人写 `audio.status` 就会 NPE。
	if withAudio && audioMap != nil {
		brief, ok := audioMap[item.ID]
		if !ok {
			brief = model.AudioBrief{Status: "none"}
		}
		dto.Audio = &AudioBriefDTO{
			Status:  brief.Status,
			AudioID: brief.AudioID,
			Voice:   brief.Voice,
			Speed:   brief.Speed,
		}
	}
	return dto
}

// UserStates 是一次文章列表/详情请求的用户态附加结果。
type UserStates struct {
	// Favorited 是 articleId → 是否已收藏（墓碑算未收藏）。
	Favorited map[int64]bool
	// Read 是 articleId → 是否已读（墓碑算未读）。
	Read map[int64]bool
	// Attached 标记本次是否真的查到了用户态。
	// false 时 handler 必须**省略** isFavorited / isRead 字段，
	// 以区分「游客未登录」与「已收藏=false」。
	Attached bool
}

// loadUserStates 批量查询用户态；失败时返回 Attached=false 的空值。
//
// 用户态是**附加信息**，查询失败绝不能影响文章列表本身的可用性——
// 因此这里只记 warn 日志，不向上返回 error。
func (s *Server) loadUserStates(r *http.Request, userID int64, articleIDs []int64) UserStates {
	empty := UserStates{Favorited: map[int64]bool{}, Read: map[int64]bool{}}
	if s.deps.State == nil || userID <= 0 || len(articleIDs) == 0 {
		return empty
	}
	// LoadFlags 内部已对单类查询失败做静默降级（游客态直接返回全false 空标记）。
	flags := s.deps.State.LoadFlags(r.Context(), userID, articleIDs)
	if flags.Favorites == nil && flags.Reads == nil {
		return empty
	}
	if flags.Favorites == nil {
		flags.Favorites = map[int64]bool{}
	}
	if flags.Reads == nil {
		flags.Reads = map[int64]bool{}
	}
	return UserStates{Favorited: flags.Favorites, Read: flags.Reads, Attached: true}
}

// errArticlesUnavailable 表示文章服务未注入。
var errArticlesUnavailable = apierr.New(http.StatusServiceUnavailable, apierr.CodeInternal,
	"文章服务未就绪")

// errAudioUnavailable 表示音频服务未注入。
var errAudioUnavailable = apierr.New(http.StatusServiceUnavailable, apierr.CodeInternal,
	"音频服务未就绪")

// errNilArticlePage 是 service 返回空页的编程错误。
var errNilArticlePage = apierr.New(http.StatusInternalServerError, apierr.CodeInternal,
	"文章列表结果为空")

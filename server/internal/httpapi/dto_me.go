package httpapi

import (
	"strings"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/service"
	"github.com/eznews/eznews/internal/util"
)

// 本文件定义 /api/v1/me/** 的传输载体，并把 service 层的 View 结构
// 映射为 openapi.yaml 声明的响应形状。
//
// ★ 映射层的存在理由：service 层返回的 FavoriteView/ReadView/StatePage
// 用 ISO 8601 字符串表达时间、且 StatePage.NextCursor 是 string 而非 *string，
// 与 openapi（updatedAt: integer / nextCursor: [string,null]）不一致。
// 统一在本文件做形状转换，使**对外契约**稳定不被下层实现漂移。

// 增量页默认值与上限（openapi StateLimit：默认 200，最大 1000）。
//
// ★ 注意 service.UserStateService.clampLimit 内部硬上限为 200，
// 因此 200–1000 的请求会被静默收敛到 200（已记入交付报告的契约漂移）。
const (
	defaultStateLimit = 200
	maxStateLimit     = 1000
	// mergeMaxItems 是 openapi MergeRequest 中 favorites/reads 的条目上限。
	mergeMaxItems = 5000
)

// MergeRequest 是 POST /api/v1/me/merge 的请求体（openapi MergeRequest）。
//
// 直接复用 service.MergeRequest —— 它的 JSON tag 与 openapi 完全一致
// （clientId / nonce / favorites / reads / preferences），
// 无需在handler 层重复定义一份再逐字段搬运。
type MergeRequest = service.MergeRequest

// MergeItem 是合并请求中的单条状态（openapi MergeItem）。
type MergeItem = service.StateItem

// MergeCounts 是单类状态的合并计数（openapi MergeCounts）。
type MergeCounts struct {
	Received int `json:"received"`
	Added    int `json:"added"`
	Merged   int `json:"merged"`
	Skipped  int `json:"skipped"`
}

// MergeResult 是 POST /api/v1/me/merge 的响应体（openapi MergeResult）。
//
// service.MergeResult 的字段名与 openapi 差异较大（favoritesAdded 而非
// favorites.added、nextCursor 而非 snapshotCursor），故在此做显式映射。
type MergeResult struct {
	Applied  bool `json:"applied"`
	Replayed bool `json:"replayed"`
	// FavoriteCounts / ReadCounts 沿用 openapi 的 favorites / reads 字段名。
	FavoriteCounts    MergeCounts `json:"favorites"`
	ReadCounts        MergeCounts `json:"reads"`
	PreferenceApplied bool        `json:"preferenceApplied"`
	SnapshotCursor    string      `json:"snapshotCursor"`
}

// newMergeResult 把 service 的合并结果映射为 openapi 形状。
//
// received 无对应字段，按 added+merged+skipped 求和还原（service 已覆盖全部分类）。
func newMergeResult(res *service.MergeResult) *MergeResult {
	if res == nil {
		return nil
	}
	fav := MergeCounts{
		Added:   res.FavoritesAdded,
		Merged:  res.FavoritesMerged,
		Skipped: res.FavoritesSkipped,
	}
	fav.Received = fav.Added + fav.Merged + fav.Skipped
	read := MergeCounts{
		Added:   res.ReadsAdded,
		Merged:  res.ReadsMerged,
		Skipped: res.ReadsSkipped,
	}
	read.Received = read.Added + read.Merged + read.Skipped

	return &MergeResult{
		// replayed=true 表示命中幂等日志、本次零写入，故 applied 取反。
		Applied:           !res.Replayed,
		Replayed:          res.Replayed,
		FavoriteCounts:    fav,
		ReadCounts:        read,
		PreferenceApplied: res.PreferencesWritten > 0,
		SnapshotCursor:    res.NextCursor,
	}
}

// validateMergeRequest 校验合并请求（openapi MergeRequest 的长度/条目约束）。
//
// service.MergeService.Merge 自带 clientId/nonce 非空与条目上限校验，
// 这里补齐 openapi 明确声明、service 未覆盖的 maxLength 与 preferences 形态约束。
func validateMergeRequest(req *MergeRequest) error {
	var details []apierr.Details
	if clientID := strings.TrimSpace(req.ClientID); clientID == "" || len(clientID) > 64 {
		details = append(details, apierr.Details{Field: "clientId", Message: "不能为空且长度不能超过 64"})
	}
	if nonce := strings.TrimSpace(req.Nonce); nonce == "" || len(nonce) > 64 {
		details = append(details, apierr.Details{Field: "nonce", Message: "不能为空且长度不能超过 64"})
	}
	details = appendMergeItems(details, "favorites", req.Favorites)
	details = appendMergeItems(details, "reads", req.Reads)
	for key, value := range req.Preferences {
		if key == "" || len(key) > 128 {
			details = append(details, apierr.Details{Field: "preferences", Message: "键不能为空且长度不能超过 128"})
			break
		}
		if len(value) > 4096 {
			details = append(details, apierr.Details{Field: "preferences", Message: "值长度不能超过 4096"})
			break
		}
	}
	if len(details) > 0 {
		return apierr.Validation("合并参数校验失败", details)
	}
	return nil
}

// appendMergeItems 校验一组 MergeItem（条目上限 + 必填字段）。
func appendMergeItems(details []apierr.Details, field string, items []MergeItem) []apierr.Details {
	if len(items) > mergeMaxItems {
		return append(details, apierr.Details{
			Field: field, Message: "条目数不能超过 " + itoa(mergeMaxItems),
		})
	}
	for _, it := range items {
		if it.ArticleID <= 0 {
			return append(details, apierr.Details{
				Field: field, Message: "articleId 必须是正整数",
			})
		}
		if it.UpdatedAt <= 0 {
			return append(details, apierr.Details{
				Field: field, Message: "updatedAt 必须大于 0",
			})
		}
	}
	return details
}

// SetStateRequest 是 POST /api/v1/me/favorites 与 /me/reads 的请求体。
type SetStateRequest struct {
	ArticleID int64 `json:"articleId"`
	// Deleted 为 true 表示取消（写墓碑，非物理删除）。
	Deleted bool `json:"deleted"`
}

// BatchStateRequest 是 POST /api/v1/me/state/batch 的请求体（openapi BatchStateRequest）。
//
// ★ 不直接复用 service.BatchSetStateRequest，而是把 Read 从 bool 改成 *bool：
// service 侧的 `read` 是**必填语义**（无 bool 可表达"没传"，缺省即 false），
// 而 false 意味着"把这批全部标记为未读"（写墓碑）。若传输层也用裸 bool，
// 客户端漏传 read 就会静默把整批已读记录抹掉 —— 一个字段拼错就造成数据丢失。
// 改成指针后，缺省与显式 false 可区分，validate() 拒绝缺省（400），
// 漏传从此是一个响亮的错误而不是静默的破坏。
//
// Favorite 保持 *bool：nil = 本批次不改动收藏（openapi 显式声明 nullable）。
type BatchStateRequest struct {
	ArticleIDs []int64 `json:"articleIds"`
	Read       *bool   `json:"read"`
	Favorite   *bool   `json:"favorite"`
}

// BatchStateResultDTO 是 POST /api/v1/me/state/batch 的响应体（openapi BatchStateResult）。
//
// 只有两个计数：批次是原子的，要么整批生效要么整批未生效，
// 所以"成功"意味着 readsChanged 条已读与（若指定了 favorite）favoritesChanged 条收藏
// 已全部落库 —— 不存在需要客户端逐条核对的部分失败清单。
type BatchStateResultDTO struct {
	ReadsChanged     int `json:"readsChanged"`
	FavoritesChanged int `json:"favoritesChanged"`
}

// newBatchStateResult 把 service 的批量结果映射为 openapi 形状。
func newBatchStateResult(res *service.BatchSetResult) BatchStateResultDTO {
	if res == nil {
		return BatchStateResultDTO{}
	}
	return BatchStateResultDTO{
		ReadsChanged:     res.ReadsChanged,
		FavoritesChanged: res.FavoritesChanged,
	}
}

// batchStateMaxItems 是单次批量标记的条目上限（openapi BatchStateRequest.maxItems）。
//
// 与 service.batchLimit 同为 500。这是有意的重复而非疏漏：
// service 侧的检查在**事务开启之后**（它是最后一道防线，防内部调用方打爆写锁），
// 而传输层必须在**任何存在性查询之前**就拒绝超大请求 —— 否则一个 5000 条的
// 客户端请求会先被"文章不存在"的404 挡回，客户端拿到的错误码与真正的
// 问题（批次太大）不符，且白跑一次大 IN 查询。
// 与 dto_me.go 里 mergeMaxItems / service 的 mergeLimits 是同一处惯例。
const batchStateMaxItems = 500

// validate 校验批量标记请求。
func (r *BatchStateRequest) validate() error {
	if r.Read == nil {
		return apierr.Validation("批量标记参数校验失败", []apierr.Details{
			{Field: "read", Message: "read 为必填（true=标记已读，false=标记未读）；不传会被视为 false 从而抹掉已读记录"},
		})
	}
	if len(r.ArticleIDs) == 0 {
		return apierr.Validation("批量标记参数校验失败", []apierr.Details{
			{Field: "articleIds", Message: "不能为空"},
		})
	}
	// 上限判定必须在存在性校验之前（见 batchStateMaxItems 的说明）。
	if len(r.ArticleIDs) > batchStateMaxItems {
		return apierr.PayloadTooLarge("单次最多处理 " + itoa(batchStateMaxItems) + " 条")
	}
	for _, id := range r.ArticleIDs {
		if id <= 0 {
			return apierr.Validation("批量标记参数校验失败", []apierr.Details{
				{Field: "articleIds", Message: "articleId 必须是正整数"},
			})
		}
	}
	return nil
}

// PutPreferencesRequest 是 PUT /api/v1/me/preferences 的请求体。
type PutPreferencesRequest struct {
	Preferences map[string]string `json:"preferences"`
}

// validate 校验偏好写入请求。
func (r *PutPreferencesRequest) validate() error {
	if r.Preferences == nil {
		return apierr.Validation("偏好参数校验失败", []apierr.Details{
			{Field: "preferences", Message: "preferences 不能为空（整包覆盖语义）"},
		})
	}
	if len(r.Preferences) > 256 {
		return apierr.PayloadTooLarge("偏好项数量不能超过 256")
	}
	for key, value := range r.Preferences {
		if key == "" || len(key) > 128 {
			return apierr.Validation("偏好参数校验失败", []apierr.Details{
				{Field: "preferences", Message: "键不能为空且长度不能超过 128"},
			})
		}
		if len(value) > 4096 {
			return apierr.PayloadTooLarge("偏好项 " + key + " 的值过长")
		}
	}
	return nil
}

// StateItem 是收藏/已读增量流的单条（openapi StateItem）。
//
// ★ DEC-11 实现强制项：墓碑项（deleted=true）**必须随增量流返回**，
// 由客户端落本地删除；服务端查询禁止追加 AND deleted_at IS NULL。
//
// ★ 后 4 个字段是**可选摘要**（withArticles=true，默认开）：
// article 行已被归档物理删除时它们整体缺席（omitempty），
// 客户端按"缺字段"降级渲染即可，不该视为协议错误。
type StateItem struct {
	ArticleID int64 `json:"articleId"`
	Deleted   bool  `json:"deleted"`
	UpdatedAt int64 `json:"updatedAt"` // epoch 毫秒（openapi 明确为 integer）
	// 以下为可选摘要，格式与 ArticleDTO 对齐（publishedAt 为 ISO 8601 字符串）。
	Title       string `json:"title,omitempty"`
	Summary     string `json:"summary,omitempty"`
	SourceName  string `json:"sourceName,omitempty"`
	PublishedAt string `json:"publishedAt,omitempty"`
}

// StatePage 是收藏/已读增量页（openapi StatePage）。
type StatePage struct {
	Items        []StateItem `json:"items"`
	NextCursor   *string     `json:"nextCursor"`
	HasMore      bool        `json:"hasMore"`
	ServerTimeMs int64       `json:"serverTimeMs"`
}

// PreferencesDTO 是 GET /api/v1/me/preferences 的响应体。
type PreferencesDTO struct {
	Preferences map[string]string `json:"preferences"`
	UpdatedAt   string            `json:"updatedAt"`
}

// SessionsDTO 是 GET /api/v1/me/sessions 的响应体。
type SessionsDTO struct {
	Items []SessionDTO `json:"items"`
}

// applyBrief 把批量查回的摘要填到条目上。
//
// briefs 为 nil（withArticles=false）或该 id 查不到行时，四字段保持零值，
// 经 omitempty 后在 JSON 里整体消失 —— 老客户端拿到的形状与改动前完全一致。
func applyBrief(item *StateItem, briefs map[int64]model.ArticleBrief) {
	if briefs == nil {
		return
	}
	b, ok := briefs[item.ArticleID]
	if !ok {
		return
	}
	item.Title = b.Title
	item.Summary = b.Summary
	item.SourceName = b.SourceName
	item.PublishedAt = util.FormatTime(b.PublishedAt)
}

// newFavoritePage 由 service 的收藏视图构造增量页（含墓碑）。
func newFavoritePage(items []service.FavoriteView, next string, hasMore bool, nowMs int64,
	briefs map[int64]model.ArticleBrief) StatePage {
	out := StatePage{
		Items:        make([]StateItem, 0, len(items)),
		HasMore:      hasMore,
		ServerTimeMs: nowMs,
	}
	for _, f := range items {
		item := StateItem{
			ArticleID: f.ArticleID,
			Deleted:   f.Deleted, // ★ 墓碑必须返回（service 已按 deleted_at != 0 填好）
			UpdatedAt: f.UpdatedAt,
		}
		applyBrief(&item, briefs)
		out.Items = append(out.Items, item)
	}
	out.NextCursor = nextCursorPtr(next, hasMore)
	return out
}

// newReadPage 由 service 的已读视图构造增量页（含墓碑）。
func newReadPage(items []service.ReadView, next string, hasMore bool, nowMs int64,
	briefs map[int64]model.ArticleBrief) StatePage {
	out := StatePage{
		Items:        make([]StateItem, 0, len(items)),
		HasMore:      hasMore,
		ServerTimeMs: nowMs,
	}
	for _, r := range items {
		item := StateItem{
			ArticleID: r.ArticleID,
			Deleted:   r.Deleted, // ★ 墓碑必须返回
			UpdatedAt: r.UpdatedAt,
		}
		applyBrief(&item, briefs)
		out.Items = append(out.Items, item)
	}
	out.NextCursor = nextCursorPtr(next, hasMore)
	return out
}

// nextCursorPtr 按 openapi 语义返回 nextCursor：hasMore=false 时为 null。
func nextCursorPtr(next string, hasMore bool) *string {
	if !hasMore || next == "" {
		return nil
	}
	cursor := next
	return &cursor
}

// strPtrOrNil 把空串映射为 nil，使 JSON 输出为 null 而非 ""。
//
// 用于 refreshToken：Cookie 通道下 refresh token 由服务端持有，响应里应是 null。
// 若输出 ""，客户端会以为"拿到了一个有效但为空的令牌"而覆盖本地存储，造成登录态丢失。
func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

// newPreferencesDTO 由 service 的偏好视图列表折叠为 openapi 的 map 形状。
//
// updatedAt 取所有条目中的最大值（service 的 GetPrefs 对整包写入同一时间戳，
// 取最大值在部分更新场景下也正确）。
func newPreferencesDTO(views []service.PreferenceView) PreferencesDTO {
	kv := make(map[string]string, len(views))
	latest := ""
	for _, v := range views {
		kv[v.Key] = v.Value
		if v.UpdatedAt > latest {
			latest = v.UpdatedAt
		}
	}
	return PreferencesDTO{Preferences: kv, UpdatedAt: latest}
}

// newSessionDTOs 由 service 的会话视图列表转换，并保证空列表输出 [] 而非 null。
func newSessionDTOs(views []service.SessionView) SessionsDTO {
	out := SessionsDTO{Items: make([]SessionDTO, 0, len(views))}
	for _, v := range views {
		dto := SessionDTO{
			ID:         v.ID,
			ClientID:   v.ClientID,
			LastSeenAt: v.LastSeenAt,
			ExpiresAt:  v.ExpiresAt,
			CreatedAt:  v.CreatedAt,
			Current:    v.Current,
		}
		if name := strings.TrimSpace(v.DeviceName); name != "" {
			dto.DeviceName = &name
		}
		out.Items = append(out.Items, dto)
	}
	return out
}

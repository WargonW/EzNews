package service

import (
	"context"
	"log/slog"
	"strings"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/repo"
	"github.com/eznews/eznews/internal/store"
	"github.com/eznews/eznews/internal/util"
)

// UserStateService 负责登录用户的收藏 / 已读 / 偏好。
//
// ★ 关键不变量：增量同步接口**必须包含墓碑**（deleted_at != 0 的行）。
//
// 若在 SQL 里加上 `AND deleted_at IS NULL`，客户端永远收不到"这条要删"的信号，
// 合并后本地会残留一条幽灵收藏，且用户再也无法把它删掉（服务端已经没有这条有效记录，
// 而客户端删了也同步不上去）。这条被架构师一票否决，改动前请先读 ARCHITECTURE.md §9.4。
type UserStateService struct {
	state *repo.UserStateRepo
	// db 只用于批量写入时开事务；单条写入走 repo 内部连接。
	db *store.DB
}

// NewUserStateService 创建 UserStateService。
func NewUserStateService(state *repo.UserStateRepo, db *store.DB) *UserStateService {
	return &UserStateService{state: state, db: db}
}

// FavoriteView 是收藏项的对外表示。
//
// ★ UpdatedAt 是 **epoch 毫秒 int64**，不是 ISO 字符串。
//
//	openapi 的 StateItem.updatedAt 声明为 integer，客户端把它直接送进
//	POST /me/merge 作为 LWW 判据。如果这里输出 ISO 字符串，客户端必须反向解析，
//	解析失败时通常会退化成 0 —— 而 0 会被 normalizeStateTime 判为"客户端时钟异常"而压到"此刻"，
//	于是每次合并都把本地数据的 updated_at 刷成当前时间，LWW 永久失效。
//	传输格式错了会静默破坏合并语义，所以这里直接对齐 openapi。
type FavoriteView struct {
	ArticleID int64 `json:"articleId"`
	Deleted   bool  `json:"deleted"`
	UpdatedAt int64 `json:"updatedAt"`
}

// ReadView 是已读项的对外表示。
type ReadView struct {
	ArticleID int64 `json:"articleId"`
	Deleted   bool  `json:"deleted"`
	UpdatedAt int64 `json:"updatedAt"`
}

// StatePage 是一页收藏/已读增量数据。
type StatePage struct {
	Items      []FavoriteView `json:"items"`
	NextCursor string         `json:"nextCursor"`
	HasMore    bool           `json:"hasMore"`
}

// SetFavorite 设置/取消收藏。
//
// 登录态下时间戳由服务端决定（客户端时间不可信），
// 但仍需读旧值做 LWW 判定：若库内 updated_at 比本次请求更晚（并发写），不覆盖。
func (s *UserStateService) SetFavorite(ctx context.Context, userID, articleID int64, favorite bool) (*FavoriteView, error) {
	if articleID <= 0 {
		return nil, apierr.Validation("articleId 非法", []apierr.Details{{Field: "articleId", Message: "必须为正整数"}})
	}
	now := util.NowMs()
	if err := s.state.SetFavorite(ctx, userID, articleID, !favorite, now); err != nil {
		return nil, apierr.Internal(err)
	}
	return &FavoriteView{ArticleID: articleID, Deleted: !favorite, UpdatedAt: now}, nil
}

// SetRead 设置/取消已读。
func (s *UserStateService) SetRead(ctx context.Context, userID, articleID int64, read bool) (*ReadView, error) {
	if articleID <= 0 {
		return nil, apierr.Validation("articleId 非法", []apierr.Details{{Field: "articleId", Message: "必须为正整数"}})
	}
	now := util.NowMs()
	if err := s.state.SetRead(ctx, userID, articleID, !read, now); err != nil {
		return nil, apierr.Internal(err)
	}
	return &ReadView{ArticleID: articleID, Deleted: !read, UpdatedAt: now}, nil
}

// BatchSetStateRequest 是批量标记请求（阅读列表里"全部标记已读"这类操作）。
type BatchSetStateRequest struct {
	ArticleIDs []int64 `json:"articleIds"`
	Read       bool    `json:"read"`
	Favorite   *bool   `json:"favorite,omitempty"` // nil 表示不改动收藏
}

// BatchSetResult 是批量标记结果。
type BatchSetResult struct {
	FavoritesChanged int `json:"favoritesChanged"`
	ReadsChanged     int `json:"readsChanged"`
}

// batchLimit 是单次批量操作的条目上限。
const batchLimit = 500

// BatchSet 批量设置已读/收藏。
//
// ★ 整个批次在**同一个写事务**里执行：调用方（前端"全部标记已读"）要的是
// 全成功或全失败。若逐条autocommit，第 250 条失败时前 249 条已经落库且无法回滚，
// 用户会看到"部分生效"的中间态——重试又会把已生效的条目重复处理。
//
// 同时也因为在事务内，N 条写入只做一次 commit/fsync，而不是 N 次；
// SQLite 写事务本就是全局串行的，拆开只会得到 N 次锁竞争 + N 次 fsync。
//
// 幂等性仍由 ux_fav/ux_read 唯一索引 + ON CONFLICT DO UPDATE 保证，
// 所以重放整个批次是安全的。
func (s *UserStateService) BatchSet(ctx context.Context, userID int64, req BatchSetStateRequest) (*BatchSetResult, error) {
	if len(req.ArticleIDs) == 0 {
		return &BatchSetResult{}, nil
	}
	if len(req.ArticleIDs) > batchLimit {
		return nil, apierr.PayloadTooLarge("单次最多处理 500 条")
	}
	// 去重并丢弃非法值。
	seen := make(map[int64]struct{}, len(req.ArticleIDs))
	ids := make([]int64, 0, len(req.ArticleIDs))
	for _, id := range req.ArticleIDs {
		if id <= 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return &BatchSetResult{}, nil
	}

	now := util.NowMs()
	res := &BatchSetResult{}

	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	// ★ 拿到 tx 之后，所有写入都必须走 tx（*Tx 版本），
	// 绝不能回头调 s.state.SetRead/SetFavorite —— 那两个走 r.db 连接池，
	// 会与本事务抢全局写锁 → SQLITE_BUSY。
	defer func() { _ = tx.Rollback() }()

	for _, id := range ids {
		if err := s.state.SetReadTx(ctx, tx, userID, id, !req.Read, now); err != nil {
			return nil, apierr.Internal(err)
		}
		res.ReadsChanged++
		if req.Favorite != nil {
			if err := s.state.SetFavoriteTx(ctx, tx, userID, id, !*req.Favorite, now); err != nil {
				return nil, apierr.Internal(err)
			}
			res.FavoritesChanged++
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, apierr.Internal(err)
	}
	return res, nil
}

// ListFavoritesDelta 按增量游标返回收藏变化（含墓碑）。
func (s *UserStateService) ListFavoritesDelta(ctx context.Context, userID int64, since model.Cursor, limit int) (*StatePage, error) {
	limit = clampStateLimit(limit)
	rows, err := s.state.ListFavorites(ctx, userID, since, limit+1)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	items := make([]FavoriteView, 0, len(rows))
	for _, r := range rows {
		items = append(items, FavoriteView{
			ArticleID: r.ArticleID,
			Deleted:   r.DeletedAt != 0,
			UpdatedAt: r.UpdatedAt,
		})
	}
	return &StatePage{
		Items:      items,
		NextCursor: model.EncodeCursor(StateLastCursor(rows, since).TS, StateLastCursor(rows, since).ID),
		HasMore:    hasMore,
	}, nil
}

// ReadPage 是一页已读增量数据。
type ReadPage struct {
	Items      []ReadView `json:"items"`
	NextCursor string     `json:"nextCursor"`
	HasMore    bool       `json:"hasMore"`
}

// ListReadsDelta 按增量游标返回已读变化（含墓碑）。
func (s *UserStateService) ListReadsDelta(ctx context.Context, userID int64, since model.Cursor, limit int) (*ReadPage, error) {
	limit = clampStateLimit(limit)
	rows, err := s.state.ListReads(ctx, userID, since, limit+1)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	items := make([]ReadView, 0, len(rows))
	for _, r := range rows {
		items = append(items, ReadView{
			ArticleID: r.ArticleID,
			Deleted:   r.DeletedAt != 0,
			UpdatedAt: r.UpdatedAt,
		})
	}
	last := StateLastCursorFromReads(rows, since)
	return &ReadPage{
		Items:      items,
		NextCursor: model.EncodeCursor(last.TS, last.ID),
		HasMore:    hasMore,
	}, nil
}

// StateLastCursorFromReads 从已读结果集推导下一页游标。
// 收藏与已读同构但类型不同，故需单独一份（避免把 Read 强转成 Favorite）。
func StateLastCursorFromReads(items []model.Read, fallback model.Cursor) model.Cursor {
	if len(items) == 0 {
		return fallback
	}
	last := items[len(items)-1]
	return model.Cursor{TS: last.UpdatedAt, ID: last.ID}
}

// PreferenceView 是偏好项。
type PreferenceView struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	UpdatedAt string `json:"updatedAt"`
}

// GetPrefs 返回用户的全部偏好。
func (s *UserStateService) GetPrefs(ctx context.Context, userID int64) ([]PreferenceView, error) {
	kv, updatedAt, err := s.state.GetPrefs(ctx, userID)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	out := make([]PreferenceView, 0, len(kv))
	for k, v := range kv {
		out = append(out, PreferenceView{Key: k, Value: v, UpdatedAt: util.FormatTime(updatedAt)})
	}
	slog.Debug("读取用户偏好", slog.Int64("userId", userID), slog.Int("count", len(out)))
	return out, nil
}

// prefsLimit 是单次偏好提交的键数上限。
const prefsLimit = 64

// prefsKeyMaxLen 是单个偏好键的最大长度。
const prefsKeyMaxLen = 64

// ReplacePrefs 整包覆盖偏好。
//
// 用 PUT 语义而非 PATCH：偏好是"客户端本地设置整包上传"，
// 服务端不做逐键合并（Merge Policy v1 已明确服务端有值以服务端为准，
// 但显式保存是用户主动行为，应整包覆盖）。
func (s *UserStateService) ReplacePrefs(ctx context.Context, userID int64, prefs map[string]string) ([]PreferenceView, error) {
	for k := range prefs {
		if k == "" || len(k) > prefsKeyMaxLen {
			return nil, apierr.Validation("偏好键非法（长度 1-"+itoa(prefsKeyMaxLen)+"）",
				[]apierr.Details{{Field: "preferences", Message: "键名非法: " + k}})
		}
	}
	if len(prefs) > prefsLimit {
		return nil, apierr.PayloadTooLarge("单次最多提交 64 项偏好")
	}

	// 整包事务写入：DELETE + 批量 INSERT 必须在同一事务，否则中间态会让并发读看到"偏好全空"。
	if err := s.replacePrefsTx(ctx, userID, prefs); err != nil {
		return nil, err
	}
	return s.GetPrefs(ctx, userID)
}

func (s *UserStateService) replacePrefsTx(ctx context.Context, userID int64, prefs map[string]string) error {
	db, err := s.state.DB()
	if err != nil {
		return apierr.Internal(err)
	}
	tx, err := db.BeginWrite(ctx)
	if err != nil {
		return apierr.Internal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.state.ReplacePrefs(ctx, tx, userID, prefs, util.NowMs()); err != nil {
		return apierr.Internal(err)
	}
	if err := tx.Commit(); err != nil {
		return apierr.Internal(err)
	}
	return nil
}

// FavoriteFlags 是文章列表里的用户态标记。
type FavoriteFlags struct {
	Favorites map[int64]bool // articleId -> 已收藏
	Reads     map[int64]bool // articleId -> 已读
}

// LoadFlags 批量查询一组文章的用户态标记（游客态跳过：userID<=0 时返回全 false）。
//
// 这是"游客可访问 + 登录后附加 isFavorited/isRead"的实现：
// 游客请求时直接把 userID=0 传进来，返回空标记，不做任何查询。
func (s *UserStateService) LoadFlags(ctx context.Context, userID int64, articleIDs []int64) FavoriteFlags {
	flags := FavoriteFlags{
		Favorites: make(map[int64]bool, len(articleIDs)),
		Reads:     make(map[int64]bool, len(articleIDs)),
	}
	if userID <= 0 || len(articleIDs) == 0 {
		return flags
	}
	if favs, err := s.state.ListFavoritesByIDs(ctx, userID, articleIDs); err == nil {
		for _, f := range favs {
			flags.Favorites[f.ArticleID] = f.DeletedAt == 0
		}
	} else {
		slog.Warn("批量查询收藏标记失败", slog.Int64("userId", userID), slog.String("err", err.Error()))
	}
	if reads, err := s.state.ListReadsByIDs(ctx, userID, articleIDs); err == nil {
		for _, r := range reads {
			flags.Reads[r.ArticleID] = r.DeletedAt == 0
		}
	} else {
		slog.Warn("批量查询已读标记失败", slog.Int64("userId", userID), slog.String("err", err.Error()))
	}
	return flags
}

// 收藏/已读分页参数，对齐 openapi StateLimit（default 200 / maximum 1000）。
const (
	stateDefaultLimit = 200
	stateMaxLimit     = 1000
)

// clampStateLimit 把客户端传入的 limit 收敛到 [1, 1000]（openapi StateLimit 声明）。
//
// 必须设上限：limit=999999 会让单次查询把该用户整张状态表拉进内存。
// 上限取 1000 是因为这些行极窄（article_id + deleted_at + 两个时间戳 ≈ 32 字节），
// 1000 行约 32 KB，对常驻内存目标无压力；而用 200 会让"万条收藏"的用户
// 要翻 50 页才能同步完一次。
func clampStateLimit(limit int) int {
	switch {
	case limit <= 0:
		return stateDefaultLimit
	case limit > stateMaxLimit:
		return stateMaxLimit
	default:
		return limit
	}
}

// itoa 是 strconv.Itoa 的本地短别名（本文件仅用于拼接错误文案，避免为一个调用引入 import）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// normalizeKey 去除偏好键的首尾空白（避免 " voice" 与 "voice" 变成两条记录）。
func normalizeKey(k string) string { return strings.TrimSpace(k) }

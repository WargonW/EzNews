package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/repo"
	"github.com/eznews/eznews/internal/store"
	"github.com/eznews/eznews/internal/util"
)

// MergeService 实现登录后「游客本地数据 → 账号」的合并（PRD-ACCOUNT §7 / Merge Policy v1）。
//
// ★ 合并策略 v1（改之前务必读完，语义反了会丢用户数据）：
//
//	收藏 / 已读：并集 + LWW(updated_at) + 墓碑。
//	  - 同一 (article_id) 两端都有，按 updated_at 大者胜；
//	  - 客户端删（墓碑，deleted_at != 0）与服务端存（deleted_at == 0）相遇，
//	    **谁的 updated_at 大听谁的**。不能写成"删除优先"——
//	    因为用户在另一台设备上重新收藏后会更新 updated_at，删除优先会把新收藏吃掉。
//	  - 墓碑必须落库并随增量流下发，否则客户端无法得知"这条要删掉"，
//	    合并后的增量同步会永远留着一条幽灵收藏。
//	偏好：整包 KV，服务端有值以服务端为准（INSERT OR IGNORE 语义）。
//	  理由：偏好是"设置"而非"事件"，没有天然的 LWW 依据，
//	  且客户端本地偏好可能是很久以前的陈旧值，覆盖服务端新值会造成回退。
//
// 幂等：以 (user_id, client_id, nonce) 为幂等键，重复提交直接回放上次结果快照。
// 这不是优化——客户端在网络超时后必然重试，缺少幂等会让收藏翻倍。
type MergeService struct {
	db    *store.DB
	state *repo.UserStateRepo
	users *repo.UserRepo
}

// NewMergeService 创建 MergeService。
func NewMergeService(db *store.DB, state *repo.UserStateRepo, users *repo.UserRepo) *MergeService {
	return &MergeService{db: db, state: state, users: users}
}

// StateItem 是客户端上送的单条收藏/已读状态。
type StateItem struct {
	ArticleID int64 `json:"articleId"`
	// Deleted 表示客户端侧"已取消"（墓碑）。
	Deleted bool `json:"deleted"`
	// UpdatedAt 是客户端产生该操作的时间（epoch 毫秒）。
	//
	// 语义警告：客户端时钟可能不准。若全部为 0 或明显超前（未来时间），
	// 会让客户端数据永久"赢过"服务端数据，服务端后续修改再也同步不下去。
	// 因此服务端对明显不合理的值做钳制（见 normalizeStateTime）。
	UpdatedAt int64 `json:"updatedAt"`
	// CreatedAt 可选，缺失时用 UpdatedAt。
	CreatedAt int64 `json:"createdAt,omitempty"`
}

// MergeRequest 是 POST /me/merge 的请求体。
type MergeRequest struct {
	ClientID string `json:"clientId"`
	// Nonce 是幂等键。客户端每次"一批待合并数据"生成一个 UUID；
	// 重试同一批数据时必须复用同一个 nonce。
	Nonce string `json:"nonce"`
	// Favorites / Reads 是待合并的游客态收藏/已读。
	Favorites []StateItem `json:"favorites"`
	Reads     []StateItem `json:"reads"`
	// Preferences 是待合并的游客态偏好 KV（map<string, JSON 标量字符串>）。
	Preferences map[string]string `json:"preferences"`
}

// MergeResult 是合并结果回执，也是幂等重放的内容。
type MergeResult struct {
	// FavoritesAdded / Merged / Skipped 是三类结果计数。
	// Added = 服务端原本没有；Merged = 两端都有且发生了覆盖；Skipped = 服务端更新，保留服务端。
	FavoritesAdded   int `json:"favoritesAdded"`
	FavoritesMerged  int `json:"favoritesMerged"`
	FavoritesSkipped int `json:"favoritesSkipped"`
	ReadsAdded       int `json:"readsAdded"`
	ReadsMerged      int `json:"readsMerged"`
	ReadsSkipped     int `json:"readsSkipped"`
	// PreferencesKept / PreferencesWritten 分别是"沿用服务端值"与"客户端补进服务端"的键数。
	PreferencesKept    int    `json:"preferencesKept"`
	PreferencesWritten int    `json:"preferencesWritten"`
	Replayed           bool   `json:"replayed"`        // true = 命中幂等日志，本次未实际写入
	ServerFavorites    int    `json:"serverFavorites"` // 合并后服务端收藏总数（不含墓碑）
	ServerReads        int    `json:"serverReads"`
	ServerPreferences  int    `json:"serverPreferences"`
	NextCursor         string `json:"nextCursor"` // 客户端应从此游标继续增量拉取
	CompletedAt        string `json:"completedAt"`
}

// mergeLimits 是单次合并的硬上限，防止恶意/失控客户端用超大请求打爆内存与写锁。
var mergeLimits = struct {
	Favorites   int
	Reads       int
	Preferences int
}{Favorites: 2000, Reads: 5000, Preferences: 64}

// Merge 执行一次合并。
func (s *MergeService) Merge(ctx context.Context, userID int64, req MergeRequest) (*MergeResult, error) {
	clientID := trimSpace(req.ClientID)
	nonce := trimSpace(req.Nonce)
	if clientID == "" {
		return nil, apierr.Validation("缺少 clientId",
			[]apierr.Details{{Field: "clientId", Message: "必填"}})
	}
	if nonce == "" {
		return nil, apierr.Validation("缺少 nonce",
			[]apierr.Details{{Field: "nonce", Message: "必填，用于幂等重试"}})
	}
	if len(req.Favorites) > mergeLimits.Favorites || len(req.Reads) > mergeLimits.Reads {
		return nil, apierr.PayloadTooLarge(fmt.Sprintf(
			"单次合并上限为 收藏 %d / 已读 %d 条，请分批提交", mergeLimits.Favorites, mergeLimits.Reads))
	}
	if len(req.Preferences) > mergeLimits.Preferences {
		return nil, apierr.PayloadTooLarge(fmt.Sprintf("单次合并偏好上限为 %d 项", mergeLimits.Preferences))
	}

	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	defer func() { _ = tx.Rollback() }()

	// 幂等：先查日志。命中即回放，不重复写入。
	if log, err := s.state.FindMergeLog(ctx, tx, userID, clientID, nonce); err == nil && log != nil {
		res := &MergeResult{}
		if jsonErr := json.Unmarshal([]byte(log.ResultJSON), res); jsonErr != nil {
			// 快照损坏不应阻断客户端登录，降级为重新执行合并。
			slog.Error("merge 幂等快照解析失败，将重新执行合并",
				slog.Int64("userId", userID), slog.String("err", jsonErr.Error()))
		} else {
			res.Replayed = true
			res.CompletedAt = util.FormatTime(util.NowMs())
			return res, nil
		}
	} else if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return nil, apierr.Internal(err)
	}

	now := util.NowMs()
	res := &MergeResult{CompletedAt: util.FormatTime(now)}

	favIDs := articleIDsOf(req.Favorites)
	existingFav, err := s.state.FavoriteUpdatedAt(ctx, tx, userID, favIDs)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	for _, item := range req.Favorites {
		_, exists := existingFav[item.ArticleID]
		created := normalizeStateTime(item.CreatedAt, now)
		updAt := normalizeStateTime(item.UpdatedAt, now)

		wrote, err := s.state.UpsertFavorite(ctx, tx, &model.Favorite{
			UserID:    userID,
			ArticleID: item.ArticleID,
			DeletedAt: deletedStamp(item.Deleted, updAt),
			CreatedAt: created,
			UpdatedAt: updAt,
		})
		if err != nil {
			return nil, apierr.Internal(err)
		}
		switch {
		case !exists:
			res.FavoritesAdded++
		case wrote:
			res.FavoritesMerged++
		default:
			// Upsert 的 WHERE excluded.updated_at > 当前值 未成立 → 服务端更新，保留服务端。
			res.FavoritesSkipped++
		}
	}

	readIDs := articleIDsOf(req.Reads)
	existingRead, err := s.state.ReadUpdatedAt(ctx, tx, userID, readIDs)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	for _, item := range req.Reads {
		updAt := normalizeStateTime(item.UpdatedAt, now)
		created := normalizeStateTime(item.CreatedAt, now)
		_, exists := existingRead[item.ArticleID]

		wrote, err := s.state.UpsertRead(ctx, tx, &model.Read{
			UserID:    userID,
			ArticleID: item.ArticleID,
			DeletedAt: deletedStamp(item.Deleted, updAt),
			CreatedAt: created,
			UpdatedAt: updAt,
		})
		if err != nil {
			return nil, apierr.Internal(err)
		}
		switch {
		case !exists:
			res.ReadsAdded++
		case wrote:
			res.ReadsMerged++
		default:
			res.ReadsSkipped++
		}
	}

	// 偏好：服务端有值以服务端为准（INSERT OR IGNORE）。
	if len(req.Preferences) > 0 {
		written, err := s.state.InsertPrefsIfAbsent(ctx, tx, userID, req.Preferences, now)
		if err != nil {
			return nil, apierr.Internal(err)
		}
		res.PreferencesWritten = written
		res.PreferencesKept = len(req.Preferences) - written
	}

	// 统计合并后的服务端规模（不含墓碑）。
	if err := s.countState(ctx, tx, userID, res); err != nil {
		return nil, apierr.Internal(err)
	}

	// 合并是"全量对账"的一次性动作，合并后客户端应从零游标重新拉一次增量，
	// 以服务端为唯一事实来源对齐（墓碑也会随之下发，清掉本地幽灵记录）。
	res.NextCursor = ""

	snapshot, err := json.Marshal(res)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	if err := s.state.InsertMergeLog(ctx, tx, &model.MergeLog{
		UserID:     userID,
		ClientID:   clientID,
		Nonce:      nonce,
		ResultJSON: string(snapshot),
		CreatedAt:  now,
	}); err != nil {
		return nil, apierr.Internal(err)
	}

	if err := tx.Commit(); err != nil {
		return nil, apierr.Internal(err)
	}
	slog.Info("合并完成",
		slog.Int64("userId", userID), slog.String("clientId", clientID),
		slog.Int("favAdded", res.FavoritesAdded), slog.Int("favMerged", res.FavoritesMerged),
		slog.Int("readAdded", res.ReadsAdded), slog.Int("prefWritten", res.PreferencesWritten))
	return res, nil
}

// countState 统计服务端收藏/已读/偏好的有效条数。
func (s *MergeService) countState(ctx context.Context, tx store.Session, userID int64, res *MergeResult) error {
	n, err := s.state.CountActive(ctx, tx, repo.FavoriteTable(), userID)
	if err != nil {
		return err
	}
	res.ServerFavorites = n

	n, err = s.state.CountActive(ctx, tx, repo.ReadTable(), userID)
	if err != nil {
		return err
	}
	res.ServerReads = n

	n, err = s.state.CountPreferences(ctx, tx, userID)
	if err != nil {
		return err
	}
	res.ServerPreferences = n
	return nil
}

// deletedStamp 返回应写入 deleted_at 的值（0 表示有效）。
func deletedStamp(deleted bool, updatedAt int64) int64 {
	if !deleted {
		return 0
	}
	if updatedAt <= 0 {
		return util.NowMs()
	}
	return updatedAt
}

// normalizeStateTime 钳制客户端时间戳。
//
// 客户端时钟不可信，若原样入库会造成两类永久性故障：
//   - 上送 0：所有条目 updated_at=0，永远赢不过服务端，之后服务端改不动；
//   - 上送未来时间（如设备时区错乱）：同理，且客户端重装后数据无法收敛。
//
// 策略：≤0 视为"此刻"；超前超过 7 天则压到"此刻"（明确的时钟错误）；
// 其余值原样保留（允许正常的小幅时钟偏差参与 LWW）。
func normalizeStateTime(v, now int64) int64 {
	const maxFutureSkewMs = 7 * 24 * 3600 * 1000
	if v <= 0 || v > now+maxFutureSkewMs {
		return now
	}
	return v
}

// articleIDsOf 提取条目中的 article_id 列表（去重）。
func articleIDsOf(items []StateItem) []int64 {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(items))
	out := make([]int64, 0, len(items))
	for _, it := range items {
		if it.ArticleID <= 0 {
			continue
		}
		if _, dup := seen[it.ArticleID]; dup {
			continue
		}
		seen[it.ArticleID] = struct{}{}
		out = append(out, it.ArticleID)
	}
	return out
}

// trimSpace 去除首尾空白（避免各处重复 import strings）。
func trimSpace(s string) string { return strings.TrimSpace(s) }

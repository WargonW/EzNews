package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/store"
)

// UserStateRepo 是收藏 / 已读 / 偏好 / 合并日志的数据访问对象。
type UserStateRepo struct {
	db *store.DB
}

// NewUserStateRepo 创建 UserStateRepo。
func NewUserStateRepo(db *store.DB) *UserStateRepo {
	return &UserStateRepo{db: db}
}

const favoriteCols = `id, user_id, article_id, IFNULL(deleted_at,0) AS deleted_at, created_at, updated_at`

// stateTable 描述收藏/已读两张同构表的元信息，避免重复实现。
type stateTable struct {
	name   string
	unique string
}

var (
	// favTable 是收藏表元信息。
	favTable = stateTable{name: "user_favorite"}
	// readTable 是已读表元信息。
	readTable = stateTable{name: "user_read"}
)

// FavoriteTable 返回收藏表元信息。
//
// stateTable 是未导出类型，但 Go 允许外部包通过 := 推断并作为参数回传，
// 因此 service 层无需知道底层表名。
func FavoriteTable() stateTable { return favTable }

// ReadTable 返回已读表元信息。
func ReadTable() stateTable { return readTable }

// ListFavoritesByIDs 批量查询收藏标记（供文章列表附加 isFavorited）。
// 返回的行**可能含墓碑**，调用方需自行判断 DeletedAt == 0。
func (r *UserStateRepo) ListFavoritesByIDs(ctx context.Context, userID int64, articleIDs []int64) ([]model.Favorite, error) {
	return r.listStateByIDs(ctx, favTable, userID, articleIDs)
}

// ListReadsByIDs 批量查询已读标记（供文章列表附加 isRead）。
func (r *UserStateRepo) ListReadsByIDs(ctx context.Context, userID int64, articleIDs []int64) ([]model.Read, error) {
	rows, err := r.listStateByIDs(ctx, readTable, userID, articleIDs)
	if err != nil {
		return nil, err
	}
	out := make([]model.Read, 0, len(rows))
	for _, f := range rows {
		out = append(out, model.Read{
			ID: f.ID, UserID: f.UserID, ArticleID: f.ArticleID,
			DeletedAt: f.DeletedAt, CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt,
		})
	}
	return out, nil
}

func (r *UserStateRepo) listStateByIDs(ctx context.Context, t stateTable, userID int64, articleIDs []int64) ([]model.Favorite, error) {
	var out []model.Favorite
	for _, chunk := range chunkInt64(articleIDs, 400) {
		ph := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		args := append([]any{userID}, toAnyInt64(chunk)...)
		rows, err := r.db.QueryContext(ctx,
			"SELECT "+favoriteCols+" FROM "+t.name+" WHERE user_id=? AND article_id IN ("+ph+")", args...)
		if err != nil {
			return nil, fmt.Errorf("批量查询状态失败: %w", err)
		}
		for rows.Next() {
			var f model.Favorite
			if err := rows.Scan(&f.ID, &f.UserID, &f.ArticleID, &f.DeletedAt, &f.CreatedAt, &f.UpdatedAt); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out = append(out, f)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DB 暴露底层句柄，供需要在同一事务内组合多项写操作的上层使用。
func (r *UserStateRepo) DB() (*store.DB, error) {
	if r == nil || r.db == nil {
		return nil, ErrNotFound
	}
	return r.db, nil
}

// FavoriteUpdatedAt 批量查询已存在收藏的 updated_at（合并时区分 added / merged / skipped）。
func (r *UserStateRepo) FavoriteUpdatedAt(ctx context.Context, tx store.Session, userID int64, articleIDs []int64) (map[int64]int64, error) {
	return r.ExistingUpdatedAt(ctx, tx, favTable, userID, articleIDs)
}

// ReadUpdatedAt 批量查询已存在已读标记的 updated_at。
func (r *UserStateRepo) ReadUpdatedAt(ctx context.Context, tx store.Session, userID int64, articleIDs []int64) (map[int64]int64, error) {
	return r.ExistingUpdatedAt(ctx, tx, readTable, userID, articleIDs)
}

// CountActive 统计某用户某张状态表中未删除（无墓碑）的条目数。
func (r *UserStateRepo) CountActive(ctx context.Context, tx store.Session, t stateTable, userID int64) (int, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+t.name+" WHERE user_id=? AND IFNULL(deleted_at,0)=0", userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计状态条数失败: %w", err)
	}
	return n, nil
}

// CountPreferences 统计用户偏好键数。
func (r *UserStateRepo) CountPreferences(ctx context.Context, tx store.Session, userID int64) (int, error) {
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_preference WHERE user_id=?", userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计偏好条数失败: %w", err)
	}
	return n, nil
}

// ExistingUpdatedAt 批量查询已存在条目的 updated_at（用于区分 added / merged / skipped）。
func (r *UserStateRepo) ExistingUpdatedAt(ctx context.Context, tx store.Session, t stateTable, userID int64, articleIDs []int64) (map[int64]int64, error) {
	out := make(map[int64]int64, len(articleIDs))
	if len(articleIDs) == 0 {
		return out, nil
	}
	for _, chunk := range chunkInt64(articleIDs, 500) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		args := append([]any{userID}, toAnyInt64(chunk)...)
		rows, err := tx.QueryContext(ctx,
			"SELECT article_id, updated_at FROM "+t.name+" WHERE user_id=? AND article_id IN ("+placeholders+")", args...)
		if err != nil {
			return nil, fmt.Errorf("查询已存在状态失败: %w", err)
		}
		for rows.Next() {
			var articleID, updatedAt int64
			if err := rows.Scan(&articleID, &updatedAt); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[articleID] = updatedAt
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// UpsertFavorite 按 LWW(updated_at) + 墓碑语义写入收藏；返回是否发生了写入。
func (r *UserStateRepo) UpsertFavorite(ctx context.Context, tx store.Session, f *model.Favorite) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO user_favorite (user_id, article_id, deleted_at, created_at, updated_at) VALUES (?,?,?,?,?)
		 ON CONFLICT(user_id, article_id) DO UPDATE SET
			deleted_at = excluded.deleted_at,
			updated_at = excluded.updated_at
		 WHERE excluded.updated_at > user_favorite.updated_at`,
		f.UserID, f.ArticleID, nullInt64(f.DeletedAt), f.CreatedAt, f.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("写入收藏失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpsertRead 按 LWW(updated_at) + 墓碑语义写入已读；返回是否发生了写入。
func (r *UserStateRepo) UpsertRead(ctx context.Context, tx store.Session, rd *model.Read) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO user_read (user_id, article_id, deleted_at, created_at, updated_at) VALUES (?,?,?,?,?)
		 ON CONFLICT(user_id, article_id) DO UPDATE SET
			deleted_at = excluded.deleted_at,
			updated_at = excluded.updated_at
		 WHERE excluded.updated_at > user_read.updated_at`,
		rd.UserID, rd.ArticleID, nullInt64(rd.DeletedAt), rd.CreatedAt, rd.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("写入已读失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetFavorite 单条设置收藏/取消收藏（登录态直接写入，服务端时间为准）。
func (r *UserStateRepo) SetFavorite(ctx context.Context, userID, articleID int64, deleted bool, now int64) error {
	return r.SetFavoriteTx(ctx, r.db, userID, articleID, deleted, now)
}

// SetFavoriteTx 是 SetFavorite 的事务版本，供批量写入把 N 条合并为一个事务。
//
// ★ 传进来的 tx 必须是调用方持有的那个写事务；本方法内部**不得**再走 r.db，
// 否则两个写事务互等 → SQLITE_BUSY（见 ARCHITECTURE.md 的 WAL 事务边界不变量）。
func (r *UserStateRepo) SetFavoriteTx(ctx context.Context, tx store.Session, userID, articleID int64, deleted bool, now int64) error {
	var deletedAt any
	if deleted {
		deletedAt = now
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO user_favorite (user_id, article_id, deleted_at, created_at, updated_at) VALUES (?,?,?,?,?)
		 ON CONFLICT(user_id, article_id) DO UPDATE SET deleted_at=excluded.deleted_at, updated_at=excluded.updated_at`,
		userID, articleID, deletedAt, now, now)
	if err != nil {
		return fmt.Errorf("设置收藏失败: %w", err)
	}
	return nil
}

// SetRead 单条设置已读/取消已读。
func (r *UserStateRepo) SetRead(ctx context.Context, userID, articleID int64, deleted bool, now int64) error {
	return r.SetReadTx(ctx, r.db, userID, articleID, deleted, now)
}

// SetReadTx 是 SetRead 的事务版本，约束同 SetFavoriteTx。
func (r *UserStateRepo) SetReadTx(ctx context.Context, tx store.Session, userID, articleID int64, deleted bool, now int64) error {
	var deletedAt any
	if deleted {
		deletedAt = now
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO user_read (user_id, article_id, deleted_at, created_at, updated_at) VALUES (?,?,?,?,?)
		 ON CONFLICT(user_id, article_id) DO UPDATE SET deleted_at=excluded.deleted_at, updated_at=excluded.updated_at`,
		userID, articleID, deletedAt, now, now)
	if err != nil {
		return fmt.Errorf("设置已读失败: %w", err)
	}
	return nil
}

// ListFavorites 按增量游标返回收藏（含墓碑，客户端据此本地删除）。
func (r *UserStateRepo) ListFavorites(ctx context.Context, userID int64, since model.Cursor, limit int) ([]model.Favorite, error) {
	return r.listState(ctx, favTable, userID, since, limit)
}

// ListReads 按增量游标返回已读（含墓碑）。
func (r *UserStateRepo) ListReads(ctx context.Context, userID int64, since model.Cursor, limit int) ([]model.Read, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+favoriteCols+" FROM "+readTable.name+" WHERE user_id=? AND (updated_at, id) > (?, ?) ORDER BY updated_at ASC, id ASC LIMIT ?",
		userID, since.TS, since.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("查询已读列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.Read
	for rows.Next() {
		rd := model.Read{}
		if err := rows.Scan(&rd.ID, &rd.UserID, &rd.ArticleID, &rd.DeletedAt, &rd.CreatedAt, &rd.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, rd)
	}
	return out, rows.Err()
}

// listState 是收藏列表的通用实现（与已读同构）。
func (r *UserStateRepo) listState(ctx context.Context, t stateTable, userID int64, since model.Cursor, limit int) ([]model.Favorite, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+favoriteCols+" FROM "+t.name+" WHERE user_id=? AND (updated_at, id) > (?, ?) ORDER BY updated_at ASC, id ASC LIMIT ?",
		userID, since.TS, since.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("查询状态列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.Favorite
	for rows.Next() {
		f := model.Favorite{}
		if err := rows.Scan(&f.ID, &f.UserID, &f.ArticleID, &f.DeletedAt, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetPrefs 读取偏好 KV 全量，返回 map 与最后更新时间。
func (r *UserStateRepo) GetPrefs(ctx context.Context, userID int64) (map[string]string, int64, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT key, value, updated_at FROM user_preference WHERE user_id=?", userID)
	if err != nil {
		return nil, 0, fmt.Errorf("查询偏好失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]string)
	var maxUpdated int64
	for rows.Next() {
		var k, v string
		var updatedAt int64
		if err := rows.Scan(&k, &v, &updatedAt); err != nil {
			return nil, 0, err
		}
		out[k] = v
		if updatedAt > maxUpdated {
			maxUpdated = updatedAt
		}
	}
	return out, maxUpdated, rows.Err()
}

// ReplacePrefs 整包覆盖偏好 KV（天然幂等）。
func (r *UserStateRepo) ReplacePrefs(ctx context.Context, tx store.Session, userID int64, kv map[string]string, now int64) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM user_preference WHERE user_id=?", userID); err != nil {
		return fmt.Errorf("清空偏好失败: %w", err)
	}
	for k, v := range kv {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO user_preference (user_id, key, value, updated_at) VALUES (?,?,?,?)",
			userID, k, v, now); err != nil {
			return fmt.Errorf("写入偏好失败: %w", err)
		}
	}
	return nil
}

// InsertPrefsIfAbsent 仅在 key 不存在时写入（合并时"服务端有值以服务端为准"）。
// 返回实际写入的键数量。
func (r *UserStateRepo) InsertPrefsIfAbsent(ctx context.Context, tx store.Session, userID int64, kv map[string]string, now int64) (int, error) {
	written := 0
	for k, v := range kv {
		res, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO user_preference (user_id, key, value, updated_at) VALUES (?,?,?,?)",
			userID, k, v, now)
		if err != nil {
			return written, fmt.Errorf("写入偏好失败: %w", err)
		}
		n, err := res.RowsAffected()
		if err == nil && n > 0 {
			written++
		}
	}
	return written, nil
}

// FindMergeLog 按幂等键 (user_id, client_id, nonce) 查询合并日志。
func (r *UserStateRepo) FindMergeLog(ctx context.Context, tx store.Session, userID int64, clientID, nonce string) (*model.MergeLog, error) {
	row := tx.QueryRowContext(ctx,
		"SELECT id, user_id, client_id, nonce, result_json, created_at FROM merge_log WHERE user_id=? AND client_id=? AND nonce=?",
		userID, clientID, nonce)
	m := &model.MergeLog{}
	if err := row.Scan(&m.ID, &m.UserID, &m.ClientID, &m.Nonce, &m.ResultJSON, &m.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("查询合并日志失败: %w", err)
	}
	return m, nil
}

// InsertMergeLog 写入合并结果快照。
func (r *UserStateRepo) InsertMergeLog(ctx context.Context, tx store.Session, m *model.MergeLog) error {
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO merge_log (user_id, client_id, nonce, result_json, created_at) VALUES (?,?,?,?,?)",
		m.UserID, m.ClientID, m.Nonce, m.ResultJSON, m.CreatedAt); err != nil {
		return fmt.Errorf("写入合并日志失败: %w", err)
	}
	return nil
}

// DeleteMergeLogs 清理用户的合并日志（注销账号时的显式级联）。
func (r *UserStateRepo) DeleteMergeLogs(ctx context.Context, tx store.Session, userID int64) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM merge_log WHERE user_id=?", userID); err != nil {
		return fmt.Errorf("清理合并日志失败: %w", err)
	}
	return nil
}

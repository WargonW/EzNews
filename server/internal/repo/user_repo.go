package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/store"
)

// UserRepo 是 user / session 两张表的数据访问对象。
type UserRepo struct {
	db *store.DB
}

// NewUserRepo 创建 UserRepo。
func NewUserRepo(db *store.DB) *UserRepo {
	return &UserRepo{db: db}
}

// userCols 是 user 表的查询列清单。
//
// ★ disabled_at 用 IFNULL 兜成 0：Go 侧用 int64 表示"停用时刻"，
//
//	0 天然表示未停用，不必为了 nullable 再引入 sql.NullInt64 —— 后者一旦
//	忘了判 Valid 就 silently 取到零值，是这类 Bug 的常见来源。
const userCols = `id, username, password_hash, IFNULL(email,'') AS email, role, token_version,
	created_at, updated_at, IFNULL(last_login_at,0) AS last_login_at,
	IFNULL(disabled_at,0) AS disabled_at`

const sessionCols = `id, user_id, refresh_token_hash, IFNULL(device_name,'') AS device_name, client_id,
	last_seen_at, expires_at, IFNULL(revoked_at,0) AS revoked_at, created_at,
	IFNULL(prev_refresh_token_hash,'') AS prev_refresh_token_hash, IFNULL(prev_rotated_at,0) AS prev_rotated_at`

// FindByUsername 按用户名查询账号（大小写敏感）。
func (r *UserRepo) FindByUsername(ctx context.Context, username string) (*model.User, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+userCols+" FROM user WHERE username=?", username)
	return scanUser(row)
}

// FindByID 按主键查询账号。
func (r *UserRepo) FindByID(ctx context.Context, id int64) (*model.User, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+userCols+" FROM user WHERE id=?", id)
	return scanUser(row)
}

// Create 创建账号并返回主键。
func (r *UserRepo) Create(ctx context.Context, tx store.Session, u *model.User) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO user (username, password_hash, email, role, token_version, created_at, updated_at, last_login_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		u.Username, u.PasswordHash, nullString(u.Email), u.Role, u.TokenVersion,
		u.CreatedAt, u.UpdatedAt, nullInt64(u.LastLoginAt))
	if err != nil {
		return 0, fmt.Errorf("创建账号失败: %w", err)
	}
	return res.LastInsertId()
}

// BumpTokenVersion 递增 token_version（全局登出 / 改密），返回新值。
func (r *UserRepo) BumpTokenVersion(ctx context.Context, tx store.Session, id int64, now int64) (int32, error) {
	if _, err := tx.ExecContext(ctx,
		"UPDATE user SET token_version = token_version + 1, updated_at=? WHERE id=?", now, id); err != nil {
		return 0, fmt.Errorf("递增 token_version 失败: %w", err)
	}
	var v int32
	if err := tx.QueryRowContext(ctx, "SELECT token_version FROM user WHERE id=?", id).Scan(&v); err != nil {
		return 0, fmt.Errorf("读取 token_version 失败: %w", err)
	}
	return v, nil
}

// UpdatePassword 更新密码哈希。
func (r *UserRepo) UpdatePassword(ctx context.Context, tx store.Session, id int64, hash string, now int64) error {
	if _, err := tx.ExecContext(ctx, "UPDATE user SET password_hash=?, updated_at=? WHERE id=?", hash, now, id); err != nil {
		return fmt.Errorf("更新密码失败: %w", err)
	}
	return nil
}

// UpdateLastLogin 刷新最后登录时间。
func (r *UserRepo) UpdateLastLogin(ctx context.Context, tx store.Session, id, now int64) error {
	if _, err := tx.ExecContext(ctx, "UPDATE user SET last_login_at=?, updated_at=? WHERE id=?", now, now, id); err != nil {
		return fmt.Errorf("更新登录时间失败: %w", err)
	}
	return nil
}

// GetTokenVersion 读取 token_version（供 tv 校验缓存回源）。
func (r *UserRepo) GetTokenVersion(ctx context.Context, id int64) (int32, error) {
	var v int32
	if err := r.db.QueryRowContext(ctx, "SELECT token_version FROM user WHERE id=?", id).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return v, nil
}

// GetAccountGuard 一次查询同时取回 token_version 与停用标记。
//
// ★ 与 GetTokenVersion 查同一张表的同一行，因此合并成一次查询是纯粹的收益：
//
//	鉴权热路径本来就要为 tv 打一次主键索引，顺带回读 disabled_at 多加一列零成本。
//	若拆成两个方法，"停用拦截"就会变成每请求第二次点查 —— 而它拦的正是
//	一个应当被立刻关在系统外的人，多一次往返毫无意义。
func (r *UserRepo) GetAccountGuard(ctx context.Context, id int64) (tv int32, disabled bool, err error) {
	var d int64
	e := r.db.QueryRowContext(ctx,
		"SELECT token_version, IFNULL(disabled_at,0) FROM user WHERE id=?", id).Scan(&tv, &d)
	if e != nil {
		if errors.Is(e, sql.ErrNoRows) {
			return 0, false, ErrNotFound
		}
		return 0, false, fmt.Errorf("查询账号状态失败: %w", e)
	}
	return tv, d > 0, nil
}

// InsertSession 写入新会话并返回主键。
func (r *UserRepo) InsertSession(ctx context.Context, tx store.Session, s *model.Session) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO session (user_id, refresh_token_hash, device_name, client_id, last_seen_at, expires_at, revoked_at, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		s.UserID, s.RefreshTokenHash, nullString(s.DeviceName), s.ClientID, s.LastSeenAt, s.ExpiresAt,
		nullInt64(s.RevokedAt), s.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("创建会话失败: %w", err)
	}
	return res.LastInsertId()
}

// FindSessionByID 按主键查询会话，并校验归属。
//
// userID 必须参与匹配：没有它，一次越权的 sessionId 探测就能读到别人的会话行，
// 进而拿到 client_id / device_name 等可关联到具体设备的信息。
func (r *UserRepo) FindSessionByID(ctx context.Context, id, userID int64) (*model.Session, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+sessionCols+" FROM session WHERE id=? AND user_id=?", id, userID)
	return scanSession(row)
}

// FindSessionByHash 按 refresh token 哈希查询会话（含已吊销的，供 reuse 检测）。
func (r *UserRepo) FindSessionByHash(ctx context.Context, hash string) (*model.Session, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+sessionCols+" FROM session WHERE refresh_token_hash=?", hash)
	return scanSession(row)
}

// FindSessionByPrevHash 按「上一代 refresh token 哈希」查询会话。
//
// 这是 DEC-7 两级判定的核心：轮换时旧哈希被移入 prev_refresh_token_hash（而非丢弃），
// 因此宽限窗口内（prev_rotated_at + graceSec 内）且 client_id 相同的重放可以被识别为
// 「同设备网络重试」而非「令牌泄露」，从而不触发全量撤销。
// 只查最近一代 prev：宽限期最长仅 60s，更早的代次已被覆盖。
func (r *UserRepo) FindSessionByPrevHash(ctx context.Context, hash string) (*model.Session, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+sessionCols+" FROM session WHERE prev_refresh_token_hash=?", hash)
	return scanSession(row)
}

// RotateSession 轮换刷新令牌：原地 UPDATE 同一行。
//
// 行身份不变量（禁止改成 delete+insert）：
//   - DEC-7：旧哈希必须移入 prev_refresh_token_hash 才有地方落，宽限重放才能识别；
//     delete+insert 会让 prev_* 永远为 NULL，60s 宽限窗口形同虚设，
//     Android 弱网下重放旧 token 会被误判为泄露，导致该用户全部设备被踢下线。
//   - DEC-14：sid（= session.id）被 JWT 携带并在登出时用于定位。
//     delete+insert 会让每次刷新都产生新 id，活跃会话的 sid 与库里对不上，登出等于没做。
//
// 返回受影响的行数；调用方须校验 ==1，否则说明会话已被并发吊销。
func (r *UserRepo) RotateSession(ctx context.Context, tx store.Session, oldID int64, s *model.Session, now int64) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE session
		    SET prev_refresh_token_hash = refresh_token_hash,
		        prev_rotated_at         = ?,
		        refresh_token_hash      = ?,
		        device_name             = COALESCE(NULLIF(?,''), device_name),
		        last_seen_at            = ?,
		        expires_at              = ?
		  WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		now, s.RefreshTokenHash, s.DeviceName, now, s.ExpiresAt, oldID, s.UserID)
	if err != nil {
		return 0, fmt.Errorf("轮换会话令牌失败: %w", err)
	}
	return res.RowsAffected()
}

// ClearPrevHash 清空上一代哈希（宽限期结束后调用，防止 prev 列长期滞留旧哈希）。
func (r *UserRepo) ClearPrevHash(ctx context.Context, tx store.Session, id int64) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE session SET prev_refresh_token_hash=NULL, prev_rotated_at=NULL WHERE id=?`, id); err != nil {
		return fmt.Errorf("清理上一代哈希失败: %w", err)
	}
	return nil
}

// IsSessionActive 判断会话是否属于该用户且未被吊销（access token 热路径专用）。
//
// 只 SELECT 常量 1、走主键索引，是本方法能进热路径的前提：
// access token 验签后每请求都要问一次"这个 sid 还活着吗"，任何多余列/多余连接都会
// 把它从"一次廉价主键点查"变成不可接受的开销。
//
// 行不存在返回 (false, nil) 而非错误：会话行被 Janitor 清理掉，同样意味着凭证已失效，
// 调用方按"已吊销"处理即可，把它当错误只会让上层多写一个无意义的错误分支。
//
// 带 AND user_id=? 是纵深防御：万一 sid 因任何原因指向了他人会话，fail-closed 直接拒绝。
// 同一个主键索引，不增加任何成本。
//
// 走 r.db 而非 tx：本方法只在事务外的只读热路径调用（与 GetTokenVersion 同理），
// 因此使用连接池是正确的——事务内调用方才必须走 tx。
func (r *UserRepo) IsSessionActive(ctx context.Context, id, userID int64) (bool, error) {
	var one int
	err := r.db.QueryRowContext(ctx,
		"SELECT 1 FROM session WHERE id=? AND user_id=? AND revoked_at IS NULL", id, userID).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("查询会话状态失败: %w", err)
	}
	return true, nil
}

// RevokeSession 吊销单个会话（踢下线）。
func (r *UserRepo) RevokeSession(ctx context.Context, tx store.Session, id, userID, now int64) error {
	res, err := tx.ExecContext(ctx,
		"UPDATE session SET revoked_at=? WHERE id=? AND user_id=? AND revoked_at IS NULL", now, id, userID)
	if err != nil {
		return fmt.Errorf("吊销会话失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeAllSessions 吊销某用户的全部会话（reuse 检测命中 / 改密）。
func (r *UserRepo) RevokeAllSessions(ctx context.Context, tx store.Session, userID, now int64) (int64, error) {
	res, err := tx.ExecContext(ctx,
		"UPDATE session SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL", now, userID)
	if err != nil {
		return 0, fmt.Errorf("吊销全部会话失败: %w", err)
	}
	return res.RowsAffected()
}

// ListSessions 列出用户的活跃（未吊销且未过期）会话。
func (r *UserRepo) ListSessions(ctx context.Context, userID int64, now int64) ([]model.Session, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+sessionCols+" FROM session WHERE user_id=? AND revoked_at IS NULL AND expires_at > ? ORDER BY last_seen_at DESC",
		userID, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// DeleteExpiredSessions 清理过期/已吊销会话（并入 Janitor 执行）。
func (r *UserRepo) DeleteExpiredSessions(ctx context.Context, tx store.Session, now, revokedRetentionMs int64) (int64, error) {
	res, err := tx.ExecContext(ctx,
		"DELETE FROM session WHERE expires_at < ? OR (revoked_at IS NOT NULL AND revoked_at < ?)",
		now, now-revokedRetentionMs)
	if err != nil {
		return 0, fmt.Errorf("清理过期会话失败: %w", err)
	}
	return res.RowsAffected()
}

// DeleteUser 删除账号；显式清理关联数据以兼容 foreign_keys 未生效的连接。
func (r *UserRepo) DeleteUser(ctx context.Context, tx store.Session, userID int64) error {
	for _, stmt := range []string{
		"DELETE FROM session WHERE user_id=?",
		"DELETE FROM user_favorite WHERE user_id=?",
		"DELETE FROM user_read WHERE user_id=?",
		"DELETE FROM user_preference WHERE user_id=?",
		"DELETE FROM merge_log WHERE user_id=?",
		"DELETE FROM source WHERE owner_user_id=?",
	} {
		if _, err := tx.ExecContext(ctx, stmt, userID); err != nil {
			return fmt.Errorf("删除账号关联数据失败: %w", err)
		}
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM user WHERE id=?", userID)
	if err != nil {
		return fmt.Errorf("删除账号失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// scanUser 扫描一行 user。
func scanUser(row scanner) (*model.User, error) {
	u := &model.User{}
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Email, &u.Role, &u.TokenVersion,
		&u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt, &u.DisabledAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("扫描账号失败: %w", err)
	}
	return u, nil
}

// scanSession 扫描一行 session。
func scanSession(row scanner) (*model.Session, error) {
	s := &model.Session{}
	if err := row.Scan(&s.ID, &s.UserID, &s.RefreshTokenHash, &s.DeviceName, &s.ClientID,
		&s.LastSeenAt, &s.ExpiresAt, &s.RevokedAt, &s.CreatedAt,
		&s.PrevRefreshTokenHash, &s.PrevRotatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("扫描会话失败: %w", err)
	}
	return s, nil
}

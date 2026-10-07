// Package repo 是数据访问层：只写 SQL，不含业务规则。
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

// ErrNotFound 表示目标行不存在（由 repository 层统一返回，便于上层映射 404）。
var ErrNotFound = errors.New("记录不存在")

// SourceRepo 是 source 表的数据访问对象。
type SourceRepo struct {
	db *store.DB
}

// NewSourceRepo 创建 SourceRepo。
func NewSourceRepo(db *store.DB) *SourceRepo {
	return &SourceRepo{db: db}
}

const sourceColumns = `id, key, name, url, type, category, is_default, enabled, suggest_interval,
	icon_url, language, remark, IFNULL(owner_user_id,0), created_at, updated_at`

// GetByID 按主键查询源。
func (r *SourceRepo) GetByID(ctx context.Context, id int64) (*model.Source, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+sourceColumns+" FROM source WHERE id=?", id)
	return scanSource(row)
}

// GetByKey 按业务键查询源。
func (r *SourceRepo) GetByKey(ctx context.Context, key string) (*model.Source, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+sourceColumns+" FROM source WHERE key=?", key)
	return scanSource(row)
}

// List 返回源列表；includeDisabled=false 时只返回启用中的源。
func (r *SourceRepo) List(ctx context.Context, includeDisabled bool) ([]model.Source, error) {
	q := "SELECT " + sourceColumns + " FROM source"
	if !includeDisabled {
		q += " WHERE enabled=1"
	}
	q += " ORDER BY is_default DESC, category, name"
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("查询源列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.Source
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// BatchGetByKeys 批量按业务键查询源，返回 key → Source 映射。
func (r *SourceRepo) BatchGetByKeys(ctx context.Context, keys []string) (map[string]*model.Source, error) {
	out := make(map[string]*model.Source, len(keys))
	for _, chunk := range chunkStrings(keys, 500) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		rows, err := r.db.QueryContext(ctx,
			"SELECT "+sourceColumns+" FROM source WHERE key IN ("+placeholders+")", toAnySlice(chunk)...)
		if err != nil {
			return nil, fmt.Errorf("批量查询源失败: %w", err)
		}
		for rows.Next() {
			s, err := scanSource(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[s.Key] = s
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Create 插入新源并返回主键。
func (r *SourceRepo) Create(ctx context.Context, s *model.Source) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO source (key, name, url, type, category, is_default, enabled, suggest_interval,
			icon_url, language, remark, owner_user_id, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.Key, s.Name, s.URL, string(s.Type), s.Category, boolToInt(s.IsDefault), boolToInt(s.Enabled),
		s.SuggestInterval, nullString(s.IconURL), nullString(s.Language), nullString(s.Remark),
		nullInt64(s.OwnerUserID), s.CreatedAt, s.UpdatedAt)
	if err != nil {
		return 0, fmt.Errorf("创建源失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, nil
}

// Update 全量更新源的可编辑字段（key 与 is_default 不可变，由 service 层保证）。
func (r *SourceRepo) Update(ctx context.Context, s *model.Source) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE source SET name=?, url=?, type=?, category=?, enabled=?, suggest_interval=?,
			icon_url=?, language=?, remark=?, updated_at=? WHERE id=?`,
		s.Name, s.URL, string(s.Type), s.Category, boolToInt(s.Enabled), s.SuggestInterval,
		nullString(s.IconURL), nullString(s.Language), nullString(s.Remark), s.UpdatedAt, s.ID)
	if err != nil {
		return fmt.Errorf("更新源失败: %w", err)
	}
	return nil
}

// SetEnabled 启停单个源。
func (r *SourceRepo) SetEnabled(ctx context.Context, id int64, enabled bool, now int64) error {
	_, err := r.db.ExecContext(ctx, "UPDATE source SET enabled=?, updated_at=? WHERE id=?",
		boolToInt(enabled), now, id)
	if err != nil {
		return fmt.Errorf("设置源启停失败: %w", err)
	}
	return nil
}

// Delete 删除源（service 层负责校验 is_default）。
//
// ★ tx 必传，不允许传 nil 走 r.db 的"兼容路径"：
//
// 删除源是复合写（先删 audio / audio_task / article，再删 source），service 层用 BeginWrite
// 开启 BEGIN IMMEDIATE 事务并持有全局写锁。若这里绕过 tx 从连接池另取连接写 source，
// 新事务拿不到写锁，而原事务又在等本方法返回才提交 —— 互相等待直到 busyTimeout 耗尽，
// 必然 SQLITE_BUSY（WAL 下写-写不阻塞，但对写事务的第二个写入会立即 BUSY）。
// 静默回退到 r.db 等于把这个坑留在原地，所以显式报错。
func (r *SourceRepo) Delete(ctx context.Context, tx store.Session, id int64) error {
	if tx == nil {
		return errors.New("删除源失败: 缺少写事务（tx 为 nil）")
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM source WHERE id=?", id)
	if err != nil {
		return fmt.Errorf("删除源失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountArticles 批量统计各源的文章数，返回 sourceID → count。
func (r *SourceRepo) CountArticles(ctx context.Context) (map[int64]int64, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT source_id, COUNT(*) FROM article GROUP BY source_id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int64]int64)
	for rows.Next() {
		var id, n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

// scanSource 从一行扫描出 Source。
func scanSource(row scanner) (*model.Source, error) {
	var (
		s           model.Source
		isDefault   int
		enabled     int
		iconURL     sql.NullString
		language    sql.NullString
		remark      sql.NullString
		ownerUserID sql.NullInt64
	)
	if err := row.Scan(&s.ID, &s.Key, &s.Name, &s.URL, &s.Type, &s.Category, &isDefault, &enabled,
		&s.SuggestInterval, &iconURL, &language, &remark, &ownerUserID, &s.CreatedAt, &s.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("扫描源失败: %w", err)
	}
	s.IsDefault = isDefault != 0
	s.Enabled = enabled != 0
	s.IconURL = iconURL.String
	s.Language = language.String
	s.Remark = remark.String
	s.OwnerUserID = ownerUserID.Int64
	return &s, nil
}

// boolToInt 将 bool 转为 SQLite 的 0/1。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullString 将空串转为 NULL。
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullInt64 将 0 转为 NULL。
func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

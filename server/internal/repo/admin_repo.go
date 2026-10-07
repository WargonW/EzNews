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

// 本文件承载管理后台所需的账号数据访问。刻意与 user_repo.go 分开：
// 常规账号路径（注册 / 登录 / 会话）的热路径不该被管理功能的复杂度拖累。

// AdminUser 是管理后台看到的一个用户。
//
// ★ 刻意不复用 model.User：后者带着 PasswordHash。
//
//	把它序列化进管理接口的响应，等于把 Argon2id 摘要交给前端 ——
//	虽然 hash 不能直接反推密码，但这是**毫无必要的攻击面暴露**
//	（离线撞库、彩虹表比对都因此成为可能）。宁可多定义一个结构体。
type AdminUser struct {
	ID          int64
	Username    string
	Email       string
	Role        string
	Disabled    bool
	CreatedAt   int64
	LastLoginAt int64

	// 以下是详情接口才填充的派生字段，列表查询刻意不算
	// （它们需要对三张表 COUNT，全表量级下代价明显）。
	SessionCount  int
	FavoriteCount int
	ReadCount     int
}

type AdminUserFilter struct {
	Query  string // 按用户名/邮箱模糊匹配
	Role   string // 精确过滤：user / admin，空串表示不过滤
	Limit  int
	Cursor int64 // >0 表示翻页模式：id < Cursor
}

// buildAdminUserWhere 拼出列表查询的公共部分。
//
// ★ 刻意不做成"先取 SQL 字符串、再返回拼参数的闭包"：那样 b.String() 在
//
//	闭包执行前就被求值，闭包里追加的 LIKE 条件会**静默丢失** ——
//	表现形式是"搜索框没反应但不报错"，属于最难查的一类 Bug。
//	一次性把 SQL 和参数一起算完，从结构上杜绝这种分离。
func buildAdminUserWhere(f AdminUserFilter) (string, []any) {
	var b strings.Builder
	var args []any
	b.WriteString(" WHERE 1=1")
	if f.Query != "" {
		// LIKE 的 % _ \ 必须转义，否则用户搜 "100%" 会变成通配符。
		pattern := "%" + escapeLike(f.Query) + "%"
		b.WriteString(` AND (username LIKE ? ESCAPE '\' OR IFNULL(email,'') LIKE ? ESCAPE '\')`)
		args = append(args, pattern, pattern)
	}
	if f.Role != "" {
		b.WriteString(" AND role=?")
		args = append(args, f.Role)
	}
	if f.Cursor > 0 {
		b.WriteString(" AND id < ?")
		args = append(args, f.Cursor)
	}
	return b.String(), args
}

const adminUserCols = `id, username, IFNULL(email,'') AS email, role, disabled_at,
	created_at, IFNULL(last_login_at,0) AS last_login_at`

// AdminListUsers 分页列出用户。
//
// 排序用 id DESC：管理后台关心的是"最近注册的谁"，且 id 天然唯一，
// 不存在同序导致的翻页错位。
func (r *UserRepo) AdminListUsers(ctx context.Context, f AdminUserFilter) ([]AdminUser, error) {
	if f.Limit <= 0 {
		f.Limit = 20
	}
	where, args := buildAdminUserWhere(f)
	args = append(args, f.Limit)

	rows, err := r.db.QueryContext(ctx,
		"SELECT "+adminUserCols+" FROM user"+where+" ORDER BY id DESC LIMIT ?", args...)
	if err != nil {
		return nil, fmt.Errorf("查询用户列表失败: %w", err)
	}
	defer rows.Close()

	out := make([]AdminUser, 0, f.Limit)
	for rows.Next() {
		var u AdminUser
		var disabledAt sql.NullInt64
		if err := rows.Scan(&u.ID, &u.Username, &u.Email, &u.Role, &disabledAt,
			&u.CreatedAt, &u.LastLoginAt); err != nil {
			return nil, fmt.Errorf("扫描用户失败: %w", err)
		}
		u.Disabled = disabledAt.Valid && disabledAt.Int64 > 0
		out = append(out, u)
	}
	return out, rows.Err()
}

// AdminCountUsers 按同一组过滤条件统计用户总数（给分页用）。
//
// 复用 buildAdminUserWhere 而不是手写一遍条件：两处条件一旦漂移，
// 表现又是那种不报错的错 —— 第一页显示 20 条，却告诉你总共 3 条。
func (r *UserRepo) AdminCountUsers(ctx context.Context, f AdminUserFilter) (int64, error) {
	// ★ 计数查询必须丢掉游标：否则"第二页的总数"会比第一页小，
	//   前端据此算出的页数会随翻页不断缩水。
	f.Cursor = 0
	where, args := buildAdminUserWhere(f)
	var n int64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM user"+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计用户数失败: %w", err)
	}
	return n, nil
}

// AdminGetUser 取单个用户的管理视图（含派生计数）。
func (r *UserRepo) AdminGetUser(ctx context.Context, id int64, now int64) (*AdminUser, error) {
	u := &AdminUser{}
	var disabledAt sql.NullInt64
	err := r.db.QueryRowContext(ctx,
		`SELECT id, username, IFNULL(email,'') AS email, role, disabled_at,
		 created_at, IFNULL(last_login_at,0) AS last_login_at FROM user WHERE id=?`, id).
		Scan(&u.ID, &u.Username, &u.Email, &u.Role, &disabledAt, &u.CreatedAt, &u.LastLoginAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("查询用户失败: %w", err)
	}
	u.Disabled = disabledAt.Valid && disabledAt.Int64 > 0

	// 派生计数：三条 COUNT，失败不致命（列表里少个数字不影响判断），
	// 但仍然返回 error 而不是静默吞掉 —— 静默吞掉的失败永远没人发现。
	if err := r.fillUserCounts(ctx, u, now); err != nil {
		return nil, err
	}
	return u, nil
}

func (r *UserRepo) fillUserCounts(ctx context.Context, u *AdminUser, now int64) error {
	// ★ "有效会话"必须是 expires_at > now（真正的未过期），不能用 last_login_at 比。
	//   用后者会让一个上周登录、会话早已过期的人显示"还在线"，
	//   管理员据此点了停用以为能立刻踢下线，其实什么都没发生。
	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM session WHERE user_id=? AND revoked_at IS NULL AND expires_at>?",
		u.ID, now).Scan(&u.SessionCount); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("统计会话数失败: %w", err)
	}
	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM user_favorite WHERE user_id=? AND deleted_at IS NULL", u.ID).
		Scan(&u.FavoriteCount); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("统计收藏数失败: %w", err)
	}
	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM user_read WHERE user_id=? AND deleted_at IS NULL", u.ID).
		Scan(&u.ReadCount); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("统计已读数失败: %w", err)
	}
	return nil
}

// AdminSetRole 修改角色。返回受影响行数（0 表示用户不存在）。
//
// ★ 调用方必须在事务里自行完成「最后一个管理员」的守卫，本方法不做业务判断 ——
//
//	repo 层的职责是忠实地执行写入，把策略留给 service。
func (r *UserRepo) AdminSetRole(ctx context.Context, tx store.Session, id int64, role string, now int64) error {
	res, err := tx.ExecContext(ctx, "UPDATE user SET role=?, updated_at=? WHERE id=?", role, now, id)
	if err != nil {
		return fmt.Errorf("修改角色失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取影响行数失败: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AdminSetDisabled 停用 / 启用账户。disabled=false 时把列写回 NULL 而非 0，
// 以保证「启用」后账户状态与从未停用过的账户完全一致。
func (r *UserRepo) AdminSetDisabled(ctx context.Context, tx store.Session, id int64, disabled bool, now int64) error {
	var v any
	if disabled {
		v = now
	}
	res, err := tx.ExecContext(ctx, "UPDATE user SET disabled_at=?, updated_at=? WHERE id=?", v, now, id)
	if err != nil {
		return fmt.Errorf("修改停用状态失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取影响行数失败: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AdminDeleteUser 删除账号及其个人数据（会话、收藏、已读、偏好、合并日志）。
//
// ★ 刻意不删文章与新闻源：它们是全局共享数据，不属于任何用户。
//
//	删号连带删内容会让一个用户的离职操作抹掉系统里其他人正在读的东西。
//
// 顺序刻意：先删从表再删主表。外键虽已配 ON DELETE CASCADE，
// 但显式删除让依赖一目了然 —— 将来换成不支持 CASCADE 的存储也不至于烂尾。
func (r *UserRepo) AdminDeleteUser(ctx context.Context, tx store.Session, id int64) error {
	tables := []string{"session", "user_favorite", "user_read", "user_preference", "merge_log"}
	for _, t := range tables {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+t+" WHERE user_id=?", id); err != nil {
			return fmt.Errorf("清理 %s 失败: %w", t, err)
		}
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM user WHERE id=?", id)
	if err != nil {
		return fmt.Errorf("删除账号失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取影响行数失败: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AdminRevokeAllSessions 吊销某用户的全部会话（踢下线所有设备）。
// 返回实际影响行数。
func (r *UserRepo) AdminRevokeAllSessions(ctx context.Context, tx store.Session, id int64, now int64) (int64, error) {
	res, err := tx.ExecContext(ctx,
		"UPDATE session SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL", now, id)
	if err != nil {
		return 0, fmt.Errorf("吊销会话失败: %w", err)
	}
	return res.RowsAffected()
}

// AdminCounts 是概览页要的一组计数。
type AdminCounts struct {
	Users          int64
	Admins         int64
	DisabledUsers  int64
	ActiveSessions int64
	Articles       int64
	Sources        int64
	Audios         int64
}

// AdminCountsOnce 一次性算出全部计数。
//
// 用一条 SQL + 子查询而不是七条独立 COUNT：SQLite 里每条语句都是一次
// 独立的执行开销，而这里的数据量（几条越来越多的计数）合并起来毫无压力。
func (r *UserRepo) AdminCountsOnce(ctx context.Context, now int64) (AdminCounts, error) {
	var c AdminCounts
	err := r.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM user),
			(SELECT COUNT(*) FROM user WHERE role=?),
			(SELECT COUNT(*) FROM user WHERE disabled_at IS NOT NULL),
			(SELECT COUNT(*) FROM session WHERE revoked_at IS NULL AND expires_at>?),
			(SELECT COUNT(*) FROM article),
			(SELECT COUNT(*) FROM source),
			(SELECT COUNT(*) FROM audio)
	`, model.RoleAdmin, now).Scan(
		&c.Users, &c.Admins, &c.DisabledUsers, &c.ActiveSessions, &c.Articles, &c.Sources, &c.Audios)
	if err != nil {
		return c, fmt.Errorf("统计概览数据失败: %w", err)
	}
	return c, nil
}

// AdminCountAdmins 统计可用管理员数量（未停用）。
//
// ★ 这是「最后一个管理员」守卫的地基：所有降权/停用/删除操作都必须先问它。
//
//	一旦条件写错（比如漏了 disabled_at IS NULL），系统就可能被永久锁死 ——
//	没有 SMTP、没有找回入口，除了直接改库再没有别的办法回来。
func (r *UserRepo) AdminCountAdmins(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM user WHERE role=? AND disabled_at IS NULL", model.RoleAdmin).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("统计管理员数量失败: %w", err)
	}
	return n, nil
}

// AdminGetAccountState 取权限判定所需的最小字段。
//
// ★ 这是 JWT 里**没有** role 的代价：每个管理请求都要一次主键点查。
//
//	换来的是降权立即生效 —— 不需要依赖"改角色时记得 bump token_version"
//	这种人为纪律。管理端点调用量极低，这个取舍稳赚。
func (r *UserRepo) AdminGetAccountState(ctx context.Context, id int64) (role string, disabled bool, err error) {
	var disabledAt sql.NullInt64
	e := r.db.QueryRowContext(ctx, "SELECT role, disabled_at FROM user WHERE id=?", id).
		Scan(&role, &disabledAt)
	if e != nil {
		if errors.Is(e, sql.ErrNoRows) {
			return "", false, ErrNotFound
		}
		return "", false, fmt.Errorf("查询账号状态失败: %w", e)
	}
	return role, disabledAt.Valid && disabledAt.Int64 > 0, nil
}

// AdminPromoteFirstUser 把当前唯一的用户提升为管理员（冷启动 CLI 用）。
//
// 返回 ErrNotFound 表示用户不存在。
func (r *UserRepo) AdminPromoteFirstUser(ctx context.Context, tx store.Session, id int64, now int64) error {
	res, err := tx.ExecContext(ctx,
		"UPDATE user SET role=?, updated_at=? WHERE id=? AND role<>?",
		model.RoleAdmin, now, id, model.RoleAdmin)
	if err != nil {
		return fmt.Errorf("提升管理员失败: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// 语音任务 / 内容 / 系统 —— 管理后台的"运维视角"查询。
//
// 这些查询刻意放在本文件而不是各自的 *_repo.go：它们不是业务的读写路径
// （那些路径关心单条记录的 CRUD），而是管理后台特有的聚合视角。
// 混在一起只会让 article_repo.go 这类热路径文件越滚越长。
// ---------------------------------------------------------------------------

// AdminAudioTask 是管理后台看到的一条合成任务。
//
// ArticleTitle 用 LEFT JOIN 取：管理员看到的是"哪篇文章的合成挂了"，
// 只给 article_id 等于让人自己去查。文章被删时任务也会级联删除，
// 所以 LEFT JOIN 的 NULL 分支实际走不到，但仍保持 LEFT 以免运维手贱删了数据导致整页 500。
type AdminAudioTask struct {
	ID           int64
	AudioID      int64 // ready 后回填的音频 ID；0 表示尚未产出
	ArticleID    int64
	ArticleTitle string
	Voice        string
	Speed        float64
	Status       string
	Provider     string
	ErrorCode    string
	ErrorMsg     string
	RetryCount   int
	TextChars    int
	CreatedAt    int64
	UpdatedAt    int64
}

// AdminListAudioTasks 列出最近的任务。status 为空表示不过滤；cursor>0 时取 id<cursor。
//
// ★ 排序刻意用**纯 id DESC**，而不是"failed 优先"：
//
//	后者拿不到稳定的 keyset 游标（排序键是派生值，翻页会在边界漏行/重行），
//	结果就是任务一多就只能一次性全量返回。
//	"先看到失败任务"这个诉求改由**调用方默认传 status=failed + 返回 statusCounts** 满足 ——
//	页面打开就能看到"失败 3 条"的筛选入口，效果一样，还能翻页。
func (r *AudioRepo) AdminListAudioTasks(ctx context.Context, status string, limit, cursor int64) ([]AdminAudioTask, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	q := `SELECT t.id, IFNULL(t.audio_id,0), t.article_id, IFNULL(a.title,''), t.voice, t.speed, t.status,
		IFNULL(t.provider,''), IFNULL(t.error_code,''), IFNULL(t.error_msg,''),
		t.retry_count, t.text_chars, t.created_at, t.updated_at
		FROM audio_task t LEFT JOIN article a ON a.id=t.article_id`
	args := []any{}
	conds := []string{}
	if status != "" {
		conds = append(conds, "t.status=?")
		args = append(args, status)
	}
	if cursor > 0 {
		conds = append(conds, "t.id < ?")
		args = append(args, cursor)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY t.id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询语音任务失败: %w", err)
	}
	defer rows.Close()

	out := make([]AdminAudioTask, 0, limit)
	for rows.Next() {
		var t AdminAudioTask
		if err := rows.Scan(&t.ID, &t.AudioID, &t.ArticleID, &t.ArticleTitle, &t.Voice, &t.Speed,
			&t.Status, &t.Provider, &t.ErrorCode, &t.ErrorMsg, &t.RetryCount,
			&t.TextChars, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("扫描语音任务失败: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AdminCountAudioTasks 按同一过滤条件统计任务总数（给分页用）。
func (r *AudioRepo) AdminCountAudioTasks(ctx context.Context, status string) (int64, error) {
	q := "SELECT COUNT(*) FROM audio_task"
	var args []any
	if status != "" {
		q += " WHERE status=?"
		args = append(args, status)
	}
	var n int64
	if err := r.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计语音任务数失败: %w", err)
	}
	return n, nil
}

// AdminAudioStats 是语音侧的一组聚合指标。
type AdminAudioStats struct {
	TasksByStatus map[string]int64
	AudioCount    int64
	AudioBytes    int64
	FailedTasks   int64
}

// AdminAudioStats 统计语音任务分布与音频占用。
func (r *AudioRepo) AdminAudioStats(ctx context.Context) (AdminAudioStats, error) {
	st := AdminAudioStats{TasksByStatus: map[string]int64{}}
	rows, err := r.db.QueryContext(ctx, "SELECT status, COUNT(*) FROM audio_task GROUP BY status")
	if err != nil {
		return st, fmt.Errorf("统计任务状态失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			return st, fmt.Errorf("扫描任务状态失败: %w", err)
		}
		st.TasksByStatus[k] = v
	}
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("遍历任务状态失败: %w", err)
	}
	st.FailedTasks = st.TasksByStatus[string(model.TaskStatusFailed)]

	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*), IFNULL(SUM(size_bytes),0) FROM audio").
		Scan(&st.AudioCount, &st.AudioBytes); err != nil {
		return st, fmt.Errorf("统计音频占用失败: %w", err)
	}
	return st, nil
}

// AdminSourceStat 是单个新闻源的内容贡献。
type AdminSourceStat struct {
	SourceID     int64
	SourceName   string
	Enabled      bool
	ArticleCount int64
	LastSeenAt   int64
}

// AdminContentStats 是内容侧聚合。
type AdminContentStats struct {
	ByCategory  map[string]int64
	TopSources  []AdminSourceStat
	Last24h     int64
	Last7d      int64
	DisabledSrc int64
	// OrphanArticles 是**不属于任何现存源**的文章数（源被删后残留）。
	// 不加这个数，前端那张"按源统计"表格的合计就会小于概览里的文章总数，
	// 用户会以为是接口算错了 —— 实际是口径不同。把它显式给出来，误会就没了。
	OrphanArticles int64
}

// AdminContentStats 统计文章分布：按分类、按源（Top 10）、近 24 小时 / 7 天增量。
//
// Last24h / Last7d 用 published_at 而不是 created_at：运维想知道的是
// "新闻本身有多新"，不是"我们的采集器什么时候抓到的"。
// 一条昨天发布、今天补抓的新闻，在内容视角下就该算进 24 小时内。
func (r *ArticleRepo) AdminContentStats(ctx context.Context, now int64) (AdminContentStats, error) {
	st := AdminContentStats{ByCategory: map[string]int64{}}

	cats, err := r.CountByCategory(ctx)
	if err != nil {
		return st, fmt.Errorf("统计分类分布失败: %w", err)
	}
	st.ByCategory = cats

	if err := r.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM article WHERE published_at >= ?),
			(SELECT COUNT(*) FROM article WHERE published_at >= ?),
			(SELECT COUNT(*) FROM source WHERE enabled = 0),
			(SELECT COUNT(*) FROM article WHERE source_id IS NULL
				OR source_id NOT IN (SELECT id FROM source))
	`, now-86400, now-7*86400).Scan(&st.Last24h, &st.Last7d, &st.DisabledSrc, &st.OrphanArticles); err != nil {
		return st, fmt.Errorf("统计内容增量失败: %w", err)
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT s.id, s.name, s.enabled, COUNT(a.id), IFNULL(MAX(a.last_seen_at),0)
		FROM source s LEFT JOIN article a ON a.source_id = s.id
		GROUP BY s.id, s.name, s.enabled
		ORDER BY COUNT(a.id) DESC LIMIT 10`)
	if err != nil {
		return st, fmt.Errorf("统计源分布失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s AdminSourceStat
		if err := rows.Scan(&s.SourceID, &s.SourceName, &s.Enabled, &s.ArticleCount, &s.LastSeenAt); err != nil {
			return st, fmt.Errorf("扫描源分布失败: %w", err)
		}
		st.TopSources = append(st.TopSources, s)
	}
	return st, rows.Err()
}

// AdminSystemStats 是 /admin/system 要看的运行环境事实。
type AdminSystemStats struct {
	SchemaVersion int
	PageSize      int64
	PageCount     int64
	FreePages     int64
	DBBytes       int64
	JournalMode   string
	TableCounts   map[string]int64
}

// LoadSystemStats 读取数据库的客观状态。
//
// 刻意做成包级函数而不是某个 repo 的方法：这份信息描述的是**数据库整体**，
// 挂到 article / user 任何一个 repo 上都会让人误以为它跟那张表有关。
//
// DBBytes 用 page_size * page_count 而不是 os.Stat 文件大小：
// SQLite 的文件长度包含从未归还的空闲页，会严重虚高
// （删掉一半文章后文件大小纹丝不动，但 page_count 早就降下来了）。
func LoadSystemStats(ctx context.Context, db *store.DB) (AdminSystemStats, error) {
	st := AdminSystemStats{TableCounts: map[string]int64{}}

	if err := db.QueryRowContext(ctx,
		"SELECT CAST(IFNULL(value,'0') AS INTEGER) FROM schema_meta WHERE key='schema_version'").
		Scan(&st.SchemaVersion); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, fmt.Errorf("读取 schema 版本失败: %w", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&st.PageSize); err != nil {
		return st, fmt.Errorf("读取 page_size 失败: %w", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&st.PageCount); err != nil {
		return st, fmt.Errorf("读取 page_count 失败: %w", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&st.FreePages); err != nil {
		return st, fmt.Errorf("读取 freelist_count 失败: %w", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&st.JournalMode); err != nil {
		return st, fmt.Errorf("读取 journal_mode 失败: %w", err)
	}
	st.DBBytes = st.PageSize * st.PageCount

	// 每张表的行数：运维核对"清理到底跑没跑"的唯一依据。
	tables := []string{"article", "source", "audio_task", "audio", "user", "session",
		"user_favorite", "user_read", "user_preference"}
	for _, t := range tables {
		var n int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+t).Scan(&n); err != nil {
			// 表不存在（比如 FTS 被跳过）跳过即可，不值得让整个系统页 500。
			continue
		}
		st.TableCounts[t] = n
	}
	return st, nil
}

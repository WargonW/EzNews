package repo

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/store"
	"github.com/eznews/eznews/internal/util"
)

// fixture 是 repo 包全部单测共享的环境。
//
// ★ 所有 repo 必须挂在**同一个库**上：user_favorite / user_read 的 article_id 与
// user_id 都有外键，而foreign_keys(1) 在 DSN 里对每条连接都开着。
// 若repo 与种子数据分属两个库，测试测到的就不是生产代码的真实行为。
type fixture struct {
	t   *testing.T
	ctx context.Context
	db  *store.DB

	srcs  *SourceRepo
	arts  *ArticleRepo
	users *UserRepo
	state *UserStateRepo
	audio *AudioRepo

	user   int64  // 真实落库的用户（外键需要）
	source int64  // 默认启用的源
	path   string // SQLite 文件路径（孤儿文章需要旁路连接）
}

// newFixture 构造跑在临时 SQLite 上的完整环境（含真实迁移 + FTS5）。
//
// 刻意走真实 DB + 真实迁移而不是 sqlmock：这批用例的核心断言是
// 「唯一索引 / ON CONFLICT /墓碑 / 分页 tiebreaker / LEFT JOIN」的真实 SQL 语义，
// mock 里手写 SQL 只会把「实现和断言一起错」这件事藏起来。
func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWithFTS(t, true)
}

// newFixtureWithFTS 允许关闭 FTS5，用于验证 FTS 不可用时的 LIKE 降级分支。
func newFixtureWithFTS(t *testing.T, fts bool) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "repo.db")
	db, err := store.Open(store.Options{Path: path})
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db, fts); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}
	ctx := context.Background()
	f := &fixture{
		t:     t,
		ctx:   ctx,
		db:    db,
		path:  path,
		srcs:  NewSourceRepo(db),
		arts:  NewArticleRepo(db),
		users: NewUserRepo(db),
		state: NewUserStateRepo(db),
		audio: NewAudioRepo(db),
	}
	// 004_seed 会写入 16 个系统默认源；断言「源总数」时必须把它们算进去，
	// 所以下面所有涉及总数的断言都只统计本fixture 自己造的源。
	f.source = f.mustSource("repo-src-a", "tech")
	f.user = f.mustUser("repo-user")
	return f
}

// countActive 是 CountActive 的测试包装（失败直接 Fatal）。
func (f *fixture) countActive(tbl stateTable, userID int64) int {
	f.t.Helper()
	n, err := f.state.CountActive(f.ctx, f.db, tbl, userID)
	if err != nil {
		f.t.Fatalf("CountActive 失败: %v", err)
	}
	return n
}

// dbPath 返回 SQLite 文件路径。
func (f *fixture) dbPath() string { return f.path }

// mustSource 造一个启用的源并返回其 ID。
func (f *fixture) mustSource(key, category string) int64 {
	f.t.Helper()
	now := util.NowMs()
	id, err := f.srcs.Create(f.ctx, &model.Source{
		Key: key, Name: key, URL: "https://" + key + ".example.com/feed.xml",
		Type: model.SourceTypeRSS, Category: category, Enabled: true,
		SuggestInterval: 1800, Language: "zh-CN", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		f.t.Fatalf("创建源 %s 失败: %v", key, err)
	}
	return id
}

// mustDisabledSource 造一个停用的源（用于验证 enabled 过滤）。
func (f *fixture) mustDisabledSource(key, category string) int64 {
	f.t.Helper()
	now := util.NowMs()
	id, err := f.srcs.Create(f.ctx, &model.Source{
		Key: key, Name: key, URL: "https://" + key + ".example.com/feed.xml",
		Type: model.SourceTypeRSS, Category: category, Enabled: false,
		SuggestInterval: 1800, Language: "zh-CN", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		f.t.Fatalf("创建停用源 %s 失败: %v", key, err)
	}
	return id
}

// mustUser 造一个真实用户。
//
// user_favorite.user_id / user_read.user_id 有外键指向 user(id)，
// 拿一个不存在的 userID 去写会被外键直接拒绝。
func (f *fixture) mustUser(name string) int64 {
	f.t.Helper()
	now := util.NowMs()
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		f.t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	id, err := f.users.Create(f.ctx, tx, &model.User{
		Username: name, PasswordHash: "x", Role: "user", TokenVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		f.t.Fatalf("创建用户 %s 失败: %v", name, err)
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatalf("提交用户 %s 失败: %v", name, err)
	}
	return id
}

// articleOpts 是 article() 的可调字段（零值即默认值）。
type articleOpts struct {
	SourceID    int64
	URL         string // 空则按 n 自动生成
	Title       string
	Summary     string
	Content     string
	Category    string
	Tags        []string
	PublishedAt int64
	CreatedAt   int64
	UpdatedAt   int64
	LastSeenAt  int64
	ExternalID  string
	Language    string
	// ContentHash 非空时直接用作指纹（用于构造「指纹不变/变化」的 UpdateIfChanged 场景）。
	// 留空则按标题+摘要+正文+分类+发布时间算出标准指纹。
	ContentHash string
}

// article 造一篇真实文章，返回其 ID。
//
// ★ url_norm / url_hash 必须自己算：ArticleRepo.Insert 不做归一化，
// 留空会让所有测试行撞上 ux_article url_hash 唯一约束。
func (f *fixture) article(n int, opt articleOpts) int64 {
	f.t.Helper()
	tag := strconv.Itoa(n)
	if opt.URL == "" {
		opt.URL = "https://repo.example.com/a/" + tag
	}
	if opt.Title == "" {
		opt.Title = "仓库测试文 " + tag
	}
	if opt.Category == "" {
		opt.Category = "tech"
	}
	if opt.SourceID == 0 {
		opt.SourceID = f.source
	}
	if opt.PublishedAt == 0 {
		opt.PublishedAt = 1_700_000_000
	}
	if opt.CreatedAt == 0 {
		opt.CreatedAt = opt.PublishedAt * 1000
	}
	if opt.UpdatedAt == 0 {
		opt.UpdatedAt = opt.CreatedAt
	}
	if opt.LastSeenAt == 0 {
		opt.LastSeenAt = opt.UpdatedAt
	}
	if opt.Language == "" {
		opt.Language = "zh-CN"
	}
	contentHash := opt.ContentHash
	if contentHash == "" {
		contentHash = util.ContentHashV1(util.CanonicalContent(
			opt.Title, opt.Summary, opt.Content, "", "", opt.Category, opt.PublishedAt))
	}
	urlNorm, err := util.NormalizeURL(opt.URL, nil)
	if err != nil {
		f.t.Fatalf("归一化 URL %s 失败: %v", opt.URL, err)
	}
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		f.t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	id, err := f.arts.Insert(f.ctx, tx, &model.Article{
		SourceID:    opt.SourceID,
		ExternalID:  opt.ExternalID,
		URL:         opt.URL,
		URLNorm:     urlNorm,
		URLHash:     util.URLHash(urlNorm),
		Title:       opt.Title,
		Summary:     opt.Summary,
		Content:     opt.Content,
		Category:    opt.Category,
		Tags:        opt.Tags,
		Language:    opt.Language,
		ContentHash: contentHash,
		PublishedAt: opt.PublishedAt,
		CreatedAt:   opt.CreatedAt,
		UpdatedAt:   opt.UpdatedAt,
		LastSeenAt:  opt.LastSeenAt,
	})
	if err != nil {
		f.t.Fatalf("创建文章 %d 失败: %v", n, err)
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatalf("提交文章 %d 失败: %v", n, err)
	}
	return id
}

// orphanArticle 插入一篇 source_id 指向不存在行的文章，返回其 ID。
//
// article.source_id 有外键指向 source(id)，正常连接插不进来；
// 而「源被删但文章还在」正是 LEFT JOIN 必须兜住的场景
// （FindBriefsByIDs 的注释明确承诺这一点），所以这里用一条
// foreign_keys=0 的独立连接把它造出来。
func (f *fixture) orphanArticle(n int, title string) int64 {
	f.t.Helper()
	tag := strconv.Itoa(n)
	urlNorm, err := util.NormalizeURL("https://orphan.example.com/a/"+tag, nil)
	if err != nil {
		f.t.Fatalf("归一化孤儿 URL 失败: %v", err)
	}
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.dbPath())+"?_pragma=foreign_keys(0)")
	if err != nil {
		f.t.Fatalf("打开旁路连接失败: %v", err)
	}
	defer func() { _ = raw.Close() }()
	res, err := raw.ExecContext(f.ctx,
		`INSERT INTO article (source_id, url, url_norm, url_hash, title, summary, content_hash,
			category, language, tags_json, published_at, created_at, updated_at, last_seen_at)
		 VALUES (999999,?,?,?,?,'',?,'tech','zh-CN','[]',?,?,?,?)`,
		"https://orphan.example.com/a/"+tag, urlNorm, util.URLHash(urlNorm), title,
		util.ContentHashV1(title), 1_600_000_000, 1_600_000_000_000, 1_600_000_000_000, 1_600_000_000_000)
	if err != nil {
		f.t.Fatalf("插入孤儿文章失败: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		f.t.Fatalf("读取孤儿文章主键失败: %v", err)
	}
	return id
}

// rows 数某张表的行数（测试用直查，绕过 repo 层）。
func (f *fixture) rows(table string) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		f.t.Fatalf("统计 %s 失败: %v", table, err)
	}
	return n
}

// scalarI64 直查单列 int64（测试用）。
func (f *fixture) scalarI64(query string, args ...any) int64 {
	f.t.Helper()
	var n int64
	if err := f.db.QueryRowContext(f.ctx, query, args...).Scan(&n); err != nil {
		f.t.Fatalf("查询 %q 失败: %v", query, err)
	}
	return n
}

// scalarI64Tx 在传入的事务上直查单列 int64（测试用）。
//
// 与 scalarI64 的区别是能看见未提交的数据——用来验证「写入确实落在
// 传入的那个事务里」，而不是碰巧在连接池的另一条连接上生效。
func (f *fixture) scalarI64Tx(ctx context.Context, tx store.Session, query string, args ...any) int64 {
	f.t.Helper()
	var n int64
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		f.t.Fatalf("事务内查询 %q 失败: %v", query, err)
	}
	return n
}

// ids 把列表行的 ID 抽出来，便于断言顺序。
func ids(items []model.ArticleListItem) []int64 {
	out := make([]int64, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

// favIDs 把收藏列表的文章 ID 抽出来。
func favIDs(items []model.Favorite) []int64 {
	out := make([]int64, 0, len(items))
	for _, it := range items {
		out = append(out, it.ArticleID)
	}
	return out
}

// readIDs 把已读列表的文章 ID 抽出来。
func readIDs(items []model.Read) []int64 {
	out := make([]int64, 0, len(items))
	for _, it := range items {
		out = append(out, it.ArticleID)
	}
	return out
}

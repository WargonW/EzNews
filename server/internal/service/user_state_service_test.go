package service

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/repo"
	"github.com/eznews/eznews/internal/store"
	"github.com/eznews/eznews/internal/util"
)

// stateFixture 是这批用例共享的测试环境。
//
// ★ 所有东西必须挂在**同一个库**上：user_read / user_favorite 的 article_id
// 有外键指向 article(id)，而 foreign_keys(1) 是开着的。若UserStateService
// 和种子文章分属两个库，外键检查会立刻失败，测到的就不是想测的东西了。
type stateFixture struct {
	svc    *UserStateService
	state  *repo.UserStateRepo
	srcs   *repo.SourceRepo
	arts   *repo.ArticleRepo
	db     *store.DB
	source int64
	// user 是真实落库的用户。user_favorite.user_id 有外键指向 user(id)，
	// 且 foreign_keys(1) 开着 —— 拿一个不存在的 userID 去写会直接被外键拒绝。
	user int64
}

// newStateFixture 构造跑在临时 SQLite 上的完整环境（含真实迁移）。
//
// 刻意走真实 DB 而不是 mock：这批用例的核心是「事务是否真把整个批次圈起来了」，
// 只有真落库再读回才能验证 —— mock 天然测不出部分提交。
func newStateFixture(t *testing.T) *stateFixture {
	t.Helper()
	db, err := store.Open(store.Options{Path: filepath.Join(t.TempDir(), "state.db")})
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db, false); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}
	state := repo.NewUserStateRepo(db)
	srcs := repo.NewSourceRepo(db)
	arts := repo.NewArticleRepo(db)
	users := repo.NewUserRepo(db)
	f := &stateFixture{
		svc:    NewUserStateService(state, db),
		state:  state,
		srcs:   srcs,
		arts:   arts,
		db:     db,
		source: mustCreateSource(t, srcs, "custom-state", "tech"),
	}
	f.user = f.mustCreateUser(t, users, "state-user")
	return f
}

// mustCreateUser 造一个真实用户。
func (f *stateFixture) mustCreateUser(t *testing.T, users *repo.UserRepo, name string) int64 {
	t.Helper()
	now := util.NowMs()
	tx, err := f.db.BeginWrite(context.Background())
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	id, err := users.Create(context.Background(), tx, &model.User{
		Username: name, PasswordHash: "x", Role: "user", TokenVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("创建用户 %s 失败: %v", name, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交用户 %s 失败: %v", name, err)
	}
	return id
}

// article 造一篇真实文章，返回其 ID。
//
// url_norm / url_hash 必须自己算：ArticleRepo.Insert 不做归一化，
// 留空会让所有测试行撞上ux_article url_hash 唯一约束。
func (f *stateFixture) article(t *testing.T, n int) int64 {
	t.Helper()
	now := util.NowMs()
	tag := strconv.Itoa(n)
	raw := "https://example.com/s" + tag
	urlNorm, err := util.NormalizeURL(raw, nil)
	if err != nil {
		t.Fatalf("归一化 URL %s 失败: %v", raw, err)
	}
	tx, err := f.db.BeginWrite(context.Background())
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	id, err := f.arts.Insert(context.Background(), tx, &model.Article{
		SourceID:    f.source,
		ExternalID:  "ext-state-" + tag,
		URL:         raw,
		URLNorm:     urlNorm,
		URLHash:     util.URLHash(urlNorm),
		Title:       "状态测试文 " + tag,
		Summary:     "摘要 " + tag,
		Content:     "正文 " + tag,
		Category:    "tech",
		PublishedAt: now / 1000,
		CreatedAt:   now,
		UpdatedAt:   now,
		ContentHash: "hash-state-" + tag,
	})
	if err != nil {
		t.Fatalf("创建文章 %d 失败: %v", n, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交文章 %d 失败: %v", n, err)
	}
	return id
}

// rows 数某张表的行数（测试用直查，绕过 service 层）。
func (f *stateFixture) rows(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := f.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("统计 %s 失败: %v", table, err)
	}
	return n
}

// TestBatchSet_全部成功_应写入并计数
func TestBatchSet_全部成功_应写入并计数(t *testing.T) {
	f := newStateFixture(t)
	ids := []int64{f.article(t, 1), f.article(t, 2), f.article(t, 3)}
	fav := true

	res, err := f.svc.BatchSet(context.Background(), f.user, BatchSetStateRequest{
		ArticleIDs: ids, Read: true, Favorite: &fav,
	})
	if err != nil {
		t.Fatalf("批量设置失败: %v", err)
	}
	if res.ReadsChanged != 3 || res.FavoritesChanged != 3 {
		t.Fatalf("计数不对: reads=%d favs=%d，期望都是 3", res.ReadsChanged, res.FavoritesChanged)
	}
	if got := f.rows(t, "user_read"); got != 3 {
		t.Fatalf("user_read 应有 3 行，实际 %d", got)
	}
	if got := f.rows(t, "user_favorite"); got != 3 {
		t.Fatalf("user_favorite 应有 3 行，实际 %d", got)
	}
}

// TestBatchSet_★中途失败_不得留下部分提交
//
// 本次修复的核心断言。修复前是逐条 autocommit，第 2 条因外键失败时
// 第 1 条已经提交且无法回滚 —— 用户看到的是「部分生效」的中间态，
// 重试又会把已生效的条目重复处理。修复后整个批次在一个写事务里，
// 任何一条失败都全批回滚。
func TestBatchSet_中途失败_不得留下部分提交(t *testing.T) {
	f := newStateFixture(t)
	good1 := f.article(t, 11)
	good2 := f.article(t, 12)
	// 999999 不存在 → 外键约束在中途拒绝这条，模拟真实故障。
	const missing = int64(999999)

	_, err := f.svc.BatchSet(context.Background(), f.user, BatchSetStateRequest{
		ArticleIDs: []int64{good1, missing, good2}, Read: true,
	})
	if err == nil {
		t.Fatal("含不存在的 articleID 时必须返回错误，实际却成功了")
	}
	if got := f.rows(t, "user_read"); got != 0 {
		t.Fatalf("失败批次必须全批回滚：user_read 仍有 %d 行（期望 0）——说明是逐条 autocommit，修复无效", got)
	}
	if got := f.rows(t, "user_favorite"); got != 0 {
		t.Fatalf("失败批次必须全批回滚：user_favorite 仍有 %d 行（期望 0）", got)
	}
}

// TestBatchSet_重放幂等_不产生重复行
//
// 增量同步客户端会重放整个批次，唯一索引 + ON CONFLICT DO UPDATE 必须兜住。
func TestBatchSet_重放幂等_不产生重复行(t *testing.T) {
	f := newStateFixture(t)
	ids := []int64{f.article(t, 21), f.article(t, 22)}
	fav := true
	req := BatchSetStateRequest{ArticleIDs: ids, Read: true, Favorite: &fav}

	for i := 0; i < 3; i++ {
		if _, err := f.svc.BatchSet(context.Background(), f.user, req); err != nil {
			t.Fatalf("第 %d 次重放失败: %v", i+1, err)
		}
	}
	if got := f.rows(t, "user_read"); got != 2 {
		t.Fatalf("重放 3 次后 user_read 应仍是 2 行，实际 %d（唯一索引失效）", got)
	}
	if got := f.rows(t, "user_favorite"); got != 2 {
		t.Fatalf("重放 3 次后 user_favorite 应仍是 2 行，实际 %d", got)
	}
}

// TestBatchSet_取消收藏_写入墓碑而非删行
//
// 墓碑是这个服务的硬不变量（见 UserStateService 顶部注释）：
// 增量同步必须能收到「这条要删」的信号，物理删行会让客户端留下幽灵收藏且再也删不掉。
func TestBatchSet_取消收藏_写入墓碑而非删行(t *testing.T) {
	f := newStateFixture(t)
	id := f.article(t, 31)
	ctx := context.Background()

	yes, no := true, false
	if _, err := f.svc.BatchSet(ctx, 1, BatchSetStateRequest{ArticleIDs: []int64{id}, Favorite: &yes}); err != nil {
		t.Fatalf("收藏失败: %v", err)
	}
	if _, err := f.svc.BatchSet(ctx, 1, BatchSetStateRequest{ArticleIDs: []int64{id}, Favorite: &no}); err != nil {
		t.Fatalf("取消收藏失败: %v", err)
	}
	if got := f.rows(t, "user_favorite"); got != 1 {
		t.Fatalf("取消收藏后应仍有 1 行（墓碑），实际 %d —— 若为 0 说明物理删了行", got)
	}
	var deletedAt *int64
	if err := f.db.QueryRowContext(context.Background(),
		"SELECT deleted_at FROM user_favorite WHERE article_id=?", id).Scan(&deletedAt); err != nil {
		t.Fatalf("读取墓碑失败: %v", err)
	}
	if deletedAt == nil {
		t.Fatal("deleted_at 应非 NULL（墓碑），实际为 NULL")
	}
}

// TestBatchSet_空与超限_不触库
func TestBatchSet_空与超限_不触库(t *testing.T) {
	f := newStateFixture(t)
	ctx := context.Background()

	if res, err := f.svc.BatchSet(ctx, 1, BatchSetStateRequest{}); err != nil || res.ReadsChanged != 0 {
		t.Fatalf("空批次应直接返回零结果，实际 res=%+v err=%v", res, err)
	}
	big := make([]int64, 501)
	if _, err := f.svc.BatchSet(ctx, 1, BatchSetStateRequest{ArticleIDs: big}); err == nil {
		t.Fatal("超过 500 条应被拒绝")
	}
	// 全是非正数：去重后为空，同样不应触库。
	junk := []int64{0, -1, -5}
	if _, err := f.svc.BatchSet(ctx, 1, BatchSetStateRequest{ArticleIDs: junk}); err != nil {
		t.Fatalf("全非法ID 应被静默丢弃而非报错: %v", err)
	}
	if got := f.rows(t, "user_read"); got != 0 {
		t.Fatalf("这些批次不应留下任何行，实际 %d", got)
	}
}

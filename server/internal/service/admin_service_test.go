package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/repo"
	"github.com/eznews/eznews/internal/store"
)

// adminFixture 是 AdminService 单测的共享环境。
//
// 走真实 SQLite（不用 mock）：这批用例断言的全是 SQL 语义相关的东西 ——
// role 过滤、disabled_at 的 NULL 语义、级联删除、COUNT 聚合。
// 换成 mock 之后，"SQL 写错但断言也跟着错"这种情况就再也测不出来了。
type adminFixture struct {
	t      *testing.T
	ctx    context.Context
	db     *store.DB
	svc    *AdminService
	users  *repo.UserRepo
	alice  int64
	bob    int64
	carol  int64
	source int64
}

// newAdminFixture 构造含三个用户的环境：alice(admin) / bob(user) / carol(admin)。
//
// 刻意让管理员有**两个**：绝大多数守卫用例都需要"还有第二个可用管理员"这个前提，
// 单独造管理员会让每个用例都得先绕一圈把人加上，反而掩盖意图。
func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin.db")
	db, err := store.Open(store.Options{Path: path})
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := store.Migrate(ctx, db, true); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}

	users := repo.NewUserRepo(db)
	f := &adminFixture{
		t: t, ctx: ctx, db: db, users: users,
		svc: NewAdminService(db, users, repo.NewArticleRepo(db),
			repo.NewAudioRepo(db), nil, nil, "test"),
	}
	f.source = f.newSource()
	f.alice = f.newUser("alice", model.RoleAdmin)
	f.bob = f.newUser("bob", model.RoleUser)
	f.carol = f.newUser("carol", model.RoleAdmin)
	return f
}

func (f *adminFixture) newUser(name, role string) int64 {
	f.t.Helper()
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		f.t.Fatalf("开启事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	id, err := f.users.Create(f.ctx, tx, &model.User{
		Username: name, PasswordHash: "not-a-real-hash", Role: role,
		TokenVersion: 1, CreatedAt: 1700000000000, UpdatedAt: 1700000000000,
	})
	if err != nil {
		f.t.Fatalf("创建用户 %s 失败: %v", name, err)
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatalf("提交失败: %v", err)
	}
	return id
}

func (f *adminFixture) newSource() int64 {
	f.t.Helper()
	srcs := repo.NewSourceRepo(f.db)
	id, err := srcs.Create(f.ctx, &model.Source{
		Key: "test-src", Name: "测试源", URL: "https://example.com/feed.xml",
		Type: model.SourceTypeRSS, Category: "tech", Enabled: true,
		SuggestInterval: 1800, Language: "zh-CN",
		CreatedAt: 1700000000000, UpdatedAt: 1700000000000,
	})
	if err != nil {
		f.t.Fatalf("创建源失败: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// ★ 以下三个用例是同一个 class of bug 的回归网。
//
// 我最初把"目标是不是管理员"的判断留在调用方，三个调用点漏了两处，
// 结果表现为：**系统只剩一个管理员时，连普通用户都停用不了**。
// 这种 bug 不会崩、不会报错，只会让管理员看到一个驴唇不对马嘴的"最后一个管理员"提示。
// 所以修复之后必须把"停用一个普通人"写成用例钉住。
// ---------------------------------------------------------------------------

func TestAdmin停用普通用户_不受最后管理员限制(t *testing.T) {
	f := newAdminFixture(t)
	// 先把 carol 停掉，系统此刻只剩 alice 一个可用管理员。
	if err := disable(f, f.carol, true); err != nil {
		t.Fatalf("停用 carol 失败: %v", err)
	}
	admins, err := f.users.AdminCountAdmins(f.ctx)
	if err != nil {
		t.Fatalf("统计管理员失败: %v", err)
	}
	if admins != 1 {
		t.Fatalf("前置条件不成立：期望只剩 1 个可用管理员，实际 %d", admins)
	}

	// 此时停用一个**普通用户** bob，必须成功。
	if _, err := f.svc.SetDisabled(f.ctx, f.alice, f.bob, true); err != nil {
		t.Fatalf("只剩一个管理员时停用普通用户失败: %v —— 守卫把非管理员也算进管理员存量了", err)
	}

	got, err := f.users.AdminGetUser(f.ctx, f.bob, nowMs())
	if err != nil {
		t.Fatalf("读取 bob 失败: %v", err)
	}
	if !got.Disabled {
		t.Fatalf("bob 应已停用")
	}
}

func TestAdmin删除普通用户_不受最后管理员限制(t *testing.T) {
	f := newAdminFixture(t)
	if err := deleteUser(f, f.carol); err != nil {
		t.Fatalf("删除 carol 失败: %v", err)
	}
	admins, _ := f.users.AdminCountAdmins(f.ctx)
	if admins != 1 {
		t.Fatalf("前置条件不成立：期望 1 个可用管理员，实际 %d", admins)
	}
	if err := f.svc.DeleteUser(f.ctx, f.alice, f.bob); err != nil {
		t.Fatalf("只剩一个管理员时删除普通用户失败: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 最后一个管理员守卫：只有真正的最后一个可用管理员才被保护。
// ---------------------------------------------------------------------------

func TestAdmin最后一个管理员_不能被降级停用删除(t *testing.T) {
	f := newAdminFixture(t)
	// carol 停用后，alice 成为唯一的可用管理员。
	if err := disable(f, f.carol, true); err != nil {
		t.Fatalf("停用 carol 失败: %v", err)
	}

	if _, err := f.svc.SetRole(f.ctx, f.carol, f.alice, model.RoleUser); err == nil {
		t.Fatal("最后一个可用管理员被降级成功，系统已无法进入后台")
	}
	if _, err := f.svc.SetDisabled(f.ctx, f.carol, f.alice, true); err == nil {
		t.Fatal("最后一个可用管理员被停用成功")
	}
	if err := f.svc.DeleteUser(f.ctx, f.carol, f.alice); err == nil {
		t.Fatal("最后一个可用管理员被删除成功")
	}
}

func TestAdmin停用的管理员_不占用管理员名额(t *testing.T) {
	f := newAdminFixture(t)
	// alice,carol 都是管理员。carol 停用后 alice 是最后一个。
	if err := disable(f, f.carol, true); err != nil {
		t.Fatalf("停用 carol 失败: %v", err)
	}
	// 此时 alice 降级自己必须被拒（她是最后一个**可用**的）。
	if _, err := f.svc.SetRole(f.ctx, f.alice, f.alice, model.RoleUser); err == nil {
		t.Fatal("管理员自降成功")
	}
	// 报表面上还有 2 个 role=admin 的人，但其中一个是停用的。
	st, err := f.svc.GetAccountState(f.ctx, f.carol)
	if err != nil {
		t.Fatalf("查询状态失败: %v", err)
	}
	if !st.IsAdmin || !st.Disabled {
		t.Fatalf("carol 状态错误: isAdmin=%v disabled=%v", st.IsAdmin, st.Disabled)
	}
	if ok, _ := f.svc.IsAdmin(f.ctx, f.carol); ok {
		t.Fatal("停用的管理员不应被判定为可用管理员")
	}
}

func TestAdmin有第二个管理员时可降级(t *testing.T) {
	f := newAdminFixture(t) // alice + carol 两个可用管理员
	got, err := f.svc.SetRole(f.ctx, f.carol, f.alice, model.RoleUser)
	if err != nil {
		t.Fatalf("存在第二个管理员时降级失败: %v", err)
	}
	if got.Role != model.RoleUser {
		t.Fatalf("角色未生效: %s", got.Role)
	}
}

// ---------------------------------------------------------------------------
// 自我保护：管理员不许对自己下狠手。
// ---------------------------------------------------------------------------

func TestAdmin不能对自己执行破坏性操作(t *testing.T) {
	f := newAdminFixture(t)
	if _, err := f.svc.SetRole(f.ctx, f.alice, f.alice, model.RoleUser); err == nil {
		t.Fatal("管理员把自己降级成功了")
	}
	if _, err := f.svc.SetDisabled(f.ctx, f.alice, f.alice, true); err == nil {
		t.Fatal("管理员把自己停用成功了")
	}
	if err := f.svc.DeleteUser(f.ctx, f.alice, f.alice); err == nil {
		t.Fatal("管理员把自己删除成功了")
	}
	if _, err := f.svc.RevokeAllSessions(f.ctx, f.alice, f.alice); err == nil {
		t.Fatal("管理员吊销了自己的全部会话")
	}
}

// ---------------------------------------------------------------------------
// 目标不存在：UPDATE 影响 0 行却返回 success 是这类接口最经典的静默失败。
// ---------------------------------------------------------------------------

func TestAdmin操作不存在的用户_返回NotFound而非成功(t *testing.T) {
	f := newAdminFixture(t)
	const missing = int64(999999)

	if _, err := f.svc.GetUser(f.ctx, f.alice, missing); err == nil {
		t.Fatal("查询不存在的用户返回了成功")
	} else if statusOf(err) != 404 {
		t.Fatalf("期望 404，实际 %d", statusOf(err))
	}
	if _, err := f.svc.SetRole(f.ctx, f.alice, missing, model.RoleUser); statusOf(err) != 404 {
		t.Fatalf("改角色期望 404，实际 %v", statusOf(err))
	}
	if _, err := f.svc.SetDisabled(f.ctx, f.alice, missing, true); statusOf(err) != 404 {
		t.Fatalf("停用期望 404，实际 %v", statusOf(err))
	}
	if err := f.svc.DeleteUser(f.ctx, f.alice, missing); statusOf(err) != 404 {
		t.Fatalf("删除期望 404，实际 %v", statusOf(err))
	}
	if _, err := f.svc.RevokeAllSessions(f.ctx, f.alice, missing); statusOf(err) != 404 {
		t.Fatalf("吊销期望 404，实际 %v", statusOf(err))
	}
}

// ---------------------------------------------------------------------------
// 能力位：前端据此置灰按钮，算错不影响安全但会让 UI 说谎。
// ---------------------------------------------------------------------------

func TestAdmin能力位_本人与普通用户的取值(t *testing.T) {
	f := newAdminFixture(t)
	list, err := f.svc.ListUsers(f.ctx, f.alice, "", "", 10, 0)
	if err != nil {
		t.Fatalf("列出用户失败: %v", err)
	}
	byID := map[int64]AdminUserDTO{}
	for _, u := range list.Items {
		byID[u.ID] = u
	}

	self, ok := byID[f.alice]
	if !ok {
		t.Fatalf("列表中缺少 alice")
	}
	if !self.Self || self.CanChangeRole || self.CanDisable || self.CanDelete || self.CanRevokeAll {
		t.Fatalf("自己的行能力位应全 false（除 self）: %+v", self)
	}

	normal, ok := byID[f.bob]
	if !ok {
		t.Fatalf("列表中缺少 bob")
	}
	if normal.Self || !normal.CanChangeRole || !normal.CanDisable || !normal.CanDelete || !normal.CanRevokeAll {
		t.Fatalf("普通用户的行应全部可操作: %+v", normal)
	}
}

func TestAdmin能力位_最后一个管理员为false(t *testing.T) {
	f := newAdminFixture(t)
	if err := disable(f, f.carol, true); err != nil {
		t.Fatalf("停用 carol 失败: %v", err)
	}
	got, err := f.svc.GetUser(f.ctx, f.alice, f.carol)
	if err != nil {
		t.Fatalf("查询 carol 失败: %v", err)
	}
	// carol 虽是 role=admin，但**已停用** → 不计入"可用管理员"，
	// 删掉她不会让系统失去管理员，所以她必须仍然可以被操作。
	//
	// ★ 反过来写（凡是 role=admin 一律保护）才是事故：
	//   停用一个管理员之后就再也没人能动她，连把她恢复回来都做不到。
	if !got.CanDisable || !got.CanDelete {
		t.Fatalf("已停用的管理员应仍可操作（她不占可用管理员名额）: %+v", got)
	}

	aliceRow, err := f.svc.GetUser(f.ctx, f.carol, f.alice)
	if err != nil {
		t.Fatalf("查询 alice 失败: %v", err)
	}
	if aliceRow.CanChangeRole || aliceRow.CanDisable || aliceRow.CanDelete {
		t.Fatalf("最后一个可用管理员的能力位应全 false: %+v", aliceRow)
	}
}

// ---------------------------------------------------------------------------
// 停用必须连带吊销会话：否则"已停用"的用户在 token 有效期内仍能继续用。
// ---------------------------------------------------------------------------

func TestAdmin停用_连带吊销全部会话(t *testing.T) {
	f := newAdminFixture(t)
	// bob 先有一个活跃会话。
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	sid, err := f.users.InsertSession(f.ctx, tx, &model.Session{
		UserID: f.bob, RefreshTokenHash: "hash-bob", ExpiresAt: nowMs() + 7200_000,
		CreatedAt: nowMs(), LastSeenAt: nowMs(),
	})
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	active, err := f.users.IsSessionActive(f.ctx, sid, f.bob)
	if err != nil || !active {
		t.Fatalf("前置条件不成立：会话应处于活跃状态")
	}

	if _, err := f.svc.SetDisabled(f.ctx, f.alice, f.bob, true); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if active, _ := f.users.IsSessionActive(f.ctx, sid, f.bob); active {
		t.Fatal("停用之后会话仍然活跃 —— 被停用者在 token 有效期内还能继续操作")
	}
}

func TestAdmin启用_不动会话(t *testing.T) {
	f := newAdminFixture(t)
	if _, err := f.svc.SetDisabled(f.ctx, f.alice, f.bob, true); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	sid, err := f.users.InsertSession(f.ctx, tx, &model.Session{
		UserID: f.bob, RefreshTokenHash: "h2", ExpiresAt: nowMs() + 7200_000,
		CreatedAt: nowMs(), LastSeenAt: nowMs(),
	})
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	if _, err := f.svc.SetDisabled(f.ctx, f.alice, f.bob, false); err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	// 启用操作刻意不碰会话：把一个准备恢复使用的用户的现有会话踢掉毫无收益。
	if active, _ := f.users.IsSessionActive(f.ctx, sid, f.bob); !active {
		t.Fatal("启用操作不应吊销会话")
	}
}

// ---------------------------------------------------------------------------
// 幂等：前端二次确认弹窗重复提交时不应报错。
// ---------------------------------------------------------------------------

func TestAdmin重复提交相同值_幂等不报错(t *testing.T) {
	f := newAdminFixture(t)
	if _, err := f.svc.SetDisabled(f.ctx, f.alice, f.bob, true); err != nil {
		t.Fatalf("首次停用失败: %v", err)
	}
	if _, err := f.svc.SetDisabled(f.ctx, f.alice, f.bob, true); err != nil {
		t.Fatalf("重复停用报错了，前端重发会看到红条: %v", err)
	}
	if _, err := f.svc.SetRole(f.ctx, f.carol, f.bob, model.RoleUser); err != nil {
		t.Fatalf("设置已相同的角色报错: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 参数校验：非法值必须 400，而不是静默返回空列表。
// ---------------------------------------------------------------------------

func TestAdmin列表参数非法_返回400(t *testing.T) {
	f := newAdminFixture(t)
	if _, err := f.svc.ListUsers(f.ctx, f.alice, "", "superadmin", 10, 0); statusOf(err) != 400 {
		t.Fatalf("非法 role 期望 400，实际 %v", statusOf(err))
	}
	if _, err := f.svc.AudioTasks(f.ctx, "bogus", 10, 0); statusOf(err) != 400 {
		t.Fatalf("非法 status 期望 400，实际 %v", statusOf(err))
	}
	for _, ok := range []string{"pending", "processing", "ready", "failed"} {
		if _, err := f.svc.AudioTasks(f.ctx, ok, 10, 0); err != nil {
			t.Fatalf("合法 status %q 被拒: %v", ok, err)
		}
	}
}

func TestAdmin搜索_通配符被转义(t *testing.T) {
	f := newAdminFixture(t)
	// 用户名里带 _ 和 % 的用户：若 LIKE 未转义，"a_b" 会匹配到 "axb"。
	wild := f.newUser("a_b%c", model.RoleUser)
	got, err := f.svc.ListUsers(f.ctx, f.alice, "a_b%c", "", 10, 0)
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	found := false
	for _, u := range got.Items {
		if u.ID == wild {
			found = true
		}
	}
	if !found {
		t.Fatal("精确匹配带通配符的用户名失败 —— LIKE 未转义")
	}
	// "%" 单独搜索在本实现中是"包含任意字符"，不该等同于列出全部。
	if got.Total != 1 {
		t.Fatalf("搜索结果数应为 1，实际 %d", got.Total)
	}
}

func TestAdmin分页_cursor与total互不干扰(t *testing.T) {
	f := newAdminFixture(t)
	page1, err := f.svc.ListUsers(f.ctx, f.alice, "", "", 2, 0)
	if err != nil {
		t.Fatalf("第一页失败: %v", err)
	}
	if len(page1.Items) != 2 || !page1.HasMore {
		t.Fatalf("第一页异常: len=%d hasMore=%v", len(page1.Items), page1.HasMore)
	}
	total := page1.Total

	page2, err := f.svc.ListUsers(f.ctx, f.alice, "", "", 2, 2)
	if err != nil {
		t.Fatalf("第二页失败: %v", err)
	}
	// ★ 总数必须恒定。它若随翻页缩水，前端据此算的页数会跟着変 causing 抖动。
	if page2.Total != total {
		t.Fatalf("第二页总数与第一页不一致: %d != %d —— COUNT 查询带上了游标", page2.Total, total)
	}
	if len(page2.Items) != 1 || page2.HasMore {
		t.Fatalf("第二页异常: len=%d hasMore=%v", len(page2.Items), page2.HasMore)
	}
	// 两页不能有重复用户。
	seen := map[int64]bool{}
	for _, u := range page1.Items {
		seen[u.ID] = true
	}
	for _, u := range page2.Items {
		if seen[u.ID] {
			t.Fatalf("用户 %d 出现在两页中 —— keyset 游标有重叠", u.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// 只读端点
// ---------------------------------------------------------------------------

func TestAdmin概览与系统信息_返回可用数据(t *testing.T) {
	f := newAdminFixture(t)
	// 迁移 004_seed.sql 会预置默认新闻源，所以源数不能直接断言等于 1。
	baseSources := countSources(t, f)
	ov, err := f.svc.Overview(f.ctx)
	if err != nil {
		t.Fatalf("概览失败: %v", err)
	}
	if ov.SchemaVersion <= 0 {
		t.Fatalf("schemaVersion 未读出: %d", ov.SchemaVersion)
	}
	if ov.Users != 3 {
		t.Fatalf("用户数应为 3，实际 %d", ov.Users)
	}
	if ov.Admins != 2 {
		t.Fatalf("role=admin 的人数应为 2（含停用），实际 %d", ov.Admins)
	}
	if ov.Sources != baseSources {
		t.Fatalf("源数应为 %d，实际 %d", baseSources, ov.Sources)
	}

	sys, err := f.svc.SystemInfo(f.ctx)
	if err != nil {
		t.Fatalf("系统信息失败: %v", err)
	}
	if sys.PageSize <= 0 || sys.PageCount <= 0 {
		t.Fatalf("页信息异常: pageSize=%d pageCount=%d", sys.PageSize, sys.PageCount)
	}
	if sys.DBBytes != sys.PageSize*sys.PageCount {
		t.Fatalf("dbBytes 应等于 page_size*page_count: %d != %d",
			sys.DBBytes, sys.PageSize*sys.PageCount)
	}
	if sys.JournalMode == "" {
		t.Fatal("journalMode 为空")
	}
	if len(sys.TableCounts) == 0 {
		t.Fatal("tableCounts 为空")
	}
}

func TestAdmin内容统计_无文章时各项为零(t *testing.T) {
	f := newAdminFixture(t)
	got, err := f.svc.ContentStats(f.ctx)
	if err != nil {
		t.Fatalf("内容统计失败: %v", err)
	}
	if got.Total != 0 || got.OrphanArticles != 0 {
		t.Fatalf("无文章时统计应全为 0: total=%d orphan=%d", got.Total, got.OrphanArticles)
	}
	// TopSources 刻意 LIMIT 10（源可能有几十个，全列出来既没用又拖满一屏）。
	// 源再多也应包含我们自建的那个吗？不一定 —— 排序按文章数降序，
	// 无文章时顺序未定义。所以这里只断言"非空且不超过 10"。
	if len(got.TopSources) == 0 || len(got.TopSources) > 10 {
		t.Fatalf("TopSources 数量异常（LEFT JOIN 至少应列出源）: %d", len(got.TopSources))
	}
	if got.TopSources[0].ArticleCount != 0 {
		t.Fatalf("源文章数应为 0，实际 %d", got.TopSources[0].ArticleCount)
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// statusOf 取出 AppError 的 HTTP 状态码（非 AppError 返回 0）。
func statusOf(err error) int {
	if ae := apierr.AsAppError(err); ae != nil {
		return ae.Status
	}
	return 0
}

// disable 直接写库改变停用状态，用于构造前置条件（绕过 service 的守卫）。
func disable(f *adminFixture, id int64, v bool) error {
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := f.users.AdminSetDisabled(f.ctx, tx, id, v, nowMs()); err != nil {
		return err
	}
	return tx.Commit()
}

func deleteUser(f *adminFixture, id int64) error {
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := f.users.AdminDeleteUser(f.ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// countSources 直接查库统计源数，用于给出与实现无关的期望值。
func countSources(t *testing.T, f *adminFixture) int64 {
	t.Helper()
	var n int64
	if err := f.db.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM source").Scan(&n); err != nil {
		t.Fatalf("统计源数失败: %v", err)
	}
	return n
}

func nowMs() int64 { return 1700000000000 }

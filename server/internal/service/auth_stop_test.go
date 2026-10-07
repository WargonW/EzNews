package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/auth"
	"github.com/eznews/eznews/internal/config"
	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/repo"
	"github.com/eznews/eznews/internal/store"
	"github.com/eznews/eznews/internal/util"
)

// ---------------------------------------------------------------------------
// 这组用例只钉一件事：**被停用的账号走 refresh 时，必须回 403 而不是 401**。
//
// 背景：管理员停用账号时会在同一个事务里吊销该用户的全部会话。于是当这个用户
// 拿着 refresh token 来续期时，AuthService.Refresh 在**还没走到** rotateSession
// 的停用检查之前，就先命中了"会话已吊销"分支 —— 那条分支按"令牌泄露重放"处理，
// 返回 401 UNAUTHORIZED。
//
// 401 与 403 的区别不是措辞问题，而是客户端行为的指令：
//   - 401 → "身份有问题，去 refresh 然后重放"（可重试）
//   - 403 → "权限不足或已停用，立即跳出登录态"（不可重试）
//
// 报错码选错，被停用的客户端就会陷入"refresh 拿到 401 → 再 refresh"的静默重试，
// 直到某个超时为止。功能上"确实被拒了"，但用户体验与日志都是一团糟。
//
// 修法与停用拦截同理：先分辨这个已吊销的会话到底是因为"泄露"还是"被停用"。
// ---------------------------------------------------------------------------

type authStopFixture struct {
	t     *testing.T
	ctx   context.Context
	db    *store.DB
	svc   *AuthService
	users *repo.UserRepo
	alice int64
	bob   int64
}

func newAuthStopFixture(t *testing.T) *authStopFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth-stop.db")
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
	cfg := config.Defaults().Auth
	// JWT 密钥在 Defaults() 里刻意留空（它是部署时注入的机密，不能有默认值）。
	// 单测只需要"能签出令牌"，随便给个非空值即可。
	cfg.JWTSecret = "test-only-secret-not-used-in-production"
	jwtMgr, jerr := auth.NewJWTManager(cfg.JWTSecret, cfg.AccessTokenTTL.Std())
	if jerr != nil {
		t.Fatalf("构造 JWT 管理器失败: %v", jerr)
	}
	f := &authStopFixture{
		t: t, ctx: ctx, db: db, users: users,
		svc:   NewAuthService(db, users, jwtMgr, cfg),
		alice: insertUser(t, db, users, "alice", model.RoleAdmin),
		bob:   insertUser(t, db, users, "bob", model.RoleUser),
	}
	return f
}

func insertUser(t *testing.T, db *store.DB, users *repo.UserRepo, name, role string) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := util.NowMs()
	id, err := users.Create(ctx, tx, &model.User{
		Username: name, PasswordHash: "x", Role: role,
		TokenVersion: 1, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("创建用户 %s 失败: %v", name, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	return id
}

// mustSession 插一条会话并返回 (会话ID, 明文 refresh token)。
//
// 明文只在这里出现一次然后被丢弃 —— 库里存的是它的哈希，测试必须持着明文才能
// 模拟客户端。所以这里必须返回明文，不能返回哈希。
func (f *authStopFixture) mustSession(userID int64, clientID string) (int64, string) {
	f.t.Helper()
	raw, err := auth.NewRefreshToken()
	if err != nil {
		f.t.Fatalf("生成 refresh token 失败: %v", err)
	}
	now := util.NowMs()
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		f.t.Fatalf("开启事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	id, err := f.users.InsertSession(f.ctx, tx, &model.Session{
		UserID: userID, RefreshTokenHash: auth.HashToken(raw), ClientID: clientID,
		DeviceName: "测试设备", LastSeenAt: now,
		ExpiresAt: now + 720*3600*1000, CreatedAt: now,
	})
	if err != nil {
		f.t.Fatalf("创建会话失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatalf("提交会话失败: %v", err)
	}
	return id, raw
}

// disableAndRevoke 模拟"管理员停用账号"：同一个写事务里置 disabled_at 并吊销全部会话。
// 这正是 AdminService.SetDisabled 做的事，两步在同一事务里 —— 而正因为如此，
// refresh 才会先撞上"会话已吊销"。
func (f *authStopFixture) disableAndRevoke(userID int64) {
	f.t.Helper()
	now := util.NowMs()
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		f.t.Fatalf("开启事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := f.users.AdminSetDisabled(f.ctx, tx, userID, true, now); err != nil {
		f.t.Fatalf("停用失败: %v", err)
	}
	if _, err := f.users.AdminRevokeAllSessions(f.ctx, tx, userID, now); err != nil {
		f.t.Fatalf("吊销会话失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatalf("提交失败: %v", err)
	}
}

// 被停用后拿**当前代** refresh token 续期 → 必须 403。
func TestAuth停用后refresh_返回403而非401(t *testing.T) {
	f := newAuthStopFixture(t)
	_, raw := f.mustSession(f.bob, "dev-1")

	// 前置：停用之前这个 token 是能用的。
	if _, err := f.svc.Refresh(f.ctx, raw, "dev-1", "测试设备"); err != nil {
		t.Fatalf("前置条件不成立：停用前 refresh 应成功，实际 %v", err)
	}

	f.disableAndRevoke(f.bob)

	_, err := f.svc.Refresh(f.ctx, raw, "dev-1", "测试设备")
	if err == nil {
		t.Fatal("被停用的账号不应能 refresh 成功")
	}
	var ae *apierr.AppError
	if !errors.As(err, &ae) {
		t.Fatalf("期望 apierr.AppError，实际 %T: %v", err, err)
	}
	if ae.Status != 403 {
		t.Errorf("被停用后 refresh 应返回 403（立即跳出登录态），实际 %d —— "+
			"报 401 会让客户端以为可以重试，陷入静默重试循环", ae.Status)
	}
}

// 被停用后拿**上一代且已超宽限窗口**的 refresh token 续期，也必须 403。
//
// 停用时全部会话被吊销，而客户端手里往往还是"上一代"（例如弱网时那次请求它没收到
// 响应）。等下它恢复网络再来续期，宽限窗口早过了 —— 于是直接落到 Refresh 的
// **泄露判定**分支。若不在那里分辨"是不是被停用"，就会报 401。
//
// ★ 造"上一代"刻意走**真实的 Refresh 轮换**，而不是手工 UPDATE 列名：
//
//	我第一版手工写的是 `prev_token_hash`，而真实列名是 `prev_refresh_token_hash`，
//	UPDATE 直接报错被 t.Skip 吞掉 —— 测试绿着，却根本没测到它声称要测的东西。
func TestAuth停用后超窗的上一代refresh_返回403而非401(t *testing.T) {
	f := newAuthStopFixture(t)
	_, oldRaw := f.mustSession(f.bob, "dev-1")

	// 真实轮换一次：oldRaw 从此成为"上一代"。
	res, err := f.svc.Refresh(f.ctx, oldRaw, "dev-1", "测试设备")
	if err != nil {
		t.Fatalf("前置轮换失败: %v", err)
	}
	if res.RefreshToken == "" || res.RefreshToken == oldRaw {
		t.Fatalf("轮换后应拿到新的 refresh token")
	}

	// 把 prev_rotated_at 推到宽限窗口之外，模拟"客户端隔了很久才来续期"。
	// 手工改这一列是刻意的：真实等待 60 秒会让测试变慢且不必。
	old := util.NowMs() - 24*3600*1000
	h := auth.HashToken(oldRaw)
	tx, terr := f.db.BeginWrite(f.ctx)
	if terr != nil {
		t.Fatalf("开启事务失败: %v", terr)
	}
	r, xerr := tx.ExecContext(f.ctx,
		"UPDATE session SET prev_rotated_at=? WHERE prev_refresh_token_hash=?", old, h)
	if xerr != nil {
		_ = tx.Rollback()
		t.Fatalf("改写 prev_rotated_at 失败: %v", xerr)
	}
	if n, _ := r.RowsAffected(); n != 1 {
		_ = tx.Rollback()
		t.Fatalf("预期命中 1 行 session，实际 %d —— 说明 oldRaw 并不在 prev 代上，"+
			"这条用例会测不到目标分支", n)
	}
	if cerr := tx.Commit(); cerr != nil {
		t.Fatalf("提交失败: %v", cerr)
	}

	// 前置校验：账号仍启用时，超窗重放必须被拒（防令牌泄露）。
	if _, err := f.svc.Refresh(f.ctx, oldRaw, "dev-1", "测试设备"); err == nil {
		t.Fatalf("前置条件不成立：超窗重放应被拒")
	}

	// 停用该账号（连带吊销全部会话）。
	f.disableAndRevoke(f.bob)

	_, err = f.svc.Refresh(f.ctx, oldRaw, "dev-1", "测试设备")
	if err == nil {
		t.Fatal("被停用的账号用超窗的上一代 token 也不应能 refresh 成功")
	}
	var ae *apierr.AppError
	if !errors.As(err, &ae) {
		t.Fatalf("期望 apierr.AppError，实际 %T: %v", err, err)
	}
	if ae.Status != 403 {
		t.Errorf("被停用后超窗的上一代 refresh 应返回 403，实际 %d —— "+
			"session 已被停用逻辑吊销并落到泄露判定分支，必须在那里也分辨出停用", ae.Status)
	}
}

// 被停用后，**异设备**拿着上一代 token 来续期（超宽限窗口的泄露形态）也必须 403。
// 与上一条互补：上一条走宽限内分支，这条走超窗/异设备的泄露分支。
func TestAuth停用后泄露形态的refresh_返回403而非401(t *testing.T) {
	f := newAuthStopFixture(t)
	_, oldRaw := f.mustSession(f.bob, "dev-1")
	if _, err := f.svc.Refresh(f.ctx, oldRaw, "dev-1", "测试设备"); err != nil {
		t.Fatalf("前置轮换失败: %v", err)
	}
	f.disableAndRevoke(f.bob)

	// 换个 clientId → 即便还在宽限窗口内，也一律按泄露判定（防跨设备复制 token）。
	_, err := f.svc.Refresh(f.ctx, oldRaw, "another-device", "另一台设备")
	if err == nil {
		t.Fatal("被停用的账号用上一代 token 异设备续期不应成功")
	}
	var ae *apierr.AppError
	if !errors.As(err, &ae) {
		t.Fatalf("期望 apierr.AppError，实际 %T: %v", err, err)
	}
	if ae.Status != 403 {
		t.Errorf("异设备泄露形态下、账号已停用时应返回 403，实际 %d", ae.Status)
	}
}

// 对照组：**不是**被停用，而是纯粹的令牌泄露重放，必须仍然是 401。
//
// 没有这条对照，上面两条用例有可能被"把整个泄露分支都改成 403"这种错误的修法骗过去 ——
// 那样会破坏"401 去 refresh / 403 跳出登录"的全局约定。
func TestAuth令牌泄露重放_仍然是401(t *testing.T) {
	f := newAuthStopFixture(t)
	sid, raw := f.mustSession(f.bob, "dev-1")

	// 手工把会话标成已吊销，但账号**保持启用**。
	now := util.NowMs()
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	if _, err := tx.ExecContext(f.ctx,
		"UPDATE session SET revoked_at=? WHERE id=?", now, sid); err != nil {
		_ = tx.Rollback()
		t.Fatalf("吊销会话失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	_, err = f.svc.Refresh(f.ctx, raw, "dev-1", "测试设备")
	if err == nil {
		t.Fatal("已吊销会话的 refresh 不应成功")
	}
	var ae *apierr.AppError
	if !errors.As(err, &ae) {
		t.Fatalf("期望 apierr.AppError，实际 %T: %v", err, err)
	}
	if ae.Status != 401 {
		t.Errorf("账号未停用的泄露重放应仍是 401，实际 %d", ae.Status)
	}
}

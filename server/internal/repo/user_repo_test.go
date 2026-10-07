package repo

import (
	"errors"
	"strconv"
	"testing"

	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/store"
	"github.com/eznews/eznews/internal/util"
)

// mustSession 造一条真实会话并返回其ID。
//
// session.refresh_token_hash 上有唯一索引 ux_session_rt，且 user_id 有外键，
// 所以 token 哈希必须唯一、userID 必须是真实落库的用户。
func (f *fixture) mustSession(userID int64, tokenHash, clientID string, expiresAt int64) int64 {
	f.t.Helper()
	now := util.NowMs()
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		f.t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	id, err := f.users.InsertSession(f.ctx, tx, &model.Session{
		UserID: userID, RefreshTokenHash: tokenHash, ClientID: clientID,
		DeviceName: "测试设备", LastSeenAt: now, ExpiresAt: expiresAt, CreatedAt: now,
	})
	if err != nil {
		f.t.Fatalf("创建会话失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatalf("提交会话失败: %v", err)
	}
	return id
}

// inTx 在一个写事务里跑 fn，出错直接 Fatal。
//
// repo 里凡是带 tx 参数的方法都必须真的写进这个事务——若实现绕过 tx 走连接池，
// 事务内就读不到自己的写入，回滚也抹不掉。这类 bug 只有真库+ 真事务才测得出来。
func (f *fixture) inTx(fn func(tx store.Session) error) {
	f.t.Helper()
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		f.t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		f.t.Fatalf("事务体失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatalf("提交失败: %v", err)
	}
}

// TestUserCreateFind_往返_可空列被正确转换
//
// userCols 用 IFNULL 把 email / last_login_at 的 NULL 兜成零值，
// scanUser 才能无脑 Scan 进非指针字段。任一侧改动都会让 Scan 静默错位。
func TestUserCreateFind_往返_可空列被正确转换(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	var id int64
	f.inTx(func(tx store.Session) error {
		var err error
		id, err = f.users.Create(f.ctx, tx, &model.User{
			Username: "roundtrip", PasswordHash: "argon2id$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA",
			Email: "rt@example.com", Role: "admin", TokenVersion: 7,
			CreatedAt: now, UpdatedAt: now, LastLoginAt: 1_700_000_999,
		})
		return err
	})

	byName, err := f.users.FindByUsername(f.ctx, "roundtrip")
	if err != nil {
		t.Fatalf("FindByUsername 失败: %v", err)
	}
	if byName.ID != id || byName.Username != "roundtrip" {
		t.Fatalf("按名查到的行不对: %+v", byName)
	}
	if byName.PasswordHash != "argon2id$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA" {
		t.Fatalf("密码哈希被截断或错位: %q", byName.PasswordHash)
	}
	if byName.Email != "rt@example.com" || byName.Role != "admin" || byName.TokenVersion != 7 {
		t.Fatalf("文本/枚举列错位: %+v", byName)
	}
	if byName.LastLoginAt != 1_700_000_999 {
		t.Fatalf("last_login_at 错位: %d", byName.LastLoginAt)
	}
	byID, err := f.users.FindByID(f.ctx, id)
	if err != nil {
		t.Fatalf("FindByID 失败: %v", err)
	}
	if byID.Username != byName.Username || byID.TokenVersion != byName.TokenVersion {
		t.Fatalf("两种查法结果不一致: %+v vs %+v", byID, byName)
	}
}

// TestUserCreate_可空列留空_应写NULL并读回零值
//
// email 与 last_login_at 都是可空列：留空必须写 NULL（不能写空串/0），
// 否则「未设置邮箱」与「邮箱是空串」在业务上无法区分。
func TestUserCreate_可空列留空_应写NULL并读回零值(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	var id int64
	f.inTx(func(tx store.Session) error {
		var err error
		id, err = f.users.Create(f.ctx, tx, &model.User{
			Username: "nonull", PasswordHash: "h", Role: "user", TokenVersion: 1,
			CreatedAt: now, UpdatedAt: now,
		})
		return err
	})
	var email *string
	var lastLogin *int64
	if err := f.db.QueryRowContext(f.ctx,
		"SELECT email, last_login_at FROM user WHERE id=?", id).
		Scan(&email, &lastLogin); err != nil {
		t.Fatalf("直查失败: %v", err)
	}
	if email != nil || lastLogin != nil {
		t.Fatalf("留空的可空列应写成 NULL，实际 email=%v last_login=%v", email, lastLogin)
	}
	// IFNULL 兜底：读回来必须是零值而不是报错
	u, err := f.users.FindByID(f.ctx, id)
	if err != nil {
		t.Fatalf("FindByID 失败: %v", err)
	}
	if u.Email != "" || u.LastLoginAt != 0 {
		t.Fatalf("NULL 应被 IFNULL 兜成零值，实际 email=%q last_login=%d", u.Email, u.LastLoginAt)
	}
}

// TestUserCreate_用户名重复_必须报错
//
// ux_user_name 是登录的唯一锚：重名会让登录分不清是谁，
// 而「后来者覆盖前者」等于把别人的账号直接顶掉。
func TestUserCreate_用户名重复_必须报错(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	f.inTx(func(tx store.Session) error {
		_, err := f.users.Create(f.ctx, tx, &model.User{
			Username: "dup", PasswordHash: "h", Role: "user", TokenVersion: 1,
			CreatedAt: now, UpdatedAt: now,
		})
		return err
	})
	f.inTx(func(tx store.Session) error {
		_, err := f.users.Create(f.ctx, tx, &model.User{
			Username: "dup", PasswordHash: "h2", Role: "user", TokenVersion: 1,
			CreatedAt: now, UpdatedAt: now,
		})
		if err == nil {
			t.Fatal("重复的用户名必须被唯一索引拒绝")
		}
		return nil
	})
	if n := f.scalarI64("SELECT COUNT(*) FROM user WHERE username='dup'"); n != 1 {
		t.Fatalf("重复插入不应留下第二行，实际 %d 行", n)
	}
}

// TestUserFind_用户名大小写敏感_不得被折叠
//
// schema 用的是 BINARY  collation（默认），"Alice" 与 "alice" 是两个账号。
// 若哪天有人给这列加 COLLATE NOCASE，这条用例会红——那时必须同步想清楚
// 登录名与显示名的区分。
func TestUserFind_用户名大小写敏感_不得被折叠(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	f.inTx(func(tx store.Session) error {
		_, err := f.users.Create(f.ctx, tx, &model.User{
			Username: "Alice", PasswordHash: "h", Role: "user", TokenVersion: 1,
			CreatedAt: now, UpdatedAt: now,
		})
		return err
	})
	if _, err := f.users.FindByUsername(f.ctx, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("小写名应查不到，返回 %v", err)
	}
	if _, err := f.users.FindByUsername(f.ctx, "Alice"); err != nil {
		t.Fatalf("精确名应查得到: %v", err)
	}
}

// TestUserFind_不存在_返回ErrNotFound
//
// ErrNotFound 是 repo 层对外的统一语义：上层靠 errors.Is 区分「不存在」
// 与「数据库出错」，两者混同会让 404 变成 500。
func TestUserFind_不存在_返回ErrNotFound(t *testing.T) {
	f := newFixture(t)
	if _, err := f.users.FindByUsername(f.ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindByUsername 不存在应返回 ErrNotFound，实际 %v", err)
	}
	if _, err := f.users.FindByID(f.ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindByID 不存在应返回 ErrNotFound，实际 %v", err)
	}
}

// TestUserBumpTokenVersion_递增并返回新值
//
// token_version 是「全局登出」的开关：+1 之后所有已签发的 access token
// 都失效。返回值必须是新值（调用方拿它回写到响应里）。
func TestUserBumpTokenVersion_递增并返回新值(t *testing.T) {
	f := newFixture(t)
	var got int32
	f.inTx(func(tx store.Session) error {
		var err error
		got, err = f.users.BumpTokenVersion(f.ctx, tx, f.user, 1_700_000_500)
		return err
	})
	if got != 2 {
		t.Fatalf("初始 1 递增后应为 2，实际 %d", got)
	}
	cur, err := f.users.GetTokenVersion(f.ctx, f.user)
	if err != nil {
		t.Fatalf("GetTokenVersion 失败: %v", err)
	}
	if cur != 2 {
		t.Fatalf("库里的 token_version 应为 2，实际 %d", cur)
	}
	// 再递增一次必须累加而不是覆盖成 1
	f.inTx(func(tx store.Session) error {
		var err error
		got, err = f.users.BumpTokenVersion(f.ctx, tx, f.user, 1_700_000_600)
		return err
	})
	if got != 3 {
		t.Fatalf("第二次递增后应为 3，实际 %d", got)
	}
}

// TestUserGetTokenVersion_不存在_返回ErrNotFound而不是零值
//
// 零值是合法取值（新建账号默认 tv=1，但语义上 0 = 无效）。
// 若把不存在当成 0 返回，上层会拿一个假的 tv 去校验 token，等于放行一切。
func TestUserGetTokenVersion_不存在_返回ErrNotFound而不是零值(t *testing.T) {
	f := newFixture(t)
	v, err := f.users.GetTokenVersion(f.ctx, 999999)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的账号应返回 ErrNotFound，实际 v=%d err=%v", v, err)
	}
	if v != 0 {
		t.Fatalf("出错时返回值应为 0，实际 %d", v)
	}
}

// TestUserUpdatePassword_只改密码不改token_version
//
// 改密与全局登出是两件事：这里只负责换哈希，是否连带失效 token 由service 层
// 决定。若repo 顺手把 token_version 也+1，会静默踢掉该用户全部设备。
func TestUserUpdatePassword_只改密码不改token版本(t *testing.T) {
	f := newFixture(t)
	f.inTx(func(tx store.Session) error {
		return f.users.UpdatePassword(f.ctx, tx, f.user, "new-hash", 1_700_000_700)
	})
	u, err := f.users.FindByID(f.ctx, f.user)
	if err != nil {
		t.Fatalf("FindByID 失败: %v", err)
	}
	if u.PasswordHash != "new-hash" {
		t.Fatalf("密码哈希未更新: %q", u.PasswordHash)
	}
	if u.TokenVersion != 1 {
		t.Fatalf("改密不应顺带递增 token_version，实际 %d", u.TokenVersion)
	}
	if u.UpdatedAt != 1_700_000_700 {
		t.Fatalf("updated_at 未推进，实际 %d", u.UpdatedAt)
	}
}

// TestUserUpdateLastLogin_首次登录应从0变成时间戳
//
// 0 表示「从未登录」，是账号列表里「从未登录」文案的依据。
func TestUserUpdateLastLogin_首次登录应从0变成时间戳(t *testing.T) {
	f := newFixture(t)
	f.inTx(func(tx store.Session) error {
		return f.users.UpdateLastLogin(f.ctx, tx, f.user, 1_700_000_800)
	})
	u, err := f.users.FindByID(f.ctx, f.user)
	if err != nil {
		t.Fatalf("FindByID 失败: %v", err)
	}
	if u.LastLoginAt != 1_700_000_800 {
		t.Fatalf("last_login_at 应为 1700000800，实际 %d", u.LastLoginAt)
	}
	if u.UpdatedAt != 1_700_000_800 {
		t.Fatalf("updated_at 应与 last_login_at 同步，实际 %d", u.UpdatedAt)
	}
}

// TestUserUpdate_必须在传入的事务里_回滚后应完全消失
//
// UpdatePassword / UpdateLastLogin 都收store.Session 参数。
// 若实现偷偷走 r.db（连接池），事务里的写入对事务外可见，
// 回滚也抹不掉——这里用「事务内可见 + 回滚后消失」两段把它钉死。
func TestUserUpdate_必须在传入的事务里_回滚后应完全消失(t *testing.T) {
	f := newFixture(t)
	before, err := f.users.FindByID(f.ctx, f.user)
	if err != nil {
		t.Fatalf("前置读取失败: %v", err)
	}
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	if err := f.users.UpdatePassword(f.ctx, tx, f.user, "tx-only-hash", 1_700_000_900); err != nil {
		t.Fatalf("UpdatePassword 失败: %v", err)
	}
	// 事务内用同一条连接读，必须看到新值
	var seen string
	if err := tx.QueryRowContext(f.ctx,
		"SELECT password_hash FROM user WHERE id=?", f.user).Scan(&seen); err != nil {
		t.Fatalf("事务内直查失败: %v", err)
	}
	if seen != "tx-only-hash" {
		t.Fatalf("事务内应看到新哈希，实际 %q", seen)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	after, err := f.users.FindByID(f.ctx, f.user)
	if err != nil {
		t.Fatalf("后置读取失败: %v", err)
	}
	if after.PasswordHash != before.PasswordHash {
		t.Fatalf("回滚后密码哈希不应变化，实际 %q -> %q", before.PasswordHash, after.PasswordHash)
	}
}

// TestUserInsertSession_往返_可空列被正确转换
func TestUserInsertSession_往返_可空列被正确转换(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	id := f.mustSession(f.user, "hash-rt-1", "client-a", now+86_400_000)
	s, err := f.users.FindSessionByID(f.ctx, id, f.user)
	if err != nil {
		t.Fatalf("FindSessionByID 失败: %v", err)
	}
	if s.UserID != f.user || s.RefreshTokenHash != "hash-rt-1" || s.ClientID != "client-a" {
		t.Fatalf("文本列错位: %+v", s)
	}
	if s.DeviceName != "测试设备" {
		t.Fatalf("device_name 错位: %q", s.DeviceName)
	}
	if s.RevokedAt != 0 {
		t.Fatalf("未吊销会话 revoked_at 应为 0，实际 %d", s.RevokedAt)
	}
	if s.PrevRefreshTokenHash != "" || s.PrevRotatedAt != 0 {
		t.Fatalf("新会话的 prev_* 应兜成零值，实际 prev=%q at=%d", s.PrevRefreshTokenHash, s.PrevRotatedAt)
	}
}

// TestUserInsertSession_令牌哈希重复_必须报错
//
// ux_session_rt 保证「一个 refresh token 只对应一行」。
// 若这条唯一约束失效，同一令牌会被两个会话共享，reuse 检测就抓不到泄露。
func TestUserInsertSession_令牌哈希重复_必须报错(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	f.mustSession(f.user, "hash-dup", "client-a", now+86_400_000)
	f.inTx(func(tx store.Session) error {
		_, err := f.users.InsertSession(f.ctx, tx, &model.Session{
			UserID: f.user, RefreshTokenHash: "hash-dup", ClientID: "client-b",
			LastSeenAt: now, ExpiresAt: now + 86_400_000, CreatedAt: now,
		})
		if err == nil {
			t.Fatal("重复的 refresh_token_hash 必须被唯一索引拒绝")
		}
		return nil
	})
	if n := f.scalarI64("SELECT COUNT(*) FROM session WHERE refresh_token_hash='hash-dup'"); n != 1 {
		t.Fatalf("重复插入不应留下第二行，实际 %d 行", n)
	}
}

// TestUserFindSessionByID_归属不匹配_必须查不到
//
// userID 参与 WHERE 是防越权的关键：没有它，一次越权的 sid 探测就能读到
// 别人的 client_id / device_name，进而关联到具体设备。
// 这条断言就是「拿别人的 sid 读不到」的直接证据。
func TestUserFindSessionByID_归属不匹配_必须查不到(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	other := f.mustUser("repo-user-other")
	id := f.mustSession(f.user, "hash-own", "client-a", now+86_400_000)
	if _, err := f.users.FindSessionByID(f.ctx, id, other); !errors.Is(err, ErrNotFound) {
		t.Fatalf("用他人 userID 查会话必须查不到，实际 %v", err)
	}
}

// TestUserFindSessionByHash_含已吊销会话
//
// reuse 检测必须能查到「已吊销」的会话：泄露发生时会话已经被踢了，
// 查不到就意味着检测不出来。所以这里特意吊销后再查，断言仍能查到。
func TestUserFindSessionByHash_含已吊销会话(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	id := f.mustSession(f.user, "hash-revoked", "client-a", now+86_400_000)
	if err := f.users.RevokeSession(f.ctx, f.db, id, f.user, now); err != nil {
		t.Fatalf("吊销失败: %v", err)
	}
	s, err := f.users.FindSessionByHash(f.ctx, "hash-revoked")
	if err != nil {
		t.Fatalf("已吊销会话也必须查得到（reuse 检测依赖它）: %v", err)
	}
	if s.RevokedAt != now {
		t.Fatalf("revoked_at 应为 %d，实际 %d", now, s.RevokedAt)
	}
}

// TestUserRotateSession_必须原地更新同一行且旧哈希移入prev
//
// ★ 这是全文件最重要的不变量。RotateSession 的注释里明确禁止改成 delete+insert：
//   - DEC-7：旧哈希移入 prev_refresh_token_hash，宽限窗口才能识别「同设备网络重试」；
//     delete+insert 会让 prev_* 永远是 NULL，60s 宽限形同虚设，
//     Android 弱网重放旧 token 会被误判为泄露，全设备被踢。
//   - DEC-14：sid = session.id 被 JWT 携带并在登出时定位；
//     delete+insert 让 sid 每次刷新都变，登出等于没做。
//
// 本用例把「行数不变、id 不变、prev 被填」三件事一起钉住。
func TestUserRotateSession_必须原地更新同一行且旧哈希移入prev(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	id := f.mustSession(f.user, "hash-gen1", "client-a", now+86_400_000)
	before := f.rows("session")

	var affected int64
	f.inTx(func(tx store.Session) error {
		var err error
		affected, err = f.users.RotateSession(f.ctx, tx, id, &model.Session{
			UserID: f.user, RefreshTokenHash: "hash-gen2", ClientID: "client-a",
			DeviceName: "新设备名", ExpiresAt: now + 172_800_000,
		}, now+1000)
		return err
	})
	if affected != 1 {
		t.Fatalf("轮换应影响 1 行，实际 %d", affected)
	}
	if after := f.rows("session"); after != before {
		t.Fatalf("轮换不得改变行数（禁止 delete+insert），%d -> %d", before, after)
	}
	s, err := f.users.FindSessionByID(f.ctx, id, f.user)
	if err != nil {
		t.Fatalf("按原 id 查询失败: %v", err)
	}
	if s.ID != id {
		t.Fatalf("session.id 变了：%d -> %d（sid 必须稳定）", id, s.ID)
	}
	if s.RefreshTokenHash != "hash-gen2" {
		t.Fatalf("当前代哈希未更新: %q", s.RefreshTokenHash)
	}
	if s.PrevRefreshTokenHash != "hash-gen1" {
		t.Fatalf("旧哈希必须移入 prev_refresh_token_hash，实际 %q", s.PrevRefreshTokenHash)
	}
	if s.PrevRotatedAt != now+1000 {
		t.Fatalf("prev_rotated_at 应为轮换时刻 %d，实际 %d", now+1000, s.PrevRotatedAt)
	}
	if s.DeviceName != "新设备名" {
		t.Fatalf("设备名未更新: %q", s.DeviceName)
	}
	if s.ExpiresAt != now+172_800_000 {
		t.Fatalf("过期时间未滑动: %d", s.ExpiresAt)
	}
	// 旧哈希仍能被 prev 查到——这正是宽限判定的入口
	byPrev, err := f.users.FindSessionByPrevHash(f.ctx, "hash-gen1")
	if err != nil {
		t.Fatalf("FindSessionByPrevHash 失败: %v", err)
	}
	if byPrev.ID != id {
		t.Fatalf("prev 查到的应是同一行，实际 %d", byPrev.ID)
	}
}

// TestUserRotateSession_设备名为空_必须保留原值
//
// SQL 用的是 COALESCE(NULLIF(?,”), device_name)：刷新时客户端没带设备名
// 不能把已知的设备名擦成空。若这条退化，写成 device_name=?，本用例会红。
func TestUserRotateSession_设备名为空_必须保留原值(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	id := f.mustSession(f.user, "hash-dev", "client-a", now+86_400_000)
	f.inTx(func(tx store.Session) error {
		_, err := f.users.RotateSession(f.ctx, tx, id, &model.Session{
			UserID: f.user, RefreshTokenHash: "hash-dev2", ClientID: "client-a",
			DeviceName: "", ExpiresAt: now + 86_400_000,
		}, now+1000)
		return err
	})
	s, err := f.users.FindSessionByID(f.ctx, id, f.user)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if s.DeviceName != "测试设备" {
		t.Fatalf("空设备名不应覆盖原值，实际 %q", s.DeviceName)
	}
}

// TestUserRotateSession_已吊销或非本人_必须返回0行
//
// 返回行数是调用方唯一的并发信号：==0 说明会话已被并发吊销，
// 上层据此判定 reuse 命中。若这里对已吊销会话也返回 1，
// 一个本该被拒的刷新请求就会被放行。
func TestUserRotateSession_已吊销或非本人_必须返回0行(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	other := f.mustUser("repo-user-rot-other")

	revokedID := f.mustSession(f.user, "hash-rev", "client-a", now+86_400_000)
	if err := f.users.RevokeSession(f.ctx, f.db, revokedID, f.user, now); err != nil {
		t.Fatalf("吊销失败: %v", err)
	}
	var affected int64
	f.inTx(func(tx store.Session) error {
		var err error
		affected, err = f.users.RotateSession(f.ctx, tx, revokedID, &model.Session{
			UserID: f.user, RefreshTokenHash: "hash-x", ClientID: "client-a",
			ExpiresAt: now + 86_400_000,
		}, now+1000)
		return err
	})
	if affected != 0 {
		t.Fatalf("已吊销会话轮换应返回 0 行，实际 %d", affected)
	}

	// 归属不匹配：拿别人的会话 id 去轮换自己的令牌
	otherID := f.mustSession(other, "hash-other", "client-b", now+86_400_000)
	f.inTx(func(tx store.Session) error {
		var err error
		affected, err = f.users.RotateSession(f.ctx, tx, otherID, &model.Session{
			UserID: f.user, RefreshTokenHash: "hash-steal", ClientID: "client-a",
			ExpiresAt: now + 86_400_000,
		}, now+1000)
		return err
	})
	if affected != 0 {
		t.Fatalf("非本人会话轮换应返回 0 行，实际 %d", affected)
	}
}

// TestUserClearPrevHash_清空后旧哈希不应再被查到
//
// 宽限期结束（60s）后调用，防止 prev 列长期滞留旧哈希。
// 若不清，宽限期外的旧 token 会被 FindSessionByPrevHash 命中，
// reuse 检测需要额外再做一次时间窗判断才不会误判。
func TestUserClearPrevHash_清空后旧哈希不应再被查到(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	id := f.mustSession(f.user, "hash-c1", "client-a", now+86_400_000)
	f.inTx(func(tx store.Session) error {
		_, err := f.users.RotateSession(f.ctx, tx, id, &model.Session{
			UserID: f.user, RefreshTokenHash: "hash-c2", ClientID: "client-a",
			ExpiresAt: now + 86_400_000,
		}, now+1000)
		return err
	})
	if _, err := f.users.FindSessionByPrevHash(f.ctx, "hash-c1"); err != nil {
		t.Fatalf("轮换后旧哈希应能查到: %v", err)
	}
	f.inTx(func(tx store.Session) error {
		return f.users.ClearPrevHash(f.ctx, tx, id)
	})
	if _, err := f.users.FindSessionByPrevHash(f.ctx, "hash-c1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("清理后旧哈希不应再查到，实际 %v", err)
	}
	// 当前代哈希不受影响
	if _, err := f.users.FindSessionByHash(f.ctx, "hash-c2"); err != nil {
		t.Fatalf("当前代哈希不应被清理掉: %v", err)
	}
}

// TestUserIsSessionActive_不存在时返回false而不是错误
//
// 热路径每请求都问一次。行不存在（Janitor 已清理）与「已吊销」在语义上
// 等价，都应返回 false,nil；返回错误会让上层多写一个无意义分支，
// 更糟的是可能把 401 变成 500。
func TestUserIsSessionActive_不存在时返回false而不是错误(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	ok, err := f.users.IsSessionActive(f.ctx, 999999, f.user)
	if err != nil {
		t.Fatalf("不存在的会话不应报错: %v", err)
	}
	if ok {
		t.Fatal("不存在的会话必须是 false")
	}
	// 归属不匹配同样 fail-closed
	id := f.mustSession(f.user, "hash-act", "client-a", now+86_400_000)
	other := f.mustUser("repo-user-act-other")
	ok, err = f.users.IsSessionActive(f.ctx, id, other)
	if err != nil {
		t.Fatalf("归属不匹配不应报错: %v", err)
	}
	if ok {
		t.Fatal("他人会话必须 fail-closed 为 false")
	}
}

// TestUserIsSessionActive_有效与已吊销
func TestUserIsSessionActive_有效与已吊销(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	id := f.mustSession(f.user, "hash-act2", "client-a", now+86_400_000)
	ok, err := f.users.IsSessionActive(f.ctx, id, f.user)
	if err != nil || !ok {
		t.Fatalf("有效会话应返回 true,nil，实际 %v %v", ok, err)
	}
	if err := f.users.RevokeSession(f.ctx, f.db, id, f.user, now); err != nil {
		t.Fatalf("吊销失败: %v", err)
	}
	ok, err = f.users.IsSessionActive(f.ctx, id, f.user)
	if err != nil || ok {
		t.Fatalf("已吊销会话应返回 false,nil，实际 %v %v", ok, err)
	}
}

// TestUserRevokeSession_重复吊销第二次必须返回ErrNotFound
//
// 重复吊销返回 ErrNotFound 而不是静默成功：调用方需要区分
// 「我吊销了它」与「它已经不在了」。
func TestUserRevokeSession_重复吊销第二次必须返回ErrNotFound(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	id := f.mustSession(f.user, "hash-rev2", "client-a", now+86_400_000)
	if err := f.users.RevokeSession(f.ctx, f.db, id, f.user, now); err != nil {
		t.Fatalf("首次吊销失败: %v", err)
	}
	if err := f.users.RevokeSession(f.ctx, f.db, id, f.user, now+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复吊销应返回 ErrNotFound，实际 %v", err)
	}
	// 归属不匹配也不能吊销别人的会话
	other := f.mustUser("repo-user-rev-other")
	otherID := f.mustSession(other, "hash-rev-other", "client-b", now+86_400_000)
	if err := f.users.RevokeSession(f.ctx, f.db, otherID, f.user, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("越权吊销应返回 ErrNotFound，实际 %v", err)
	}
	s, err := f.users.FindSessionByID(f.ctx, otherID, other)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if s.RevokedAt != 0 {
		t.Fatalf("越权吊销不得生效，实际 revoked_at=%d", s.RevokedAt)
	}
}

// TestUserRevokeAllSessions_只影响本人且返回真实条数
//
// reuse 检测命中时要踢掉该用户全部设备；返回条数供上层记日志/审计。
// 已吊销的会话不该被重复计入（SQL 里有 revoked_at IS NULL 过滤）。
func TestUserRevokeAllSessions_只影响本人且返回真实条数(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	other := f.mustUser("repo-user-all-other")
	f.mustSession(f.user, "u1", "client-a", now+86_400_000)
	f.mustSession(f.user, "u2", "client-a", now+86_400_000)
	revokedID := f.mustSession(f.user, "u3", "client-a", now+86_400_000)
	otherID := f.mustSession(other, "o1", "client-b", now+86_400_000)
	if err := f.users.RevokeSession(f.ctx, f.db, revokedID, f.user, now); err != nil {
		t.Fatalf("预置吊销失败: %v", err)
	}

	var n int64
	f.inTx(func(tx store.Session) error {
		var err error
		n, err = f.users.RevokeAllSessions(f.ctx, tx, f.user, now+500)
		return err
	})
	if n != 2 {
		t.Fatalf("只应吊销 2 个未吊销会话，实际 %d", n)
	}
	// 别人的会话不受影响
	otherS, err := f.users.FindSessionByID(f.ctx, otherID, other)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if otherS.RevokedAt != 0 {
		t.Fatalf("他人会话被误吊销: revoked_at=%d", otherS.RevokedAt)
	}
	// 再次全量吊销应返回 0
	f.inTx(func(tx store.Session) error {
		var err error
		n, err = f.users.RevokeAllSessions(f.ctx, tx, f.user, now+600)
		return err
	})
	if n != 0 {
		t.Fatalf("已全部吊销后应返回 0，实际 %d", n)
	}
}

// TestUserListSessions_过滤已吊销与已过期并按活跃度倒序
//
// 「我的设备」页的数据源：只列还能用的会话，最近活跃的排最前。
// 排序键last_seen_at 不唯一，但这个列表一次性全量返回、不分页，
// 所以不需要 tiebreaker——若有tiebreaker 则同值时的顺序无业务含义。
func TestUserListSessions_过滤已吊销与已过期并按活跃度倒序(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	other := f.mustUser("repo-user-list-other")
	old := f.mustSession(f.user, "s-old", "client-a", now+86_400_000)
	mid := f.mustSession(f.user, "s-mid", "client-a", now+86_400_000)
	newest := f.mustSession(f.user, "s-new", "client-a", now+86_400_000)
	expired := f.mustSession(f.user, "s-exp", "client-a", now-1)
	revoked := f.mustSession(f.user, "s-rev", "client-a", now+86_400_000)
	f.mustSession(other, "s-other", "client-b", now+86_400_000)

	// 直接改 last_seen_at 造出明确顺序
	for i, id := range []int64{old, mid, newest} {
		ts := now + int64(1000*(i+1))
		if _, err := f.db.ExecContext(f.ctx,
			"UPDATE session SET last_seen_at=? WHERE id=?", ts, id); err != nil {
			t.Fatalf("更新 last_seen_at 失败: %v", err)
		}
	}
	if err := f.users.RevokeSession(f.ctx, f.db, revoked, f.user, now); err != nil {
		t.Fatalf("吊销失败: %v", err)
	}

	list, err := f.users.ListSessions(f.ctx, f.user, now)
	if err != nil {
		t.Fatalf("ListSessions 失败: %v", err)
	}
	got := make([]int64, 0, len(list))
	for _, s := range list {
		got = append(got, s.ID)
	}
	want := []int64{newest, mid, old}
	if len(got) != len(want) {
		t.Fatalf("活跃会话数不对，期望 %v 实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("排序不对，期望 %v 实际 %v", want, got)
		}
	}
	for _, id := range []int64{expired, revoked} {
		for _, g := range got {
			if g == id {
				t.Fatalf("已过期/已吊销的会话 %d 不该出现在活跃列表里", id)
			}
		}
	}
}

// TestUserDeleteExpiredSessions_过期立即删_吊销需过保留期
//
// 两个删除条件的时间基准不同：expires_at 直接比now，
// revoked_at 要比「now - 保留期」——刚吊销的会话要留一段时间，
// 供 reuse 检测在窗口内回查它的 client_id。
func TestUserDeleteExpiredSessions_过期立即删_吊销需过保留期(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	const retention = 7 * 24 * 3600 * 1000

	f.mustSession(f.user, "d-expired", "client-a", now-1)
	fresh := f.mustSession(f.user, "d-fresh", "client-a", now+86_400_000)
	oldRevoked := f.mustSession(f.user, "d-oldrev", "client-a", now+86_400_000)
	newRevoked := f.mustSession(f.user, "d-newrev", "client-a", now+86_400_000)
	if _, err := f.db.ExecContext(f.ctx,
		"UPDATE session SET revoked_at=? WHERE id=?", now-int64(retention)-1, oldRevoked); err != nil {
		t.Fatalf("预置吊销时间失败: %v", err)
	}
	if err := f.users.RevokeSession(f.ctx, f.db, newRevoked, f.user, now); err != nil {
		t.Fatalf("吊销失败: %v", err)
	}

	var n int64
	f.inTx(func(tx store.Session) error {
		var err error
		n, err = f.users.DeleteExpiredSessions(f.ctx, tx, now, retention)
		return err
	})
	if n != 2 {
		t.Fatalf("应删除 2 条（过期 1 + 吊销超期1），实际 %d", n)
	}
	if f.rows("session") != 2 {
		t.Fatalf("session 表应剩 2 行，实际 %d", f.rows("session"))
	}
	// 已清理的两条必须查不到（哈希是已知的固定值，不做反查——
	// 行被删掉后反查本身就会失败，那样测不到任何东西）
	for _, h := range []string{"d-expired", "d-oldrev"} {
		if _, err := f.users.FindSessionByHash(f.ctx, h); !errors.Is(err, ErrNotFound) {
			t.Fatalf("哈希 %s 的会话应已被清理，实际 %v", h, err)
		}
	}
	for _, id := range []int64{fresh, newRevoked} {
		if _, err := f.users.FindSessionByID(f.ctx, id, f.user); err != nil {
			t.Fatalf("会话 %d 不该被清理: %v", id, err)
		}
	}
}

// TestUserDeleteUser_级联清理全部关联数据
//
// 账号注销涉及 7 张表。schema 上有 ON DELETE CASCADE，但 DeleteUser 仍显式
// 逐表删——为的是「foreign_keys 没生效的连接」也能清干净。
// 这条用例把 7 张表全部验一遍：漏掉任何一张，注销后都会残留孤儿数据。
func TestUserDeleteUser_级联清理全部关联数据(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	victim := f.mustUser("repo-user-victim")
	keep := f.mustUser("repo-user-keep")

	// 让 victim 成为某个源的 owner，验证 owner_user_id 侧也被清理
	// 让 victim 独占一个源，keep 独占另一个，用来验证 owner_user_id 侧的清理边界。
	// 刻意不把承载测试文章的 f.source挂到 victim 名下——否则删源会级联删文章，
	// 再级联删掉 keep 的收藏，把「owner 清理」和「文章级联」两件事缠在一起，
	// keep 的断言就变成在测级联而不是测 owner 清理。
	victimOnly := f.mustSource("repo-src-victim-only", "tech")
	keepOnly := f.mustSource("repo-src-keep-only", "tech")
	if _, err := f.db.ExecContext(f.ctx,
		"UPDATE source SET owner_user_id=? WHERE id=?", victim, victimOnly); err != nil {
		t.Fatalf("设置 victim 源 owner 失败: %v", err)
	}
	if _, err := f.db.ExecContext(f.ctx,
		"UPDATE source SET owner_user_id=? WHERE id=?", keep, keepOnly); err != nil {
		t.Fatalf("设置 keep 源 owner 失败: %v", err)
	}
	art := f.article(1, articleOpts{})

	// 会话
	f.mustSession(victim, "v-s1", "client-a", now+86_400_000)
	f.mustSession(keep, "k-s1", "client-a", now+86_400_000)
	// 收藏/已读
	f.inTx(func(tx store.Session) error {
		if err := f.state.SetFavoriteTx(f.ctx, tx, victim, art, true, now); err != nil {
			return err
		}
		return f.state.SetReadTx(f.ctx, tx, victim, art, false, now)
	})
	f.inTx(func(tx store.Session) error {
		if err := f.state.SetFavoriteTx(f.ctx, tx, keep, art, true, now); err != nil {
			return err
		}
		return f.state.SetReadTx(f.ctx, tx, keep, art, false, now)
	})
	// 偏好
	for _, uid := range []int64{victim, keep} {
		f.inTx(func(tx store.Session) error {
			return f.state.ReplacePrefs(f.ctx, tx, uid, map[string]string{"ui.density": "compact"}, now)
		})
	}
	// 合并日志
	for _, uid := range []int64{victim, keep} {
		f.inTx(func(tx store.Session) error {
			return f.state.InsertMergeLog(f.ctx, tx, &model.MergeLog{
				UserID: uid, ClientID: "client-x",
				Nonce:      "nonce-" + strconv.FormatInt(uid, 10),
				ResultJSON: `{"ok":true}`, CreatedAt: now,
			})
		})
	}

	// 删前先确认数据都在
	if f.rows("session") != 2 || f.rows("user_favorite") != 2 || f.rows("user_read") != 2 {
		t.Fatalf("前置数据不对: session=%d fav=%d read=%d",
			f.rows("session"), f.rows("user_favorite"), f.rows("user_read"))
	}

	f.inTx(func(tx store.Session) error {
		return f.users.DeleteUser(f.ctx, tx, victim)
	})

	// victim 的行全部消失
	if n := f.scalarI64("SELECT COUNT(*) FROM session WHERE user_id=?", victim); n != 0 {
		t.Fatalf("残留 session %d 行", n)
	}
	if n := f.scalarI64("SELECT COUNT(*) FROM user_favorite WHERE user_id=?", victim); n != 0 {
		t.Fatalf("残留 user_favorite %d 行", n)
	}
	if n := f.scalarI64("SELECT COUNT(*) FROM user_read WHERE user_id=?", victim); n != 0 {
		t.Fatalf("残留 user_read %d 行", n)
	}
	if n := f.scalarI64("SELECT COUNT(*) FROM user_preference WHERE user_id=?", victim); n != 0 {
		t.Fatalf("残留 user_preference %d 行", n)
	}
	if n := f.scalarI64("SELECT COUNT(*) FROM merge_log WHERE user_id=?", victim); n != 0 {
		t.Fatalf("残留 merge_log %d 行", n)
	}
	if n := f.scalarI64("SELECT COUNT(*) FROM source WHERE owner_user_id=?", victim); n != 0 {
		t.Fatalf("残留 owner_user_id 指向 victim 的 source %d 行", n)
	}
	if _, err := f.users.FindByID(f.ctx, victim); !errors.Is(err, ErrNotFound) {
		t.Fatalf("账号本身应已删除，实际 %v", err)
	}
	// keep 的数据一根都不能少
	if n := f.scalarI64("SELECT COUNT(*) FROM session WHERE user_id=?", keep); n != 1 {
		t.Fatalf("keep 的 session 应保留 1 行，实际 %d", n)
	}
	if n := f.scalarI64("SELECT COUNT(*) FROM user_favorite WHERE user_id=?", keep); n != 1 {
		t.Fatalf("keep 的收藏应保留 1 行，实际 %d", n)
	}
	if n := f.scalarI64("SELECT COUNT(*) FROM user_read WHERE user_id=?", keep); n != 1 {
		t.Fatalf("keep 的已读应保留 1 行，实际 %d", n)
	}
	if n := f.scalarI64("SELECT COUNT(*) FROM user_preference WHERE user_id=?", keep); n != 1 {
		t.Fatalf("keep 的偏好应保留 1 行，实际 %d", n)
	}
	if n := f.scalarI64("SELECT COUNT(*) FROM merge_log WHERE user_id=?", keep); n != 1 {
		t.Fatalf("keep 的合并日志应保留 1 行，实际 %d", n)
	}
	// keep 独占的源不能被误删；victim 独占的源必须已删
	if n := f.scalarI64("SELECT COUNT(*) FROM source WHERE id=?", keepOnly); n != 1 {
		t.Fatalf("keep 独占的源被误删了")
	}
	if n := f.scalarI64("SELECT COUNT(*) FROM source WHERE id=?", victimOnly); n != 0 {
		t.Fatalf("victim 独占的源应随账号一起删除，实际还剩 %d 行", n)
	}
	// 承载测试文章的源不属于任何账号，必须原封不动
	if n := f.scalarI64("SELECT COUNT(*) FROM article WHERE id=?", art); n != 1 {
		t.Fatalf("测试文章被误删了")
	}
}

// TestUserDeleteUser_不存在的账号_返回ErrNotFound
func TestUserDeleteUser_不存在的账号_返回ErrNotFound(t *testing.T) {
	f := newFixture(t)
	f.inTx(func(tx store.Session) error {
		err := f.users.DeleteUser(f.ctx, tx, 999999)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("删除不存在的账号应返回 ErrNotFound，实际 %v", err)
		}
		return nil
	})
}

// TestUserSession_事务回滚后写入不得残留
//
// 一次登录要写 user.last_login_at + session 两处。若其中一处绕过 tx 走连接池，
// 外层回滚时它会留在库里，形成「有会话但没登录时间」的幽灵数据。
func TestUserSession_事务回滚后写入不得残留(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := f.users.UpdateLastLogin(f.ctx, tx, f.user, now); err != nil {
		t.Fatalf("UpdateLastLogin 失败: %v", err)
	}
	if _, err := f.users.InsertSession(f.ctx, tx, &model.Session{
		UserID: f.user, RefreshTokenHash: "rb-s1", ClientID: "client-a",
		LastSeenAt: now, ExpiresAt: now + 86_400_000, CreatedAt: now,
	}); err != nil {
		t.Fatalf("InsertSession 失败: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	u, err := f.users.FindByID(f.ctx, f.user)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if u.LastLoginAt != 0 {
		t.Fatalf("回滚后 last_login_at 应仍为 0，实际 %d", u.LastLoginAt)
	}
	if n := f.scalarI64("SELECT COUNT(*) FROM session WHERE refresh_token_hash='rb-s1'"); n != 0 {
		t.Fatalf("回滚后会话不应落盘，实际 %d 行", n)
	}
}

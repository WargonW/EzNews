package repo

import (
	"errors"
	"testing"

	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/store"
)

// TestUpsertFavorite_同键二次写入_必须走ON CONFLICT更新而不是报错
//
// 增量同步客户端会重放整个批次，唯一索引 ux_fav(user_id, article_id) 一定会被撞。
// 若这里是裸 INSERT，第二次就报 UNIQUE 约束错误，整个合并请求直接失败。
// ON CONFLICT DO UPDATE 是幂等性的唯一保证。
func TestUpsertFavorite_同键二次写入_必须走冲突更新而不是报错(t *testing.T) {
	f := newFixture(t)
	id := f.article(300, articleOpts{URL: "https://repo.example.com/s1"})
	ctx := f.ctx

	first, err := f.state.UpsertFavorite(ctx, f.db, &model.Favorite{
		UserID: f.user, ArticleID: id, DeletedAt: 0, CreatedAt: 1000, UpdatedAt: 1000,
	})
	if err != nil {
		t.Fatalf("首次 UpsertFavorite 失败: %v", err)
	}
	if !first {
		t.Fatal("首次写入必须报告发生了变更（回执 added）")
	}

	// 同键再来一次：更晚的 updated_at + 变成墓碑
	second, err := f.state.UpsertFavorite(ctx, f.db, &model.Favorite{
		UserID: f.user, ArticleID: id, DeletedAt: 2000, CreatedAt: 1000, UpdatedAt: 2000,
	})
	if err != nil {
		t.Fatalf("同键二次 UpsertFavorite 报错（ON CONFLICT 失效）: %v", err)
	}
	if !second {
		t.Fatal("updated_at 更新的写入必须报告发生了变更")
	}
	if got := f.rows("user_favorite"); got != 1 {
		t.Fatalf("重放不得产生重复行，实际 %d 行（唯一索引/ON CONFLICT 失效）", got)
	}
	var deletedAt *int64
	if err := f.db.QueryRowContext(ctx, "SELECT deleted_at FROM user_favorite WHERE article_id=?", id).
		Scan(&deletedAt); err != nil {
		t.Fatalf("读取墓碑失败: %v", err)
	}
	if deletedAt == nil || *deletedAt != 2000 {
		t.Fatalf("DO UPDATE 应把 deleted_at 覆盖为 2000，实际 %v", deletedAt)
	}
}

// TestUpsertFavorite_★LWW_更旧的写入必须被丢弃
//
// 合并是 Last-Write-Wins：updated_at 更小的写入不能覆盖已有的较新状态，
// 否则乱序到达的旧客户端会把新状态改回去（收藏被莫名取消 / 又冒出来）。
// ON CONFLICT 的 WHERE 子句 `excluded.updated_at > user_favorite.updated_at` 是关键，
// 删掉它这条用例会红。
func TestUpsertFavorite_LWW_更旧的写入必须被丢弃(t *testing.T) {
	f := newFixture(t)
	id := f.article(301, articleOpts{URL: "https://repo.example.com/s2"})
	ctx := f.ctx

	if _, err := f.state.UpsertFavorite(ctx, f.db, &model.Favorite{
		UserID: f.user, ArticleID: id, DeletedAt: 5000, CreatedAt: 1000, UpdatedAt: 5000,
	}); err != nil {
		t.Fatalf("写入新状态失败: %v", err)
	}

	// 更旧的写入试图「复活」收藏：必须被丢弃
	changed, err := f.state.UpsertFavorite(ctx, f.db, &model.Favorite{
		UserID: f.user, ArticleID: id, DeletedAt: 0, CreatedAt: 1000, UpdatedAt: 1000,
	})
	if err != nil {
		t.Fatalf("写入更旧状态失败: %v", err)
	}
	if changed {
		t.Fatal("updated_at 更旧的写入必须被 LWW 规则丢弃（changed 应为 false）")
	}
	var deletedAt *int64
	if err := f.db.QueryRowContext(ctx, "SELECT deleted_at FROM user_favorite WHERE article_id=?", id).
		Scan(&deletedAt); err != nil {
		t.Fatalf("读取墓碑失败: %v", err)
	}
	if deletedAt == nil || *deletedAt != 5000 {
		t.Fatalf("较旧的写入不得覆盖墓碑，实际 deleted_at=%v（期望 5000）", deletedAt)
	}
}

// TestUpsertFavorite_相同updatedAt_不报告变更
//
// 时间戳相等时 `excluded.updated_at > updated_at` 为假，
// 视为无变更（幂等重放不应产生额外回执）。
func TestUpsertFavorite_相同updatedAt_不报告变更(t *testing.T) {
	f := newFixture(t)
	id := f.article(302, articleOpts{URL: "https://repo.example.com/s3"})
	ctx := f.ctx
	if _, err := f.state.UpsertFavorite(ctx, f.db, &model.Favorite{
		UserID: f.user, ArticleID: id, DeletedAt: 0, CreatedAt: 1000, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	changed, err := f.state.UpsertFavorite(ctx, f.db, &model.Favorite{
		UserID: f.user, ArticleID: id, DeletedAt: 9, CreatedAt: 1000, UpdatedAt: 1000,
	})
	if err != nil {
		t.Fatalf("相同时间戳写入失败: %v", err)
	}
	if changed {
		t.Fatal("updated_at 相同时不应报告变更（避免重放刷出虚假的 changed 回执）")
	}
}

// TestUpsertRead_★同键二次写入_必须走ON CONFLICT更新
//
// user_read 与 user_favorite 是同构的两张表，共用同一套 upsert 语义。
// 这里用「批量标记已读」的真实形态验证：一条文章被标记多次已读只应有一行。
func TestUpsertRead_同键二次写入_必须走冲突更新(t *testing.T) {
	f := newFixture(t)
	id := f.article(303, articleOpts{URL: "https://repo.example.com/s4"})
	ctx := f.ctx

	if _, err := f.state.UpsertRead(ctx, f.db, &model.Read{
		UserID: f.user, ArticleID: id, DeletedAt: 0, CreatedAt: 1000, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("首次 UpsertRead 失败: %v", err)
	}
	// 取消已读（墓碑）
	if _, err := f.state.UpsertRead(ctx, f.db, &model.Read{
		UserID: f.user, ArticleID: id, DeletedAt: 3000, CreatedAt: 1000, UpdatedAt: 3000,
	}); err != nil {
		t.Fatalf("二次 UpsertRead 报错（ON CONFLICT 失效）: %v", err)
	}
	if got := f.rows("user_read"); got != 1 {
		t.Fatalf("重放不得产生重复行，实际 %d 行", got)
	}
	// LWW：更旧的「标记已读」不得复活
	changed, err := f.state.UpsertRead(ctx, f.db, &model.Read{
		UserID: f.user, ArticleID: id, DeletedAt: 0, CreatedAt: 1000, UpdatedAt: 1000,
	})
	if err != nil {
		t.Fatalf("写入更旧状态失败: %v", err)
	}
	if changed {
		t.Fatal("更旧的写入必须被丢弃")
	}
}

// TestUpsertRead_DeletedAt为0_应写NULL而非0
//
// nullInt64 把 0 转成 NULL，读回时靠 IFNULL(deleted_at,0) 兜成 0。
// 若直接把 0 写进去，「有效」与「墓碑时间恰好为 0」就永远无法区分。
func TestUpsertRead_DeletedAt为0_应写NULL而非0(t *testing.T) {
	f := newFixture(t)
	id := f.article(304, articleOpts{URL: "https://repo.example.com/s5"})
	ctx := f.ctx
	if _, err := f.state.UpsertRead(ctx, f.db, &model.Read{
		UserID: f.user, ArticleID: id, DeletedAt: 0, CreatedAt: 1000, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("UpsertRead 失败: %v", err)
	}
	var raw *int64
	if err := f.db.QueryRowContext(ctx, "SELECT deleted_at FROM user_read WHERE article_id=?", id).
		Scan(&raw); err != nil {
		t.Fatalf("直查失败: %v", err)
	}
	if raw != nil {
		t.Fatalf("deleted_at=0 必须写成 NULL，实际写入 %d —— 0 与 NULL 语义不同", *raw)
	}
	reads, err := f.state.ListReadsByIDs(ctx, f.user, []int64{id})
	if err != nil {
		t.Fatalf("ListReadsByIDs 失败: %v", err)
	}
	if len(reads) != 1 || reads[0].DeletedAt != 0 {
		t.Fatalf("读回应得到 DeletedAt=0 的有效行，实际 %+v", reads)
	}
}

// TestSetFavorite_★墓碑与复活_再收藏应把deleted_at置回NULL
//
// ★ 软删除墓碑语义的核心：取消收藏写墓碑（不删行），
// 之后重新收藏必须把 deleted_at 置回 NULL（"复活"），
// 而不是残留一个旧墓碑 —— 残留墓碑会让「已收藏」的文章对不上账，
// 表现为收藏列表里条目永远无法真正生效。
//
// SetFavorite 内部走 SetFavoriteTx，ON CONFLICT 的 DO UPDATE SET deleted_at=excluded.deleted_at
// 负责把墓碑清掉。若这里退化成 INSERT（冲突报错）或只更新 updated_at，复活都会失效。
func TestSetFavorite_墓碑与复活_再收藏应把deleted_at置回NULL(t *testing.T) {
	f := newFixture(t)
	id := f.article(310, articleOpts{URL: "https://repo.example.com/f1"})
	ctx := f.ctx

	// 1) 收藏
	if err := f.state.SetFavorite(ctx, f.user, id, false, 1000); err != nil {
		t.Fatalf("收藏失败: %v", err)
	}
	if n := f.countActive(FavoriteTable(), f.user); n != 1 {
		t.Fatalf("收藏后有效条目应为 1，实际 %d", n)
	}

	// 2) 取消收藏 → 墓碑，仍是同一行
	if err := f.state.SetFavorite(ctx, f.user, id, true, 2000); err != nil {
		t.Fatalf("取消收藏失败: %v", err)
	}
	if got := f.rows("user_favorite"); got != 1 {
		t.Fatalf("取消收藏后应仍是 1 行（墓碑），实际 %d —— 若为 0 说明物理删了行，增量同步会丢信号", got)
	}
	if n := f.countActive(FavoriteTable(), f.user); n != 0 {
		t.Fatalf("墓碑不应计入有效条目，实际 %d", n)
	}

	// 3) 重新收藏 → 复活，deleted_at 必须被置回 NULL
	if err := f.state.SetFavorite(ctx, f.user, id, false, 3000); err != nil {
		t.Fatalf("重新收藏失败: %v", err)
	}
	var raw *int64
	if err := f.db.QueryRowContext(ctx, "SELECT deleted_at FROM user_favorite WHERE article_id=?", id).
		Scan(&raw); err != nil {
		t.Fatalf("直查 deleted_at 失败: %v", err)
	}
	if raw != nil {
		t.Fatalf("重新收藏后 deleted_at 必须被置回 NULL，实际残留墓碑 %d —— 复活失效", *raw)
	}
	if got := f.rows("user_favorite"); got != 1 {
		t.Fatalf("复活不得新增行，实际 %d 行", got)
	}
	if n := f.countActive(FavoriteTable(), f.user); n != 1 {
		t.Fatalf("复活后有效条目应回到 1，实际 %d", n)
	}
	// updated_at 也要跟着推进，否则增量同步推不出「复活」这个事件
	var updatedAt int64
	if err := f.db.QueryRowContext(ctx, "SELECT updated_at FROM user_favorite WHERE article_id=?", id).
		Scan(&updatedAt); err != nil {
		t.Fatalf("直查 updated_at 失败: %v", err)
	}
	if updatedAt != 3000 {
		t.Fatalf("复活时 updated_at 应推进到 3000，实际 %d —— 客户端拉不到「复活」事件", updatedAt)
	}
}

// TestSetRead_墓碑与复活
func TestSetRead_墓碑与复活(t *testing.T) {
	f := newFixture(t)
	id := f.article(311, articleOpts{URL: "https://repo.example.com/f2"})
	ctx := f.ctx

	if err := f.state.SetRead(ctx, f.user, id, false, 1000); err != nil {
		t.Fatalf("标记已读失败: %v", err)
	}
	if n := f.countActive(ReadTable(), f.user); n != 1 {
		t.Fatalf("标记已读后有效条目应为 1，实际 %d", n)
	}
	if err := f.state.SetRead(ctx, f.user, id, true, 2000); err != nil {
		t.Fatalf("标记未读失败: %v", err)
	}
	if got := f.rows("user_read"); got != 1 {
		t.Fatalf("取消已读后应仍是 1 行（墓碑），实际 %d", got)
	}
	if n := f.countActive(ReadTable(), f.user); n != 0 {
		t.Fatalf("墓碑不应计入有效条目，实际 %d", n)
	}
	if err := f.state.SetRead(ctx, f.user, id, false, 3000); err != nil {
		t.Fatalf("重新标记已读失败: %v", err)
	}
	var raw *int64
	if err := f.db.QueryRowContext(ctx, "SELECT deleted_at FROM user_read WHERE article_id=?", id).
		Scan(&raw); err != nil {
		t.Fatalf("直查失败: %v", err)
	}
	if raw != nil {
		t.Fatalf("重新标记已读后 deleted_at 必须回到 NULL，实际 %d", *raw)
	}
	if n := f.countActive(ReadTable(), f.user); n != 1 {
		t.Fatalf("复活后有效条目应回到 1，实际 %d", n)
	}
}

// TestSetFavoriteTx_★必须写在传入的事务里而不是另起连接
//
// ARCHITECTURE.md 的 WAL 事务边界不变量：批量写入时调用方已持有
// BEGIN IMMEDIATE 的全局写锁。若 SetFavoriteTx 内部绕过 tx 从连接池
// 另取连接写，新事务拿不到写锁，而原事务又在等本方法返回才提交 ——
// 互相等待直到 busyTimeout 耗尽，必然 SQLITE_BUSY。
//
// 这条用例用「不 commit 就查不到」证明写入确实落在同一个事务里：
// 若绕过 tx 走r.db，在autocommit 下那行会立刻可见并绕过回滚。
func TestSetFavoriteTx_必须写在传入的事务里而不是另起连接(t *testing.T) {
	f := newFixture(t)
	id := f.article(312, articleOpts{URL: "https://repo.example.com/f3"})
	ctx := f.ctx

	tx, err := f.db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	// tx 必须在整个用例内持锁，不能用 defer Rollback（要显式控制）
	if err := f.state.SetFavoriteTx(ctx, tx, f.user, id, false, 1000); err != nil {
		_ = tx.Rollback()
		t.Fatalf("SetFavoriteTx 失败: %v", err)
	}
	// 同事务内可见（同一连接）
	var inTx int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_favorite WHERE article_id=?", id).
		Scan(&inTx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("事务内查询失败: %v", err)
	}
	if inTx != 1 {
		_ = tx.Rollback()
		t.Fatalf("事务内应看到刚写入的行，实际 %d", inTx)
	}
	// 回滚后必须消失
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if got := f.rows("user_favorite"); got != 0 {
		t.Fatalf("回滚后不得落盘，实际残留 %d 行 —— 写入绕过了传入的事务", got)
	}
}

// TestSetFavoriteTx_★一批多写_整批原子回滚
//
// 批量设置的正确性全靠「整批一个事务」。若某条走r.db（autocommit），
// 后续条目失败时前面的已提交，无法回滚 —— 用户看到部分生效的中间态。
// 这里用「第二条 article_id 不存在触发外键失败」模拟真实故障。
func TestSetFavoriteTx_一批多写_整批原子回滚(t *testing.T) {
	f := newFixture(t)
	good1 := f.article(313, articleOpts{URL: "https://repo.example.com/f4"})
	const missing = int64(999999)
	ctx := f.ctx

	tx, err := f.db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	if err := f.state.SetFavoriteTx(ctx, tx, f.user, good1, false, 1000); err != nil {
		_ = tx.Rollback()
		t.Fatalf("第一条写入失败: %v", err)
	}
	// 外键指向不存在的文章 → 必须报错
	if err := f.state.SetFavoriteTx(ctx, tx, f.user, missing, false, 1001); err == nil {
		_ = tx.Rollback()
		t.Fatal("article_id 不存在时必须被外键拒绝")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if got := f.rows("user_favorite"); got != 0 {
		t.Fatalf("失败批次必须全批回滚，实际残留 %d 行 —— 说明存在逐条 autocommit", got)
	}
}

// TestSetReadTx_写入同一事务并可整体回滚
func TestSetReadTx_写入同一事务并可整体回滚(t *testing.T) {
	f := newFixture(t)
	id := f.article(314, articleOpts{URL: "https://repo.example.com/f5"})
	ctx := f.ctx

	tx, err := f.db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	if err := f.state.SetReadTx(ctx, tx, f.user, id, false, 1000); err != nil {
		_ = tx.Rollback()
		t.Fatalf("SetReadTx 失败: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if got := f.rows("user_read"); got != 0 {
		t.Fatalf("回滚后不得落盘，实际残留 %d 行", got)
	}
}

// TestSetFavorite_跨用户互不干扰
//
// 唯一索引是 (user_id, article_id)，同一篇文章可被多个用户收藏。
// 若索引或 ON CONFLICT 漏掉 user_id，A 的收藏会覆盖 B 的。
func TestSetFavorite_跨用户互不干扰(t *testing.T) {
	f := newFixture(t)
	other := f.mustUser("repo-user-2")
	id := f.article(315, articleOpts{URL: "https://repo.example.com/f6"})
	ctx := f.ctx

	if err := f.state.SetFavorite(ctx, f.user, id, false, 1000); err != nil {
		t.Fatalf("用户 A 收藏失败: %v", err)
	}
	if err := f.state.SetFavorite(ctx, other, id, true, 1000); err != nil {
		t.Fatalf("用户 B 取消收藏失败: %v", err)
	}
	if got := f.rows("user_favorite"); got != 2 {
		t.Fatalf("两个用户应各自一行，实际 %d 行 —— 唯一键漏了 user_id", got)
	}
	a, err := f.state.ListFavoritesByIDs(ctx, f.user, []int64{id})
	if err != nil {
		t.Fatalf("ListFavoritesByIDs 失败: %v", err)
	}
	if len(a) != 1 || a[0].DeletedAt != 0 {
		t.Fatalf("用户 A 的收藏状态被污染: %+v", a)
	}
	b, err := f.state.ListFavoritesByIDs(ctx, other, []int64{id})
	if err != nil {
		t.Fatalf("ListFavoritesByIDs 失败: %v", err)
	}
	if len(b) != 1 || b[0].DeletedAt != 1000 {
		t.Fatalf("用户 B 应为墓碑态: %+v", b)
	}
}

// TestListFavorites_★同时刻多篇_游标分页必须以id兜底
//
// ★ 增量同步的分页正确性全靠 (updated_at, id) 复合游标 + ORDER BY id ASC。
// updated_at 精度只到毫秒，批量设置时几十条同刻写入是常态。
// 若 ORDER BY 少了 `id ASC`，游标翻页会跳过同刻的其余条目，
// 客户端表现为「收藏同步到一半就少了几个」。
//
// 本用例把 5 条收藏**全部设为同一 updated_at**，逐页翻到底，
// 断言每页条数正确、无重复、无丢失。删掉 id ASC 后本用例会红。
func TestListFavorites_同时刻多篇_游标分页必须以id兜底(t *testing.T) {
	f := newFixture(t)
	const sameTS = 5000
	const total = 5
	var inserted []int64
	for i := 0; i < total; i++ {
		id := f.article(320+i, articleOpts{URL: "https://repo.example.com/c" + string(rune('a'+i))})
		inserted = append(inserted, id)
		if err := f.state.SetFavorite(f.ctx, f.user, id, false, sameTS); err != nil {
			t.Fatalf("写入收藏 %d 失败: %v", i, err)
		}
	}

	var (
		got    []int64
		cursor model.Cursor
	)
	for round := 0; round < total+2; round++ {
		items, err := f.state.ListFavorites(f.ctx, f.user, cursor, 2)
		if err != nil {
			t.Fatalf("第 %d 页 ListFavorites 失败: %v", round, err)
		}
		if len(items) == 0 {
			break
		}
		if len(items) > 2 {
			t.Fatalf("第 %d 页返回 %d 条，超过 limit=2", round, len(items))
		}
		got = append(got, favIDs(items)...)
		last := items[len(items)-1]
		cursor = model.Cursor{TS: last.UpdatedAt, ID: last.ID}
	}
	if len(got) != total {
		t.Fatalf("共 %d 条同时刻收藏，分页只取到 %d 条（%v）——排序缺 id 兜底会漏条目",
			total, len(got), got)
	}
	seen := map[int64]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("分页出现重复 article_id=%d（%v）", id, got)
		}
		seen[id] = true
	}
	for _, id := range inserted {
		if !seen[id] {
			t.Fatalf("article_id=%d 在分页中丢失", id)
		}
	}
	// 同时刻必须严格按 id ASC 出
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("同时刻收藏的排序不是 id ASC: %v", got)
		}
	}
}

// TestListFavorites_★必须包含墓碑行
//
// 增量流必须能把「这条要删」推给客户端，客户端据此本地删除。
// 若查询加了 `deleted_at IS NULL` 过滤，客户端会永远留着幽灵收藏，
// 而且再也收不到删除信号（行还在库里，只是不返回了）。
func TestListFavorites_必须包含墓碑行(t *testing.T) {
	f := newFixture(t)
	alive := f.article(330, articleOpts{URL: "https://repo.example.com/alive"})
	dead := f.article(331, articleOpts{URL: "https://repo.example.com/dead"})
	ctx := f.ctx

	if err := f.state.SetFavorite(ctx, f.user, alive, false, 1000); err != nil {
		t.Fatalf("写入有效收藏失败: %v", err)
	}
	if err := f.state.SetFavorite(ctx, f.user, dead, true, 2000); err != nil {
		t.Fatalf("写入墓碑失败: %v", err)
	}

	items, err := f.state.ListFavorites(ctx, f.user, model.ZeroCursor, 100)
	if err != nil {
		t.Fatalf("ListFavorites 失败: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("增量流必须含墓碑行，应返回 2 条，实际 %d 条 —— 墓碑被过滤掉会导致客户端幽灵收藏", len(items))
	}
	var foundDead bool
	for _, it := range items {
		if it.ArticleID == dead {
			foundDead = true
			if it.DeletedAt != 2000 {
				t.Fatalf("墓碑行的 DeletedAt 应为 2000，实际 %d", it.DeletedAt)
			}
		}
	}
	if !foundDead {
		t.Fatal("墓碑行未出现在增量流中")
	}
}

// TestListFavorites_游标只返回水位之后的变更
func TestListFavorites_游标只返回水位之后的变更(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	a1 := f.article(332, articleOpts{URL: "https://repo.example.com/w1"})
	a2 := f.article(333, articleOpts{URL: "https://repo.example.com/w2"})
	a3 := f.article(334, articleOpts{URL: "https://repo.example.com/w3"})
	for i, id := range []int64{a1, a2, a3} {
		now := int64(1000 * (i + 1))
		if err := f.state.SetFavorite(ctx, f.user, id, false, now); err != nil {
			t.Fatalf("写入收藏失败: %v", err)
		}
	}
	// 取第一页并记下游标
	page1, err := f.state.ListFavorites(ctx, f.user, model.ZeroCursor, 1)
	if err != nil {
		t.Fatalf("第一页失败: %v", err)
	}
	if len(page1) != 1 || page1[0].ArticleID != a1 {
		t.Fatalf("第一页应返回最早写入的一条，实际 %+v", page1)
	}
	last := page1[len(page1)-1]
	page2, err := f.state.ListFavorites(ctx, f.user,
		model.Cursor{TS: last.UpdatedAt, ID: last.ID}, 100)
	if err != nil {
		t.Fatalf("第二页失败: %v", err)
	}
	got := favIDs(page2)
	if len(got) != 2 || got[0] != a2 || got[1] != a3 {
		t.Fatalf("第二页应返回 [%d %d]，实际 %v", a2, a3, got)
	}
}

// TestListFavorites_LIMIT边界
func TestListFavorites_LIMIT边界(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	for i := 0; i < 3; i++ {
		f.article(335+i, articleOpts{URL: "https://repo.example.com/l" + string(rune('a'+i))})
	}
	if err := f.state.SetFavorite(ctx, f.user, f.article(340, articleOpts{URL: "https://repo.example.com/lz"}), false, 1000); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	zero, err := f.state.ListFavorites(ctx, f.user, model.ZeroCursor, 0)
	if err != nil {
		t.Fatalf("limit=0 时失败: %v", err)
	}
	if len(zero) != 0 {
		t.Fatalf("limit=0 应返回 0 条，实际 %d", len(zero))
	}
	one, err := f.state.ListFavorites(ctx, f.user, model.ZeroCursor, 1)
	if err != nil {
		t.Fatalf("limit=1 时失败: %v", err)
	}
	if len(one) != 1 {
		t.Fatalf("limit=1 应返回 1 条，实际 %d", len(one))
	}
	all, err := f.state.ListFavorites(ctx, f.user, model.ZeroCursor, 1000)
	if err != nil {
		t.Fatalf("大 limit 时失败: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("实际只有 1 条收藏，应返回 1 条，实际 %d", len(all))
	}
}

// TestListFavorites_只返回该用户的行
func TestListFavorites_只返回该用户的行(t *testing.T) {
	f := newFixture(t)
	other := f.mustUser("repo-user-3")
	ctx := f.ctx
	mine := f.article(341, articleOpts{URL: "https://repo.example.com/m1"})
	theirs := f.article(342, articleOpts{URL: "https://repo.example.com/m2"})
	if err := f.state.SetFavorite(ctx, f.user, mine, false, 1000); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := f.state.SetFavorite(ctx, other, theirs, false, 1000); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	items, err := f.state.ListFavorites(ctx, f.user, model.ZeroCursor, 100)
	if err != nil {
		t.Fatalf("ListFavorites 失败: %v", err)
	}
	if len(items) != 1 || items[0].ArticleID != mine {
		t.Fatalf("只应返回当前用户的收藏，实际 %v", favIDs(items))
	}
}

// TestListReads_游标分页含墓碑且仅限本用户
func TestListReads_游标分页含墓碑且仅限本用户(t *testing.T) {
	f := newFixture(t)
	other := f.mustUser("repo-user-4")
	ctx := f.ctx
	var mine []int64
	for i := 0; i < 3; i++ {
		id := f.article(350+i, articleOpts{URL: "https://repo.example.com/r" + string(rune('a'+i))})
		mine = append(mine, id)
		// 第 2 条设为墓碑
		deleted := int64(0)
		if i == 1 {
			deleted = int64(2000 * (i + 1))
		}
		if _, err := f.state.UpsertRead(ctx, f.db, &model.Read{
			UserID: f.user, ArticleID: id, DeletedAt: deleted,
			CreatedAt: int64(1000 * (i + 1)), UpdatedAt: int64(1000 * (i + 1)),
		}); err != nil {
			t.Fatalf("写入已读失败: %v", err)
		}
	}
	theirs := f.article(360, articleOpts{URL: "https://repo.example.com/rz"})
	if _, err := f.state.UpsertRead(ctx, f.db, &model.Read{
		UserID: other, ArticleID: theirs, CreatedAt: 1000, UpdatedAt: 1000,
	}); err != nil {
		t.Fatalf("写入他人已读失败: %v", err)
	}

	var got []int64
	cursor := model.ZeroCursor
	for round := 0; round < 6; round++ {
		items, err := f.state.ListReads(ctx, f.user, cursor, 2)
		if err != nil {
			t.Fatalf("第 %d 页 ListReads 失败: %v", round, err)
		}
		if len(items) == 0 {
			break
		}
		got = append(got, readIDs(items)...)
		last := items[len(items)-1]
		cursor = model.Cursor{TS: last.UpdatedAt, ID: last.ID}
	}
	if len(got) != 3 {
		t.Fatalf("应取到 3 条（含墓碑、仅本用户），实际 %d 条: %v", len(got), got)
	}
	for _, id := range got {
		if id == theirs {
			t.Fatal("不得返回其他用户的已读记录")
		}
	}
	if got[1] != mine[1] {
		t.Fatalf("墓碑行应出现在增量流中且按 updated_at 排序，期望第 2 位是 %d，实际 %v", mine[1], got)
	}
}

// TestListFavoritesByIDs_★必须含墓碑且按传入ID过滤
//
// 供文章列表附加 isFavorited 用。返回含墓碑是**有意**的：
// 调用方靠 DeletedAt==0 判断是否收藏中，墓碑不能被提前滤掉
// （否则「取消收藏后又重新收藏」的竞态无法正确处理）。
func TestListFavoritesByIDs_必须含墓碑且按传入ID过滤(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	a1 := f.article(370, articleOpts{URL: "https://repo.example.com/b1"})
	a2 := f.article(371, articleOpts{URL: "https://repo.example.com/b2"})
	other := f.article(372, articleOpts{URL: "https://repo.example.com/b3"})
	if err := f.state.SetFavorite(ctx, f.user, a1, false, 1000); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := f.state.SetFavorite(ctx, f.user, a2, true, 2000); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	// 只查 a1/a2，不该带出 other
	got, err := f.state.ListFavoritesByIDs(ctx, f.user, []int64{a1, a2, other})
	if err != nil {
		t.Fatalf("ListFavoritesByIDs 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("只应返回入参中的 2 条，实际 %d 条", len(got))
	}
	byArticle := map[int64]model.Favorite{}
	for _, it := range got {
		byArticle[it.ArticleID] = it
	}
	if byArticle[a1].DeletedAt != 0 {
		t.Fatalf("a1 应为有效态，实际 DeletedAt=%d", byArticle[a1].DeletedAt)
	}
	if byArticle[a2].DeletedAt != 2000 {
		t.Fatalf("a2 应为墓碑态且 DeletedAt=2000，实际 %d —— 墓碑被提前滤掉了", byArticle[a2].DeletedAt)
	}
	// 空入参
	empty, err := f.state.ListFavoritesByIDs(ctx, f.user, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空入参应返回空切片，实际 %v err=%v", empty, err)
	}
}

// TestListReadsByIDs_字段完整映射且含墓碑
func TestListReadsByIDs_字段完整映射且含墓碑(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	a1 := f.article(373, articleOpts{URL: "https://repo.example.com/b4"})
	if err := f.state.SetRead(ctx, f.user, a1, true, 2000); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	got, err := f.state.ListReadsByIDs(ctx, f.user, []int64{a1})
	if err != nil {
		t.Fatalf("ListReadsByIDs 失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应返回 1 条，实际 %d", len(got))
	}
	r := got[0]
	if r.UserID != f.user || r.ArticleID != a1 || r.DeletedAt != 2000 ||
		r.CreatedAt != 2000 || r.UpdatedAt != 2000 || r.ID == 0 {
		t.Fatalf("已读行字段映射错位: %+v", r)
	}
}

// TestCountActive_墓碑不计入
func TestCountActive_墓碑不计入(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	for i := 0; i < 3; i++ {
		f.article(380+i, articleOpts{URL: "https://repo.example.com/n" + string(rune('a'+i))})
	}
	if err := f.state.SetFavorite(ctx, f.user, f.article(385, articleOpts{URL: "https://repo.example.com/n0"}), false, 1000); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := f.state.SetFavorite(ctx, f.user, f.article(386, articleOpts{URL: "https://repo.example.com/n1"}), true, 2000); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := f.state.SetFavorite(ctx, f.user, f.article(387, articleOpts{URL: "https://repo.example.com/n2"}), true, 3000); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	n, err := f.state.CountActive(ctx, f.db, FavoriteTable(), f.user)
	if err != nil {
		t.Fatalf("CountActive 失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("3 条中2 条是墓碑，有效条目应为 1，实际 %d", n)
	}
	// 其他用户不串号
	n2, err := f.state.CountActive(ctx, f.db, FavoriteTable(), 999999)
	if err != nil {
		t.Fatalf("CountActive 失败: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("不存在的用户应有0 条，实际 %d", n2)
	}
}

// TestFavoriteTable与ReadTable_指向不同的表
func TestFavoriteTable与ReadTable_指向不同的表(t *testing.T) {
	fav := FavoriteTable()
	rd := ReadTable()
	if fav == rd {
		t.Fatal("收藏表与已读表元信息不应相同")
	}
	// 用表元信息做一次真实计数，确认两者确实落到不同的表
	f := newFixture(t)
	id := f.article(390, articleOpts{URL: "https://repo.example.com/t1"})
	if err := f.state.SetFavorite(f.ctx, f.user, id, false, 1000); err != nil {
		t.Fatalf("写收藏失败: %v", err)
	}
	if err := f.state.SetRead(f.ctx, f.user, id, false, 1000); err != nil {
		t.Fatalf("写已读失败: %v", err)
	}
	nf, err := f.state.CountActive(f.ctx, f.db, FavoriteTable(), f.user)
	if err != nil {
		t.Fatalf("CountActive(fav) 失败: %v", err)
	}
	nr, err := f.state.CountActive(f.ctx, f.db, ReadTable(), f.user)
	if err != nil {
		t.Fatalf("CountActive(read) 失败: %v", err)
	}
	if nf != 1 || nr != 1 {
		t.Fatalf("两张表各应有 1 条，实际 fav=%d read=%d", nf, nr)
	}
	if f.rows("user_favorite") != 1 || f.rows("user_read") != 1 {
		t.Fatalf("两张表实际行数错误: fav=%d read=%d",
			f.rows("user_favorite"), f.rows("user_read"))
	}
}

// TestFavoriteUpdatedAt_只返回已存在条目的更新时间
func TestFavoriteUpdatedAt_只返回已存在条目的更新时间(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	a1 := f.article(400, articleOpts{URL: "https://repo.example.com/u1"})
	a2 := f.article(401, articleOpts{URL: "https://repo.example.com/u2"})
	if err := f.state.SetFavorite(ctx, f.user, a1, false, 1111); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := f.state.SetFavorite(ctx, f.user, a2, true, 2222); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	tx, err := f.db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	got, err := f.state.FavoriteUpdatedAt(ctx, tx, f.user, []int64{a1, a2, 88888})
	if err != nil {
		t.Fatalf("FavoriteUpdatedAt 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应只返回已存在的 2 条，实际 %d 条: %+v", len(got), got)
	}
	if got[a1] != 1111 || got[a2] != 2222 {
		t.Fatalf("updated_at 错位: %+v", got)
	}
	// 空入参
	empty, err := f.state.FavoriteUpdatedAt(ctx, tx, f.user, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空入参应返回空 map，实际 %v err=%v", empty, err)
	}
}

// TestReadUpdatedAt_只返回已存在条目的更新时间
func TestReadUpdatedAt_只返回已存在条目的更新时间(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	a1 := f.article(402, articleOpts{URL: "https://repo.example.com/u3"})
	if err := f.state.SetRead(ctx, f.user, a1, false, 3333); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	tx, err := f.db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	got, err := f.state.ReadUpdatedAt(ctx, tx, f.user, []int64{a1})
	if err != nil {
		t.Fatalf("ReadUpdatedAt 失败: %v", err)
	}
	if len(got) != 1 || got[a1] != 3333 {
		t.Fatalf("updated_at 错位: %+v", got)
	}
}

// TestExistingUpdatedAt_入参去重且只返回本用户
func TestExistingUpdatedAt_入参去重且只返回本用户(t *testing.T) {
	f := newFixture(t)
	other := f.mustUser("repo-user-5")
	ctx := f.ctx
	a1 := f.article(403, articleOpts{URL: "https://repo.example.com/u4"})
	theirs := f.article(404, articleOpts{URL: "https://repo.example.com/u5"})
	if err := f.state.SetRead(ctx, f.user, a1, false, 4444); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := f.state.SetRead(ctx, other, theirs, false, 5555); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	tx, err := f.db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	got, err := f.state.ExistingUpdatedAt(ctx, tx, ReadTable(), f.user, []int64{a1, a1, theirs})
	if err != nil {
		t.Fatalf("ExistingUpdatedAt 失败: %v", err)
	}
	if len(got) != 1 || got[a1] != 4444 {
		t.Fatalf("应只返回本用户的那一条（重复入参去重），实际 %+v", got)
	}
}

// TestGetPrefs_返回KV与最后更新时间
func TestGetPrefs_返回KV与最后更新时间(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	if kv, ts, err := f.state.GetPrefs(ctx, f.user); err != nil || len(kv) != 0 || ts != 0 {
		t.Fatalf("无偏好时应返回空 map 与 0，实际 kv=%v ts=%d err=%v", kv, ts, err)
	}
	tx, err := f.db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	if err := f.state.ReplacePrefs(ctx, tx, f.user, map[string]string{
		"tts.voice": "zh-CN", "tts.speed": "1.0", "ui.density": "compact",
	}, 7000); err != nil {
		_ = tx.Rollback()
		t.Fatalf("ReplacePrefs 失败: %v", err)
	}
	if err := f.state.ReplacePrefs(ctx, tx, f.user, map[string]string{
		"tts.voice": "en-US", "ui.theme": "dark",
	}, 8000); err != nil {
		_ = tx.Rollback()
		t.Fatalf("二次 ReplacePrefs 失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	kv, ts, err := f.state.GetPrefs(ctx, f.user)
	if err != nil {
		t.Fatalf("GetPrefs 失败: %v", err)
	}
	// 整包覆盖：第二次只留下的键
	if len(kv) != 2 || kv["tts.voice"] != "en-US" || kv["ui.theme"] != "dark" {
		t.Fatalf("ReplacePrefs 必须是整包覆盖，实际 %+v", kv)
	}
	if _, ok := kv["tts.speed"]; ok {
		t.Fatal("整包覆盖后旧键不得残留")
	}
	if ts != 8000 {
		t.Fatalf("应返回最大的 updated_at=8000，实际 %d", ts)
	}
}

// TestGetPrefs_只返回本用户的偏好
func TestGetPrefs_只返回本用户的偏好(t *testing.T) {
	f := newFixture(t)
	other := f.mustUser("repo-user-6")
	ctx := f.ctx
	for _, u := range []int64{f.user, other} {
		tx, err := f.db.BeginWrite(ctx)
		if err != nil {
			t.Fatalf("开启写事务失败: %v", err)
		}
		if err := f.state.ReplacePrefs(ctx, tx, u, map[string]string{"k": "v"}, 1000); err != nil {
			_ = tx.Rollback()
			t.Fatalf("ReplacePrefs 失败: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("提交失败: %v", err)
		}
	}
	kv, _, err := f.state.GetPrefs(ctx, f.user)
	if err != nil {
		t.Fatalf("GetPrefs 失败: %v", err)
	}
	if len(kv) != 1 {
		t.Fatalf("应只返回本用户的 1 条偏好，实际 %d 条: %+v", len(kv), kv)
	}
}

// TestReplacePrefs_空map应清空全部偏好
func TestReplacePrefs_空map应清空全部偏好(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	tx, err := f.db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	if err := f.state.ReplacePrefs(ctx, tx, f.user, map[string]string{"a": "1", "b": "2"}, 1000); err != nil {
		_ = tx.Rollback()
		t.Fatalf("ReplacePrefs 失败: %v", err)
	}
	if err := f.state.ReplacePrefs(ctx, tx, f.user, map[string]string{}, 2000); err != nil {
		_ = tx.Rollback()
		t.Fatalf("ReplacePrefs(空) 失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	kv, _, err := f.state.GetPrefs(ctx, f.user)
	if err != nil {
		t.Fatalf("GetPrefs 失败: %v", err)
	}
	if len(kv) != 0 {
		t.Fatalf("空 map 覆盖应清空全部偏好，实际 %+v", kv)
	}
	if f.rows("user_preference") != 0 {
		t.Fatalf("user_preference 表应被清空，实际 %d 行", f.rows("user_preference"))
	}
}

// TestInsertPrefsIfAbsent_★已存在的键不得被覆盖
//
// 合并时「服务端有值以服务端为准」：已存在的键必须保持原值，
// 只有缺失的键才写入。返回值是实际写入的键数，调用方用它区分
// added 与 merged 的条数。
func TestInsertPrefsIfAbsent_已存在的键不得被覆盖(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	tx, err := f.db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	n, err := f.state.InsertPrefsIfAbsent(ctx, tx, f.user,
		map[string]string{"a": "服务端值", "b": "服务端值"}, 1000)
	if err != nil {
		t.Fatalf("InsertPrefsIfAbsent 失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("首次应写入 2 个键，实际 %d", n)
	}
	// 客户端带着冲突值重放：服务端已有，不应覆盖，且 written=0
	n, err = f.state.InsertPrefsIfAbsent(ctx, tx, f.user,
		map[string]string{"a": "客户端值", "c": "客户端值"}, 2000)
	if err != nil {
		t.Fatalf("二次 InsertPrefsIfAbsent 失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("只有缺失的键 c 应被写入（written=1），实际 %d —— INSERT OR IGNORE 失效", n)
	}
	// GetPrefs 走连接池（r.db），看不到未提交事务 —— 必须先提交再读。
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	kv, _, err := f.state.GetPrefs(ctx, f.user)
	if err != nil {
		t.Fatalf("GetPrefs 失败: %v", err)
	}
	if kv["a"] != "服务端值" {
		t.Fatalf("已存在的键不得被覆盖，实际 a=%q", kv["a"])
	}
	if kv["c"] != "客户端值" {
		t.Fatalf("缺失的键应被写入，实际 c=%q", kv["c"])
	}
}

// TestInsertPrefsIfAbsent_空map写入0个
func TestInsertPrefsIfAbsent_空map写入0个(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	tx, err := f.db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	n, err := f.state.InsertPrefsIfAbsent(ctx, tx, f.user, map[string]string{}, 1000)
	if err != nil || n != 0 {
		t.Fatalf("空 map 应写入 0 个键，实际 n=%d err=%v", n, err)
	}
}

// TestCountPreferences_按用户统计
func TestCountPreferences_按用户统计(t *testing.T) {
	f := newFixture(t)
	other := f.mustUser("repo-user-7")
	ctx := f.ctx
	tx, err := f.db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	if err := f.state.ReplacePrefs(ctx, tx, f.user,
		map[string]string{"a": "1", "b": "2"}, 1000); err != nil {
		_ = tx.Rollback()
		t.Fatalf("ReplacePrefs 失败: %v", err)
	}
	if err := f.state.ReplacePrefs(ctx, tx, other, map[string]string{"c": "3"}, 1000); err != nil {
		_ = tx.Rollback()
		t.Fatalf("ReplacePrefs 失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	n, err := f.state.CountPreferences(ctx, f.db, f.user)
	if err != nil {
		t.Fatalf("CountPreferences 失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("应统计到 2 个键，实际 %d", n)
	}
	n2, err := f.state.CountPreferences(ctx, f.db, 999999)
	if err != nil {
		t.Fatalf("CountPreferences 失败: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("不存在的用户应为 0，实际 %d", n2)
	}
}

// TestMergeLog_写入查询与幂等键
func TestMergeLog_写入查询与幂等键(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	m := &model.MergeLog{
		UserID: f.user, ClientID: "client-1", Nonce: "nonce-1",
		ResultJSON: `{"favoritesAdded":3}`, CreatedAt: 9000,
	}
	tx, err := f.db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	if _, err := f.state.FindMergeLog(ctx, tx, f.user, "client-1", "nonce-1"); !errors.Is(err, ErrNotFound) {
		_ = tx.Rollback()
		t.Fatalf("不存在的合并日志应返回 ErrNotFound，实际 %v", err)
	}
	if err := f.state.InsertMergeLog(ctx, tx, m); err != nil {
		_ = tx.Rollback()
		t.Fatalf("InsertMergeLog 失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	tx2, err := f.db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx2.Rollback() }()
	got, err := f.state.FindMergeLog(ctx, tx2, f.user, "client-1", "nonce-1")
	if err != nil {
		t.Fatalf("FindMergeLog 失败: %v", err)
	}
	if got.ID == 0 || got.ResultJSON != m.ResultJSON || got.CreatedAt != 9000 ||
		got.ClientID != "client-1" || got.Nonce != "nonce-1" || got.UserID != f.user {
		t.Fatalf("合并日志字段错位: %+v", got)
	}
	// 幂等键：同 (user, client, nonce) 再插一次必须被唯一索引拒绝
	if err := f.state.InsertMergeLog(ctx, tx2, m); err == nil {
		t.Fatal("同幂等键二次写入必须被 ux_merge 拒绝")
	}
}

// TestMergeLog_幂等键跨用户互不冲突
func TestMergeLog_幂等键跨用户互不冲突(t *testing.T) {
	f := newFixture(t)
	other := f.mustUser("repo-user-8")
	ctx := f.ctx
	for _, u := range []int64{f.user, other} {
		tx, err := f.db.BeginWrite(ctx)
		if err != nil {
			t.Fatalf("开启写事务失败: %v", err)
		}
		if err := f.state.InsertMergeLog(ctx, tx, &model.MergeLog{
			UserID: u, ClientID: "same-client", Nonce: "same-nonce",
			ResultJSON: "{}", CreatedAt: 1000,
		}); err != nil {
			_ = tx.Rollback()
			t.Fatalf("用户 %d 写入合并日志失败: %v", u, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("提交失败: %v", err)
		}
	}
	if got := f.rows("merge_log"); got != 2 {
		t.Fatalf("两个用户应各自一条，实际 %d 条", got)
	}
}

// TestDeleteMergeLogs_只清理本用户的日志
func TestDeleteMergeLogs_只清理本用户的日志(t *testing.T) {
	f := newFixture(t)
	other := f.mustUser("repo-user-9")
	ctx := f.ctx
	for _, u := range []int64{f.user, other} {
		tx, err := f.db.BeginWrite(ctx)
		if err != nil {
			t.Fatalf("开启写事务失败: %v", err)
		}
		if err := f.state.InsertMergeLog(ctx, tx, &model.MergeLog{
			UserID: u, ClientID: "c", Nonce: "n", ResultJSON: "{}", CreatedAt: 1000,
		}); err != nil {
			_ = tx.Rollback()
			t.Fatalf("写入失败: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("提交失败: %v", err)
		}
	}
	tx, err := f.db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	if err := f.state.DeleteMergeLogs(ctx, tx, f.user); err != nil {
		_ = tx.Rollback()
		t.Fatalf("DeleteMergeLogs 失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if got := f.rows("merge_log"); got != 1 {
		t.Fatalf("只应清理本用户的日志，剩余应为 1 条，实际 %d 条", got)
	}
}

// TestDB_暴露底层句柄且空repo返回ErrNotFound
//
// service 层用它在同一事务内组合多项写操作；
// 零值 repo 必须返回 ErrNotFound 而不是 panic。
func TestDB_暴露底层句柄且空repo返回ErrNotFound(t *testing.T) {
	f := newFixture(t)
	got, err := f.state.DB()
	if err != nil {
		t.Fatalf("DB 失败: %v", err)
	}
	if got == nil || got != f.db {
		t.Fatal("DB 应返回同一个底层句柄")
	}
	var empty *UserStateRepo
	if _, err := empty.DB(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("零值 repo 的 DB 应返回 ErrNotFound，实际 %v", err)
	}
}

// TestUpsertFavorite_外键不存在的用户或文章必须失败
//
// 外键开着，不存在的 user_id / article_id 必须被拒绝 ——
// 这条同时是repo 层外键确实生效的证据（防止将来有人误关PRAGMA）。
func TestUpsertFavorite_外键不存在的用户或文章必须失败(t *testing.T) {
	f := newFixture(t)
	id := f.article(410, articleOpts{URL: "https://repo.example.com/fk"})
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := f.state.UpsertFavorite(f.ctx, tx, &model.Favorite{
		UserID: 999999, ArticleID: id, CreatedAt: 1, UpdatedAt: 1,
	}); err == nil {
		t.Fatal("不存在的 user_id 必须被外键拒绝")
	}
	if _, err := f.state.UpsertFavorite(f.ctx, tx, &model.Favorite{
		UserID: f.user, ArticleID: 999999, CreatedAt: 1, UpdatedAt: 1,
	}); err == nil {
		t.Fatal("不存在的 article_id 必须被外键拒绝")
	}
}

// 编译期断言：UserStateRepo 的写方法必须接受 store.Session。
var _ store.Session = (*store.Tx)(nil)

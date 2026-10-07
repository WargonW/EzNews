package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestDB 开一个带迁移的临时库。
func newTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := Open(Options{Path: path, BusyTimeoutMs: 500})
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(context.Background(), db, true); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}
	return db, path
}

// countRows 统计指定表行数。
func countRows(t *testing.T, d *DB, table string) int {
	t.Helper()
	var n int
	if err := d.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("统计 %s 失败: %v", table, err)
	}
	return n
}

// TestBeginWrite_提交后数据可见
func TestBeginWrite_提交后数据可见(t *testing.T) {
	db, _ := newTestDB(t)
	ctx := context.Background()

	tx, err := db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO source(key,name,url,type,category,is_default,enabled,suggest_interval,language,created_at,updated_at) "+
			"VALUES('t1','测试源','https://t1.example.com','rss','tech',0,1,1800,'zh-CN',1,1)"); err != nil {
		t.Fatalf("事务内写入失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if n := countRows(t, db, "source"); n != 17 { // 16 个种子 + 1
		t.Fatalf("提交后应可见 17 行，实际 %d", n)
	}
}

// TestBeginWrite_回滚后数据确实未落盘
//
// ★ 这是 WAL 事务边界不变量的核心断言：BeginWrite 是真正的
// BEGIN IMMEDIATE 事务，Rollback 必须把这一批写全部撤销。
// 若事务被静默降级成"逐条自动提交"，回滚会变成空操作，
// 半成品数据直接进库——上层却以为已经回滚了。
func TestBeginWrite_回滚后数据确实未落盘(t *testing.T) {
	db, _ := newTestDB(t)
	ctx := context.Background()
	before := countRows(t, db, "source")

	tx, err := db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	// 一批复合写：3 条源 + 1 条用户
	for i := 0; i < 3; i++ {
		key := "rb-" + string(rune('a'+i))
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO source(key,name,url,type,category,is_default,enabled,suggest_interval,language,created_at,updated_at) "+
				"VALUES(?,?,?,'rss','tech',0,1,1800,'zh-CN',1,1)",
			key, key, "https://"+key+".example.com"); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO user(username,password_hash,role,token_version,created_at,updated_at) "+
			"VALUES('rb-user','h','user',1,1,1)"); err != nil {
		t.Fatalf("写入用户失败: %v", err)
	}
	// 事务内必须能看见自己的写入
	var inTx int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM source WHERE key LIKE 'rb-%'").Scan(&inTx); err != nil {
		t.Fatalf("事务内查询失败: %v", err)
	}
	if inTx != 3 {
		t.Fatalf("事务内应看到 3 条，实际 %d", inTx)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	// 全部撤销
	if n := countRows(t, db, "source"); n != before {
		t.Fatalf("回滚后 source 应回到 %d 行，实际 %d", before, n)
	}
	if n := countRows(t, db, "user"); n != 0 {
		t.Fatalf("回滚后不应残留 user，实际 %d 行", n)
	}
	var after int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM source WHERE key LIKE 'rb-%'").Scan(&after); err != nil {
		t.Fatalf("事务外查询失败: %v", err)
	}
	if after != 0 {
		t.Fatalf("回滚后 rb- 开头的源应全部消失，实际还剩 %d", after)
	}
}

// TestTx_重复Rollback与Commit都安全且幂等
//
// ★ Tx.finish 用 sync.Once 保护，重复调用必须安全。
// 业务代码的标准写法是 `defer tx.Rollback()` 后面再显式 Commit，
// 这个模式必然让 Rollback 在 Commit 之后被调用一次。
// 若 finish 不幂等，第二次调用会对已关闭的连接 Exec，返回
// sql.ErrTxDone 之类的垃圾错误，甚至 panic。
func TestTx_重复Rollback与Commit都安全且幂等(t *testing.T) {
	db, _ := newTestDB(t)
	ctx := context.Background()

	t.Run("提交后重复回滚", func(t *testing.T) {
		tx, err := db.BeginWrite(ctx)
		if err != nil {
			t.Fatalf("开启写事务失败: %v", err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO user(username,password_hash,role,token_version,created_at,updated_at) "+
				"VALUES('dup-commit','h','user',1,1,1)"); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("提交失败: %v", err)
		}
		// defer 风格的第二次回滚必须无害，且不得撤销已提交的数据
		for i := 0; i < 3; i++ {
			if err := tx.Rollback(); err != nil {
				t.Fatalf("提交后重复回滚第 %d 次返回错误: %v", i, err)
			}
		}
		if n := countRows(t, db, "user"); n != 1 {
			t.Fatalf("已提交的数据不应被重复回滚抹掉，实际 user %d 行", n)
		}
	})

	t.Run("回滚后重复提交", func(t *testing.T) {
		tx, err := db.BeginWrite(ctx)
		if err != nil {
			t.Fatalf("开启写事务失败: %v", err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO user(username,password_hash,role,token_version,created_at,updated_at) "+
				"VALUES('dup-rollback','h','user',1,1,1)"); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("回滚失败: %v", err)
		}
		for i := 0; i < 3; i++ {
			if err := tx.Commit(); err != nil {
				t.Fatalf("回滚后重复提交第 %d 次返回错误: %v", i, err)
			}
		}
		var n int
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM user WHERE username='dup-rollback'").Scan(&n); err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		if n != 0 {
			t.Fatalf("已回滚的数据不应被重复提交写回，实际 %d 行", n)
		}
	})
}

// TestBeginWrite_并发写事务必须串行化
//
// ★ ARCHITECTURE.md「WAL 事务边界不变量」：写事务经全局互斥锁串行化。
// 这条用例直接验证它——两个 goroutine 各自开事务写不同的 key，
// 断言：① 第二个 BeginWrite 在第一个结束前拿不到锁；
// ② 两笔都成功落盘（没有互相吞掉）。
//
// 正确实现下第二个 BeginWrite 会阻塞在 d.mu.Lock()；
// 若去掉全局锁改用 BEGIN DEFERRED（先读后写），两个事务会同时进入，
// 后写的那笔拿到 SQLITE_BUSY，测试就会红。
func TestBeginWrite_并发写事务必须串行化(t *testing.T) {
	db, _ := newTestDB(t)
	ctx := context.Background()

	firstAcquired := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondAcquired := make(chan struct{})

	go func() {
		tx, err := db.BeginWrite(ctx)
		if err != nil {
			t.Errorf("第一个事务开启失败: %v", err)
			close(firstAcquired)
			return
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO user(username,password_hash,role,token_version,created_at,updated_at) "+
				"VALUES('ser-1','h','user',1,1,1)"); err != nil {
			t.Errorf("第一个事务写入失败: %v", err)
		}
		close(firstAcquired)
		<-releaseFirst
		if err := tx.Commit(); err != nil {
			t.Errorf("第一个事务提交失败: %v", err)
		}
	}()

	<-firstAcquired

	go func() {
		tx, err := db.BeginWrite(ctx)
		if err != nil {
			t.Errorf("第二个事务开启失败: %v", err)
			close(secondAcquired)
			return
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO user(username,password_hash,role,token_version,created_at,updated_at) "+
				"VALUES('ser-2','h','user',1,1,1)"); err != nil {
			t.Errorf("第二个事务写入失败: %v", err)
		}
		close(secondAcquired)
		if err := tx.Commit(); err != nil {
			t.Errorf("第二个事务提交失败: %v", err)
		}
	}()

	// 第一个事务持锁期间，第二个必须拿不到
	select {
	case <-secondAcquired:
		t.Fatal("第二个事务在第一个持锁期间就拿到了写锁（未串行化）")
	case <-time.After(300 * time.Millisecond):
		// 预期：阻塞中
	}

	close(releaseFirst)

	select {
	case <-secondAcquired:
	case <-time.After(10 * time.Second):
		t.Fatal("第一个事务结束后第二个事务应在超时内获得锁，实际像是死锁了")
	}

	// 等第二个 goroutine 跑完写与提交
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countRowsQuiet(db, "user") == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := countRows(t, db, "user"); n != 2 {
		t.Fatalf("两笔并发写都应落盘，实际 user %d 行", n)
	}
}

// countRowsQuiet 不报错地数行数（用于并发等待）。
func countRowsQuiet(d *DB, table string) int {
	var n int
	_ = d.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n)
	return n
}

// TestBeginWrite_事务外直写在事务持锁时快速失败而非死锁
//
// ★ 这是 BEGIN IMMEDIATE + 全局写锁组合的价值：
// 绕过 BeginWrite 的裸写（运维脚本、只读连接误写）会立刻拿到
// SQLITE_BUSY，而不是无限期挂住——挂住意味着请求线程全部堆积、
// 整个服务假死。busy_timeout 兜底后应在超时内返回错误。
//
// 若实现改成 BEGIN DEFERRED，裸写会先成功拿到读锁、
// 事务提交时才升级失败，行为完全不同——这条用例正是用来区分这两者。
func TestBeginWrite_事务外直写在事务持锁时快速失败而非死锁(t *testing.T) {
	db, _ := newTestDB(t)
	ctx := context.Background()

	tx, err := db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO user(username,password_hash,role,token_version,created_at,updated_at) "+
			"VALUES('holder','h','user',1,1,1)"); err != nil {
		t.Fatalf("事务内写入失败: %v", err)
	}

	// 事务外裸写：用独立连接 + 短busy_timeout，避免等满默认 5000ms
	raw, err := Open(Options{Path: mustReopenPath(t, db), BusyTimeoutMs: 200})
	if err != nil {
		t.Fatalf("打开旁路连接失败: %v", err)
	}
	defer func() { _ = raw.Close() }()

	done := make(chan error, 1)
	go func() {
		_, err := raw.ExecContext(ctx,
			"INSERT INTO user(username,password_hash,role,token_version,created_at,updated_at) "+
				"VALUES('intruder','h','user',1,1,1)")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("事务持锁时事务外裸写必须失败")
		}
		// 必须是"库忙"类错误，而不是语法/约束错误
		if !strings.Contains(strings.ToLower(err.Error()), "locked") &&
			!strings.Contains(strings.ToLower(err.Error()), "busy") {
			t.Fatalf("期望 SQLITE_BUSY 类错误，实际 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("事务外裸写应快速失败，实际挂住了（死锁）")
	}
}

// mustReopenPath 从 DB 反查文件路径（测试用）。
func mustReopenPath(t *testing.T, d *DB) string {
	t.Helper()
	var path string
	// SQLite 的 file: DSN 出现在 PRAGMA database_list 里
	if err := d.QueryRowContext(context.Background(),
		"SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		t.Fatalf("反查数据库路径失败: %v", err)
	}
	return path
}

// TestBeginWrite_事务失败后可继续开启新事务
//
// ★ 写锁必须被可靠释放。若 Commit/Rollback 某条路径忘了 release，
// 后续所有 BeginWrite 都会永久阻塞——服务表现为「重启前一直正常，
// 一次异常之后再也不写库」，极难排查。
func TestBeginWrite_事务失败后可继续开启新事务(t *testing.T) {
	db, _ := newTestDB(t)
	ctx := context.Background()

	// 依次制造三种收尾：提交、回滚、执行出错后回滚
	for i, finish := range []string{"commit", "rollback", "error-rollback"} {
		done := make(chan error, 1)
		go func(i int, finish string) {
			tx, err := db.BeginWrite(ctx)
			if err != nil {
				done <- err
				return
			}
			defer func() { _ = tx.Rollback() }()
			if finish == "error-rollback" {
				// 故意用一条语法错误的 SQL，确认错误后仍能正常收尾。
				if _, err := tx.ExecContext(ctx, "SELECT * FROM 不存在的表"); err == nil {
					done <- errNoErrorExpected
					return
				}
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO user(username,password_hash,role,token_version,created_at,updated_at) "+
					"VALUES(?,'h','user',1,1,1)", "lock-test-"+string(rune('a'+i))); err != nil {
				done <- err
				return
			}
			if finish == "commit" {
				done <- tx.Commit()
			} else {
				done <- tx.Rollback()
			}
		}(i, finish)

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("第 %d 轮（%s）失败: %v", i, finish, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("第 %d 轮（%s）后写锁似乎没释放，后续 BeginWrite 永久阻塞", i, finish)
		}
	}

	// 三个正常收尾 + 一次错误收尾里，error-rollback 的写入不该落盘；
	// commit 的那条应该落盘
	if n := countRows(t, db, "user"); n != 1 {
		t.Fatalf("只有 commit 那轮应落盘，实际 user %d 行", n)
	}
}

// errNoErrorExpected 表示"本该报错却没报"。
var errNoErrorExpected = errors.New("本该返回错误但成功了")

// TestTx_并发提交后写锁必须已释放
//
// 用并发压测式地反复开关事务，验证锁不会在某条路径上漏释放。
// 单次顺序调用测不出这类泄漏，必须并发。
func TestTx_并发提交后写锁必须已释放(t *testing.T) {
	db, _ := newTestDB(t)
	ctx := context.Background()

	const workers = 8
	const rounds = 5
	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				tx, err := db.BeginWrite(ctx)
				if err != nil {
					errs <- err
					return
				}
				name := "w" + string(rune('0'+w)) + "-r" + string(rune('0'+r))
				if _, err := tx.ExecContext(ctx,
					"INSERT INTO user(username,password_hash,role,token_version,created_at,updated_at) "+
						"VALUES(?,'h','user',1,1,1)", name); err != nil {
					errs <- err
					_ = tx.Rollback()
					return
				}
				// 一半提交一半回滚，覆盖两条释放路径
				if (w+r)%2 == 0 {
					errs <- tx.Commit()
				} else {
					errs <- tx.Rollback()
				}
			}
		}(w)
	}

	waitDone := make(chan struct{})
	go func() { wg.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(60 * time.Second):
		t.Fatal("并发开关事务超时，疑似写锁泄漏或死锁")
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发事务失败: %v", err)
		}
	}
	// 8 workers × 5 rounds，一半提交 = 20 行
	if n := countRows(t, db, "user"); n != 20 {
		t.Fatalf("应落盘 20 行，实际 %d", n)
	}
	// 关键：全部收尾后仍能正常开新事务（锁确实释放了）
	done := make(chan error, 1)
	go func() {
		tx, err := db.BeginWrite(ctx)
		if err != nil {
			done <- err
			return
		}
		_ = tx.Rollback()
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("全部并发事务结束后仍无法开启新事务: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("并发事务结束后写锁未释放")
	}
}

// TestTx_事务失败时后续语句应报错
//
// 事务内出现 SQL 错误后，SQLite 语义上事务并未自动回滚
// （除少数错误），但实现应让调用方感知。这里只验证一个更实用的点：
// 出错后回滚，数据不会残留。
func TestTx_事务失败时后续语句应报错(t *testing.T) {
	db, _ := newTestDB(t)
	ctx := context.Background()
	before := countRows(t, db, "source")

	tx, err := db.BeginWrite(ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO source(key,name,url,type,category,is_default,enabled,suggest_interval,language,created_at,updated_at) "+
			"VALUES('ok-row','x','https://x.example.com','rss','tech',0,1,1800,'zh-CN',1,1)"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	// 违反唯一约束
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO source(key,name,url,type,category,is_default,enabled,suggest_interval,language,created_at,updated_at) "+
			"VALUES('ok-row','y','https://y.example.com','rss','tech',0,1,1800,'zh-CN',1,1)"); err == nil {
		t.Fatal("重复的 source.key 应被唯一索引拒绝")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if n := countRows(t, db, "source"); n != before {
		t.Fatalf("出错后回滚，数据应完全撤销，期望 %d 行实际 %d", before, n)
	}
}

// TestMigrate_首次迁移后FTS可用
func TestMigrate_首次迁移后FTS可用(t *testing.T) {
	db, _ := newTestDB(t)
	if !db.FTSEnabled() {
		t.Fatal("首次迁移（enableFTS=true）后 FTSEnabled 应为 true")
	}
	// article_fts 表应存在
	var n int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='article_fts'").Scan(&n); err != nil {
		t.Fatalf("检查 article_fts 失败: %v", err)
	}
	if n != 1 {
		t.Fatal("article_fts 表应存在")
	}
}

// ★ TestMigrate_重启后仍应报告FTS可用 —— 这条用例在写这份测试时是红的。
//
// 复现：同一个库文件 Migrate 两次（模拟重启）。
//
//	第 1 次：currentVersion=0 → 全部迁移执行 → 走到 m.Version==3 分支
//	         → SetFTSEnabled(true)。
//	第 2 次：currentVersion=4 → 所有迁移被 `continue` 跳过
//	         → SetFTSEnabled 一次都没被调用 → 保持零值 false。
//
// 后果：cmd/eznews/main.go 把 db.FTSEnabled() 注入 NewArticleService，
// 重启后搜索静默退化为 LIKE——大结果集全表扫描，且短/长关键词行为不一致，
// 但**没有任何报错**，只是一直变慢。
func TestMigrate_重启后仍应报告FTS可用(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	ctx := context.Background()

	db1, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("首次打开失败: %v", err)
	}
	if err := Migrate(ctx, db1, true); err != nil {
		t.Fatalf("首次迁移失败: %v", err)
	}
	if !db1.FTSEnabled() {
		t.Fatal("首次迁移后 FTSEnabled 应为 true")
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	// 模拟重启：重新打开同一个文件再迁移一次
	db2, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("重启后打开失败: %v", err)
	}
	defer func() { _ = db2.Close() }()
	if err := Migrate(ctx, db2, true); err != nil {
		t.Fatalf("重启后迁移失败: %v", err)
	}
	if !db2.FTSEnabled() {
		t.Fatal("重启后 FTSEnabled 必须仍为 true：article_fts 表还在，" +
			"Migrate 却因所有迁移被跳过而没有重新回填这个标志")
	}
	// 表确实还在（证明不是"表没了"，而是标志位丢了）
	var n int
	if err := db2.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='article_fts'").Scan(&n); err != nil {
		t.Fatalf("检查 article_fts 失败: %v", err)
	}
	if n != 1 {
		t.Fatal("article_fts 表应仍然存在")
	}
}

// TestMigrate_关闭FTS时不建表且标志为false
func TestMigrate_关闭FTS时不建表且标志为false(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nofts.db")
	ctx := context.Background()
	db, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := Migrate(ctx, db, false); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if db.FTSEnabled() {
		t.Fatal("enableFTS=false 时 FTSEnabled 必须为 false")
	}
	var n int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='article_fts'").Scan(&n); err != nil {
		t.Fatalf("检查 article_fts 失败: %v", err)
	}
	if n != 0 {
		t.Fatal("enableFTS=false 时不应创建 article_fts 表")
	}
	// 其余表必须照常建好——跳过 FTS 不能影响其他迁移
	for _, tbl := range []string{"source", "article", "user", "session", "user_favorite"} {
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&n); err != nil {
			t.Fatalf("检查 %s 失败: %v", tbl, err)
		}
		if n != 1 {
			t.Fatalf("表 %s 应存在", tbl)
		}
	}
}

// TestMigrate_重复执行幂等
func TestMigrate_重复执行幂等(t *testing.T) {
	db, _ := newTestDB(t)
	ctx := context.Background()
	before := countRows(t, db, "source")
	for i := 0; i < 3; i++ {
		if err := Migrate(ctx, db, true); err != nil {
			t.Fatalf("第 %d 次重复迁移失败: %v", i+2, err)
		}
	}
	if n := countRows(t, db, "source"); n != before {
		t.Fatalf("重复迁移不得改变数据，实际 source %d -> %d", before, n)
	}
}

// TestOpen_参数默认值与错误
func TestOpen_参数默认值与错误(t *testing.T) {
	t.Run("空路径报错", func(t *testing.T) {
		if _, err := Open(Options{Path: "   "}); err == nil {
			t.Fatal("空路径必须报错")
		}
	})
	t.Run("自动创建目录", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "a", "b", "c.db")
		db, err := Open(Options{Path: path})
		if err != nil {
			t.Fatalf("应自动创建父目录: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("关闭失败: %v", err)
		}
	})
	t.Run("Ping可用", func(t *testing.T) {
		db, _ := newTestDB(t)
		if err := db.Ping(context.Background()); err != nil {
			t.Fatalf("Ping 失败: %v", err)
		}
	})
}

// TestBuildDSN_每个连接都要带上全部pragma
//
// ★ _pragma 参数写在 DSN 里，驱动会在**每条连接**建立时应用。
// 若漏掉任何一个（比如只在一个连接上设了 foreign_keys），
// 外键约束在部分连接上不生效，级联删除时行为随机——
// 测试库里能过，生产多连接下偶发脏数据。
func TestBuildDSN_每个连接都要带上全部pragma(t *testing.T) {
	dsn := buildDSN(Options{Path: "x.db", BusyTimeoutMs: 1234, CacheSizeKB: 2048})
	for _, want := range []string{
		"file:x.db",
		"_pragma=busy_timeout(1234)",
		"_pragma=foreign_keys(1)",
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		"_pragma=cache_size(-2048)",
		"_pragma=temp_store(MEMORY)",
		"_pragma=wal_autocheckpoint(1000)",
	} {
		if !strings.Contains(dsn, want) {
			t.Fatalf("DSN 缺少 %q：%s", want, dsn)
		}
	}
	// 路径必须转成正斜杠（Windows 上反斜杠会破坏 file: DSN）
	if got := buildDSN(Options{Path: `C:\a\b.db`}); !strings.Contains(got, "file:C:/a/b.db") {
		t.Fatalf("Windows 路径未转正斜杠: %s", got)
	}
}

// TestOpen_外键在每条连接上都生效
//
// 用超过连接池上限的并发连接数来验证：连接池会开出多条连接，
// 每条都必须有 foreign_keys=1。若只对第一条连接生效，
// 第 2 条起的连接就能插进外键非法的行。
func TestOpen_外键在每条连接上都生效(t *testing.T) {
	db, _ := newTestDB(t)
	ctx := context.Background()

	const conns = 6
	var wg sync.WaitGroup
	errs := make(chan error, conns*2)
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 引用一个不存在的 user_id——有外键时必须失败
			_, err := db.ExecContext(ctx,
				"INSERT INTO session(user_id,refresh_token_hash,client_id,last_seen_at,expires_at,created_at) "+
					"VALUES(?,?,?,1,2,3)", 999999, "fk-hash-"+string(rune('a'+i)), "client")
			if err == nil {
				errs <- errForeignKeyNotEnforced
			} else if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("外键约束未在所有连接上生效: %v", err)
	}
	if n := countRows(t, db, "session"); n != 0 {
		t.Fatalf("不应留下任何 session 行，实际 %d", n)
	}
}

// errForeignKeyNotEnforced 表示外键没生效。
var errForeignKeyNotEnforced = errors.New("外键约束未生效")

// TestOpen_WAL模式已启用
func TestOpen_WAL模式已启用(t *testing.T) {
	db, _ := newTestDB(t)
	var mode string
	if err := db.QueryRowContext(context.Background(),
		"PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("读取 journal_mode 失败: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode 应为 wal，实际 %q", mode)
	}
}

// TestFTSEnabled_默认值为false
//
// 新建库但还没迁移时不该声称 FTS 可用——那会让 ArticleService
// 对着一张不存在的 article_fts 表发查询。
func TestFTSEnabled_默认值为false(t *testing.T) {
	db, err := Open(Options{Path: filepath.Join(t.TempDir(), "raw.db")})
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	defer func() { _ = db.Close() }()
	if db.FTSEnabled() {
		t.Fatal("未迁移的库不应声称 FTS 可用")
	}
	db.SetFTSEnabled(true)
	if !db.FTSEnabled() {
		t.Fatal("SetFTSEnabled(true) 未生效")
	}
	db.SetFTSEnabled(false)
	if db.FTSEnabled() {
		t.Fatal("SetFTSEnabled(false) 未生效")
	}
}

// TestSession_接口由DB与Tx同时满足
//
// 这是 repo 层解耦的基础：所有 repo 方法都只依赖 Session，
// 所以同一段SQL既能跑在事务里也能跑在连接池上。
func TestSession_接口由DB与Tx同时满足(t *testing.T) {
	db, _ := newTestDB(t)
	var _ Session = db
	tx, err := db.BeginWrite(context.Background())
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	var _ Session = tx
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
}

// TestSplitStatements_跳过注释并处理触发器块
//
// 迁移脚本里全是注释与触发器，切分错了会漏建表或建一半。
func TestSplitStatements(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want int
	}{
		{name: "空脚本", sql: "", want: 0},
		{name: "只有注释", sql: "-- 注释\n-- 另一行\n", want: 0},
		{name: "单条", sql: "CREATE TABLE a(id);", want: 1},
		{name: "多条", sql: "CREATE TABLE a(id);\nCREATE TABLE b(id);", want: 2},
		{
			name: "行尾注释不计入",
			sql:  "CREATE TABLE a( -- 列\n  id\n);",
			want: 1,
		},
		{
			name: "触发器整块算一条",
			sql:  "CREATE TRIGGER t AFTER INSERT ON a\nBEGIN\n  UPDATE b SET x=1;\n  UPDATE c SET y=2;\nEND;",
			want: 1,
		},
		{
			name: "触发器后还有语句",
			sql:  "CREATE TRIGGER t AFTER INSERT ON a\nBEGIN\n  UPDATE b SET x=1;\nEND;\nCREATE TABLE z(id);",
			want: 2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitStatements(c.sql)
			if len(got) != c.want {
				t.Fatalf("期望 %d 条，实际 %d 条：%q", c.want, len(got), got)
			}
		})
	}
}

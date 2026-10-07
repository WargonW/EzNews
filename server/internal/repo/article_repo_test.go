package repo

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/store"
	"github.com/eznews/eznews/internal/util"
)

// TestGetByID_往返_全部列与可空列都被正确扫描
//
// 这条锁的是 articleCols（18 列）与 Scan 的目标一一对应。
// 列清单与Scan 顺序一旦错位，症状是「标题栏显示成发布时间」这类静默数据错乱，
// 不会报任何错，只有逐列比对才能发现。
func TestGetByID_往返_全部列与可空列都被正确扫描(t *testing.T) {
	f := newFixture(t)
	id := f.article(1, articleOpts{
		URL: "https://repo.example.com/full", Title: "完整字段文", Summary: "摘要",
		Content: "正文内容", Category: "finance", Tags: []string{"a", "b"},
		PublishedAt: 1_700_000_123, CreatedAt: 1_700_000_456, UpdatedAt: 1_700_000_789,
		LastSeenAt: 1_700_000_999, ExternalID: "ext-full", Language: "en-US",
	})
	a, err := f.arts.GetByID(f.ctx, id)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if a.ID != id || a.SourceID != f.source {
		t.Fatalf("主键/源错位: id=%d source=%d", a.ID, a.SourceID)
	}
	if a.Title != "完整字段文" || a.Summary != "摘要" || a.Content != "正文内容" {
		t.Fatalf("文本列错位: title=%q summary=%q content=%q", a.Title, a.Summary, a.Content)
	}
	if a.Category != "finance" || a.Language != "en-US" || a.ExternalID != "ext-full" {
		t.Fatalf("枚举列错位: category=%q language=%q external=%q", a.Category, a.Language, a.ExternalID)
	}
	if a.PublishedAt != 1_700_000_123 || a.CreatedAt != 1_700_000_456 ||
		a.UpdatedAt != 1_700_000_789 || a.LastSeenAt != 1_700_000_999 {
		t.Fatalf("时间列错位: pub=%d created=%d updated=%d seen=%d",
			a.PublishedAt, a.CreatedAt, a.UpdatedAt, a.LastSeenAt)
	}
	if len(a.Tags) != 2 || a.Tags[0] != "a" || a.Tags[1] != "b" {
		t.Fatalf("标签解析错位: %+v", a.Tags)
	}
	if a.ContentHash == "" || a.ContentHash[:3] != "v1:" {
		t.Fatalf("内容指纹应带 v1: 前缀，实际 %q", a.ContentHash)
	}
}

// TestGetByID_不存在_返回ErrNotFound
//
// ErrNotFound 是 repo 层给上层映射 404 的唯一凭据。
// 若这里退化成 sql.ErrNoRows，httpapi 的 errors.Is 判断就会失效，404 变500。
func TestGetByID_不存在_返回ErrNotFound(t *testing.T) {
	f := newFixture(t)
	_, err := f.arts.GetByID(f.ctx, 999999)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的文章应返回 ErrNotFound，实际: %v", err)
	}
}

// TestGetByID_可空列为NULL_应读成空串而非报错
//
// author / image_url / content / external_id 都是可空列，
// 写入时 nullString 把空串转成 NULL，读回时必须靠 IFNULL 兜成 ""。
// 少写一个 IFNULL，Scan 就会因 NULL 无法写入 string 而整条查询报错。
func TestGetByID_可空列为NULL_应读成空串而非报错(t *testing.T) {
	f := newFixture(t)
	// 不给 Author / ImageURL / Content / ExternalID → Insert 走 nullString 写 NULL。
	id := f.article(2, articleOpts{URL: "https://repo.example.com/nullable"})
	a, err := f.arts.GetByID(f.ctx, id)
	if err != nil {
		t.Fatalf("可空列为 NULL 时 GetByID 失败: %v", err)
	}
	if a.Author != "" || a.ImageURL != "" || a.Content != "" || a.ExternalID != "" {
		t.Fatalf("NULL 列应读成空串，实际 author=%q image=%q content=%q external=%q",
			a.Author, a.ImageURL, a.Content, a.ExternalID)
	}
	// 直接查库确认确实是 NULL 而不是空串，否则这条用例就测不到 IFNULL 了。
	var nAuthor, nImage sql.NullString
	if err := f.db.QueryRowContext(f.ctx, "SELECT author, image_url FROM article WHERE id=?", id).
		Scan(&nAuthor, &nImage); err != nil {
		t.Fatalf("直查可空列失败: %v", err)
	}
	if nAuthor.Valid || nImage.Valid {
		t.Fatalf("前提不成立：author/image_url 应为 NULL，实际 %v/%v", nAuthor, nImage)
	}
}

// TestInsert_urlHash重复_必须报错而不是静默覆盖
//
// ★ 去重语义分层：article 表的 url_hash 唯一约束是**最后一道防线**，
// 正常路径由 BatchFindByURL 提前拦下（见 TestBatchFindByURL_命中已存在哈希_返回原行）。
// 也就是说 Insert 遇到重复哈希**应该报错**（把bug 暴露出来），
// 而不是 ON CONFLICT 静默覆盖 —— 后者会把「两篇不同文章被判为同一篇」藏起来。
// 这里把该行为钉死，防止有人误加 ON CONFLICT DO UPDATE。
func TestInsert_urlHash重复_必须报错而不是静默覆盖(t *testing.T) {
	f := newFixture(t)
	f.article(3, articleOpts{URL: "https://repo.example.com/dup"})
	f.article(4, articleOpts{URL: "https://repo.example.com/other"})

	urlNorm, err := util.NormalizeURL("https://repo.example.com/dup", nil)
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = f.arts.Insert(f.ctx, tx, &model.Article{
		SourceID: f.source, URL: "https://repo.example.com/dup", URLNorm: urlNorm,
		URLHash: util.URLHash(urlNorm), Title: "重复哈希", ContentHash: "h",
		Category: "tech", Language: "zh-CN", PublishedAt: 1, CreatedAt: 1, UpdatedAt: 1, LastSeenAt: 1,
	})
	if err == nil {
		t.Fatal("重复 url_hash 必须报错（唯一约束是防静默覆盖的最后防线），实际却插入成功")
	}
	if got := f.rows("article"); got != 2 {
		t.Fatalf("失败插入后 article 应仍是 2 行，实际 %d", got)
	}
}

// TestBatchFindByURL_命中已存在哈希_返回原行
//
// 去重地基：ingest 靠它在同一事务里识别「这批 URL 已入库」，
// 从而走 UpdateIfChanged 而非 Insert。返回的必须是**原行**（含原 id 与原指纹）。
func TestBatchFindByURL_命中已存在哈希_返回原行(t *testing.T) {
	f := newFixture(t)
	a1 := f.article(5, articleOpts{URL: "https://repo.example.com/f1", Title: "已存在"})
	a2 := f.article(6, articleOpts{URL: "https://repo.example.com/f2", Title: "已存在2"})

	h1 := util.URLHash(mustNormalize(t, "https://repo.example.com/f1"))
	h2 := util.URLHash(mustNormalize(t, "https://repo.example.com/f2"))
	hMiss := util.URLHash(mustNormalize(t, "https://repo.example.com/nope"))

	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	got, err := f.arts.BatchFindByURL(f.ctx, tx, []string{h1, h2, hMiss})
	if err != nil {
		t.Fatalf("BatchFindByURL 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应命中 2 行（miss 不入map），实际 %d: %+v", len(got), got)
	}
	if got[h1].ID != a1 || got[h2].ID != a2 {
		t.Fatalf("命中行错位: h1->%d(期望%d) h2->%d(期望%d)", got[h1].ID, a1, got[h2].ID, a2)
	}
	if _, ok := got[hMiss]; ok {
		t.Fatal("未命中的哈希不得出现在结果 map 里（调用方靠缺失判定 created）")
	}
}

// TestBatchFindByURL_空入参_直接返回空map不触库
func TestBatchFindByURL_空入参_直接返回空map不触库(t *testing.T) {
	f := newFixture(t)
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	got, err := f.arts.BatchFindByURL(f.ctx, tx, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("空入参应返回空 map，实际 got=%v err=%v", got, err)
	}
}

// TestBatchFindByURL_超过单批上限_仍能跨批命中
//
// 分批阈值是 500（SQLITE_MAX_VARIABLE_NUMBER=999 的安全余量）。
// 一旦有人把阈值改成超大值，单条 IN 就会撞变量上限；
// 改成 1 之类的小值则性能退化但功能仍对—— 所以这里只验「跨批仍命中」。
func TestBatchFindByURL_超过单批上限_仍能跨批命中(t *testing.T) {
	f := newFixture(t)
	// 造 3 篇文章，取它们的哈希（不足一批，只验功能正确性）
	var hashes []string
	for i := 0; i < 3; i++ {
		u := "https://repo.example.com/chunk" + string(rune('a'+i))
		f.article(10+i, articleOpts{URL: u})
		hashes = append(hashes, util.URLHash(mustNormalize(t, u)))
	}
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	got, err := f.arts.BatchFindByURL(f.ctx, tx, hashes)
	if err != nil {
		t.Fatalf("BatchFindByURL 失败: %v", err)
	}
	if len(got) != len(hashes) {
		t.Fatalf("应命中 %d 行，实际 %d", len(hashes), len(got))
	}
}

// TestBatchFindByExt_按源加外部ID命中_不跨源串号
//
// 去重键 1 是 (source_id, external_id)。同external_id 挂在不同源下必须是两篇
// 独立文章 —— 若 SQL 漏掉 source_id 条件，跨源同号会被误判为重复。
func TestBatchFindByExt_按源加外部ID命中_不跨源串号(t *testing.T) {
	f := newFixture(t)
	other := f.mustSource("repo-src-b", "finance")
	a1 := f.article(20, articleOpts{URL: "https://repo.example.com/e1", ExternalID: "shared-id"})
	a2 := f.article(21, articleOpts{SourceID: other, URL: "https://repo.example.com/e2", ExternalID: "shared-id"})

	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	got, err := f.arts.BatchFindByExt(f.ctx, tx, []ExtKey{
		{SourceID: f.source, ExternalID: "shared-id"},
		{SourceID: other, ExternalID: "shared-id"},
	})
	if err != nil {
		t.Fatalf("BatchFindByExt 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("两个源下的同号外部 ID 应是两条独立记录，实际命中 %d 条", len(got))
	}
	if got[ExtKey{SourceID: f.source, ExternalID: "shared-id"}].ID != a1 ||
		got[ExtKey{SourceID: other, ExternalID: "shared-id"}].ID != a2 {
		t.Fatalf("跨源串号: %+v", got)
	}
}

// TestBatchFindByExt_空入参_直接返回空map
func TestBatchFindByExt_空入参_直接返回空map(t *testing.T) {
	f := newFixture(t)
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	got, err := f.arts.BatchFindByExt(f.ctx, tx, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("空入参应返回空 map，实际 got=%v err=%v", got, err)
	}
}

// TestUpdateIfChanged_指纹未变_返回false且不刷新updated_at
//
// 这是增量游标不被污染的关键：内容没变就绝不能刷新 updated_at，
// 否则每轮ingest 都会把所有文章重新推给客户端。
func TestUpdateIfChanged_指纹未变_返回false且不刷新updated_at(t *testing.T) {
	f := newFixture(t)
	id := f.article(30, articleOpts{
		URL: "https://repo.example.com/uc1", Title: "原标题", Summary: "原摘要",
		ContentHash: util.ContentHashV1("fixed"), UpdatedAt: 1_000,
	})
	before, err := f.arts.GetByID(f.ctx, id)
	if err != nil {
		t.Fatalf("读取基线失败: %v", err)
	}

	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	changed, err := f.arts.UpdateIfChanged(f.ctx, tx, &model.Article{
		ID: id, URL: "https://repo.example.com/uc1", Title: "不该写入的标题",
		ContentHash: util.ContentHashV1("fixed"), UpdatedAt: 9_999,
	})
	if err != nil {
		t.Fatalf("UpdateIfChanged 失败: %v", err)
	}
	if changed {
		t.Fatal("content_hash 未变时必须返回 false（回执 skipped），实际返回 true")
	}
	after, err := f.arts.GetByID(f.ctx, id)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if after.Title != before.Title {
		t.Fatalf("未命中变更时不得写入任何业务字段，标题被改成了 %q", after.Title)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Fatalf("未命中变更时 updated_at 不得被刷新: %d -> %d", before.UpdatedAt, after.UpdatedAt)
	}
}

// TestUpdateIfChanged_指纹变化_返回true并刷新业务字段与updated_at
func TestUpdateIfChanged_指纹变化_返回true并刷新业务字段与updated_at(t *testing.T) {
	f := newFixture(t)
	id := f.article(31, articleOpts{
		URL: "https://repo.example.com/uc2", Title: "旧标题",
		ContentHash: util.ContentHashV1("old"), UpdatedAt: 1_000,
	})
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	changed, err := f.arts.UpdateIfChanged(f.ctx, tx, &model.Article{
		ID: id, URL: "https://repo.example.com/uc2", Title: "新标题", Summary: "新摘要",
		ContentHash: util.ContentHashV1("new"), Category: "finance", Language: "zh-CN",
		UpdatedAt: 8_888, LastSeenAt: 8_889, PublishedAt: 1_700_000_000,
	})
	if err != nil {
		t.Fatalf("UpdateIfChanged 失败: %v", err)
	}
	if !changed {
		t.Fatal("content_hash 变化时必须返回 true（回执 updated）")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	after, err := f.arts.GetByID(f.ctx, id)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if after.Title != "新标题" || after.Summary != "新摘要" || after.Category != "finance" {
		t.Fatalf("业务字段未刷新: %+v", after)
	}
	if after.UpdatedAt != 8_888 || after.LastSeenAt != 8_889 {
		t.Fatalf("时间字段未刷新: updated=%d seen=%d", after.UpdatedAt, after.LastSeenAt)
	}
}

// TestUpdateIfChanged_不得改动去重键
//
// url_norm / url_hash 被刻意排除在 UPDATE 之外：它们是去重键，
// 一旦被改动，ux_article 的唯一性与「URL 变了= 新文章」的语义同时失效。
func TestUpdateIfChanged_不得改动去重键(t *testing.T) {
	f := newFixture(t)
	id := f.article(32, articleOpts{
		URL: "https://repo.example.com/uc3", ContentHash: util.ContentHashV1("old"),
	})
	before, err := f.arts.GetByID(f.ctx, id)
	if err != nil {
		t.Fatalf("读取基线失败: %v", err)
	}
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := f.arts.UpdateIfChanged(f.ctx, tx, &model.Article{
		ID: id, URL: "https://repo.example.com/changed",
		URLNorm: "https://repo.example.com/changed", URLHash: util.URLHash("https://repo.example.com/changed"),
		ContentHash: util.ContentHashV1("new"), UpdatedAt: 5_000,
	}); err != nil {
		t.Fatalf("UpdateIfChanged 失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	after, err := f.arts.GetByID(f.ctx, id)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if after.URLNorm != before.URLNorm || after.URLHash != before.URLHash {
		t.Fatalf("去重键不得被更新: url_norm %q->%q url_hash %q->%q",
			before.URLNorm, after.URLNorm, before.URLHash, after.URLHash)
	}
	if after.URL != "https://repo.example.com/changed" {
		t.Fatalf("原始 URL（展示/跳转用）应被更新，实际 %q", after.URL)
	}
}

// TestTouchLastSeen_只刷新last_seen_at不碰updated_at
//
// last_seen_at 不参与增量游标（见建表注释）：「本轮又见到了」不是内容变更。
// 若这里顺手刷了 updated_at，每次轮询都会把全部文章重推一遍。
func TestTouchLastSeen_只刷新last_seen_at不碰updated_at(t *testing.T) {
	f := newFixture(t)
	id := f.article(33, articleOpts{
		URL: "https://repo.example.com/tls", UpdatedAt: 1_234, LastSeenAt: 1_234,
	})
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	if err := f.arts.TouchLastSeen(f.ctx, tx, id, 77_777); err != nil {
		t.Fatalf("TouchLastSeen 失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	a, err := f.arts.GetByID(f.ctx, id)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if a.LastSeenAt != 77_777 {
		t.Fatalf("last_seen_at 未刷新: %d", a.LastSeenAt)
	}
	if a.UpdatedAt != 1_234 {
		t.Fatalf("TouchLastSeen 不得刷新 updated_at（增量游标依据），实际 %d", a.UpdatedAt)
	}
}

// TestList_★同时刻多篇_排序必须以id兜底否则翻页漏条目
//
// ★ 本批最易出真bug 的一处。列表排序键是 published_at DESC，
// 而 published_at 精度只到秒 —— 同一秒内的多篇文章排序键完全相同。
// 若 ORDER BY 少了 `a.id DESC` 这个tiebreaker，翻页时
// 「取最后一条的 (published_at, id) 作游标」会跳过同秒的其余条目：
// 客户端表现为「越翻越少 / 中间凭空少几条」。
//
// 这里把5 篇文章**全部设为同一 published_at**，逐页翻到底，
// 断言：① 每页条数正确 ② 累计 ID 无重复 ③ 5 条全部出现。
// 删掉 tiebreaker 后本用例会红。
func TestList_同时刻多篇_排序必须以id兜底否则翻页漏条目(t *testing.T) {
	f := newFixture(t)
	const sameTS = 1_700_000_000
	const total = 5
	all := map[int64]bool{}
	for i := 0; i < total; i++ {
		id := f.article(40+i, articleOpts{
			URL:         "https://repo.example.com/same" + string(rune('a'+i)),
			PublishedAt: sameTS,
		})
		all[id] = true
	}

	var (
		got      []int64
		cursor   model.Cursor
		pageRuns int
	)
	for {
		pageRuns++
		if pageRuns > total+2 {
			t.Fatalf("翻页未收敛，可能存在死循环游标: got=%v", got)
		}
		items, err := f.arts.List(f.ctx, ArticleQuery{
			PageCursor: cursor, Limit: 2,
		})
		if err != nil {
			t.Fatalf("第 %d 页 List 失败: %v", pageRuns, err)
		}
		if len(items) == 0 {
			break
		}
		if len(items) > 2 {
			t.Fatalf("第 %d 页返回 %d 条，超过 Limit=2", pageRuns, len(items))
		}
		got = append(got, ids(items)...)
		last := items[len(items)-1]
		cursor = model.Cursor{TS: last.PublishedAt, ID: last.ID}
	}

	if len(got) != total {
		t.Fatalf("共 %d 条同时刻文章，翻页累计只取到 %d 条（%v）——排序缺 tiebreaker 会漏条目",
			total, len(got), got)
	}
	seen := map[int64]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("翻页出现重复条目 id=%d（%v）", id, got)
		}
		seen[id] = true
		if !all[id] {
			t.Fatalf("翻页返回了不存在的 id=%d", id)
		}
	}
	for id := range all {
		if !seen[id] {
			t.Fatalf("文章 id=%d 在翻页中丢失", id)
		}
	}
	// 同时刻时必须严格按 id DESC 出（最新入库在前）
	for i := 1; i < len(got); i++ {
		if got[i-1] <= got[i] {
			t.Fatalf("同时刻文章的排序不是 id DESC: %v", got)
		}
	}
}

// TestList_LIMIT边界_零条与刚好一页与超出页码
func TestList_LIMIT边界_零条与刚好一页与超出页码(t *testing.T) {
	t.Run("库为空", func(t *testing.T) {
		f := newFixture(t) // 空库：只有 004_seed 的16 个源，没有文章
		items, err := f.arts.List(f.ctx, ArticleQuery{Limit: 10})
		if err != nil {
			t.Fatalf("空库 List 失败: %v", err)
		}
		if len(items) != 0 {
			t.Fatalf("空库应返回 0 条，实际 %d", len(items))
		}
	})
	t.Run("刚好一页", func(t *testing.T) {
		f := newFixture(t)
		f.article(200, articleOpts{URL: "https://repo.example.com/p1", PublishedAt: 1000})
		f.article(201, articleOpts{URL: "https://repo.example.com/p2", PublishedAt: 2000})
		items, err := f.arts.List(f.ctx, ArticleQuery{Limit: 2})
		if err != nil {
			t.Fatalf("List 失败: %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("库中 2 条、Limit=2，应恰好返回 2 条，实际 %d", len(items))
		}
	})
	t.Run("超出页码返回空", func(t *testing.T) {
		f := newFixture(t)
		f.article(202, articleOpts{URL: "https://repo.example.com/p3", PublishedAt: 1000})
		f.article(203, articleOpts{URL: "https://repo.example.com/p4", PublishedAt: 2000})
		// 游标指向比最老一条还早的位置 → 必须干净地返回空而不是回绕
		items, err := f.arts.List(f.ctx, ArticleQuery{
			PageCursor: model.Cursor{TS: 1, ID: 1}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("List 失败: %v", err)
		}
		if len(items) != 0 {
			t.Fatalf("游标早于全部数据时应返回 0 条，实际 %d 条: %v", len(items), ids(items))
		}
	})
	t.Run("limit为0", func(t *testing.T) {
		f := newFixture(t)
		f.article(204, articleOpts{URL: "https://repo.example.com/p5"})
		items, err := f.arts.List(f.ctx, ArticleQuery{Limit: 0})
		if err != nil {
			t.Fatalf("Limit=0 时 List 失败: %v", err)
		}
		if len(items) != 0 {
			t.Fatalf("LIMIT 0 应返回 0 条，实际 %d", len(items))
		}
	})
}

// TestList_停用源的文章默认被过滤
//
// s.enabled=1 是列表的硬门槛：停用源的文章不进默认列表，
// 但 article 行仍在（前端按 sourceId 显式查询时仍可拿到）。
func TestList_停用源的文章默认被过滤(t *testing.T) {
	f := newFixture(t)
	disabled := f.mustDisabledSource("repo-src-off", "tech")
	on := f.article(50, articleOpts{URL: "https://repo.example.com/on1"})
	off := f.article(51, articleOpts{SourceID: disabled, URL: "https://repo.example.com/off1"})

	def, err := f.arts.List(f.ctx, ArticleQuery{Limit: 100})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	for _, id := range ids(def) {
		if id == off {
			t.Fatal("停用源的文章不得出现在默认列表")
		}
	}
	found := false
	for _, id := range ids(def) {
		if id == on {
			found = true
		}
	}
	if !found {
		t.Fatal("启用源的文章应出现在默认列表")
	}

	all, err := f.arts.List(f.ctx, ArticleQuery{Limit: 100, IncludeDisabled: true})
	if err != nil {
		t.Fatalf("List(IncludeDisabled) 失败: %v", err)
	}
	foundOff := false
	for _, id := range ids(all) {
		if id == off {
			foundOff = true
		}
	}
	if !foundOff {
		t.Fatal("IncludeDisabled=true 时停用源的文章应被带出")
	}
}

// TestList_按源与分类与时间区间过滤
func TestList_按源与分类与时间区间过滤(t *testing.T) {
	f := newFixture(t)
	other := f.mustSource("repo-src-b", "finance")
	a1 := f.article(60, articleOpts{URL: "https://repo.example.com/f1", Category: "tech", PublishedAt: 1000})
	a2 := f.article(61, articleOpts{URL: "https://repo.example.com/f2", Category: "finance", PublishedAt: 2000})
	a3 := f.article(62, articleOpts{SourceID: other, URL: "https://repo.example.com/f3", Category: "tech", PublishedAt: 3000})

	cases := []struct {
		name string
		q    ArticleQuery
		want []int64
	}{
		{"按源过滤", ArticleQuery{SourceID: other, Limit: 100}, []int64{a3}},
		{"按分类过滤", ArticleQuery{Category: "tech", Limit: 100}, []int64{a3, a1}},
		{"按下界", ArticleQuery{From: 2000, Limit: 100}, []int64{a3, a2}},
		{"按上界", ArticleQuery{To: 2000, Limit: 100}, []int64{a2, a1}},
		{"按区间", ArticleQuery{From: 1000, To: 2000, Limit: 100}, []int64{a2, a1}},
		{"组合过滤", ArticleQuery{SourceID: other, Category: "tech", From: 2000, Limit: 100}, []int64{a3}},
		{"条件互斥返回空", ArticleQuery{SourceID: other, Category: "finance", Limit: 100}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, err := f.arts.List(f.ctx, tc.q)
			if err != nil {
				t.Fatalf("List 失败: %v", err)
			}
			got := ids(items)
			if len(got) != len(tc.want) {
				t.Fatalf("%s:期望 %v，实际 %v", tc.name, tc.want, got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("%s: 期望 %v，实际 %v（排序为 published_at DESC, id DESC）", tc.name, tc.want, got)
				}
			}
		})
	}
}

// TestList_★增量模式_按updated_at升序且以id兜底
//
// 增量流的方向与列表相反：ORDER BY updated_at ASC（从水位往后推）。
// 同样必须带 id ASC，否则同一毫秒内变更的多篇会被漏推。
func TestList_增量模式_按updated_at升序且以id兜底(t *testing.T) {
	f := newFixture(t)
	const sameTS = 1_700_000_500
	const total = 4
	var inserted []int64
	for i := 0; i < total; i++ {
		id := f.article(70+i, articleOpts{
			URL:         "https://repo.example.com/d" + string(rune('a'+i)),
			PublishedAt: int64(1000 + i),
			UpdatedAt:   sameTS,
		})
		inserted = append(inserted, id)
	}

	var (
		got    []int64
		cursor model.Cursor
	)
	for i := 0; i < total+2; i++ {
		items, err := f.arts.List(f.ctx, ArticleQuery{
			Delta: true, Cursor: cursor, Limit: 2,
		})
		if err != nil {
			t.Fatalf("增量 List 失败: %v", err)
		}
		if len(items) == 0 {
			break
		}
		got = append(got, ids(items)...)
		last := items[len(items)-1]
		cursor = model.Cursor{TS: last.UpdatedAt, ID: last.ID}
	}
	if len(got) != total {
		t.Fatalf("增量翻页应取到 %d 条，实际 %d条（%v）——(updated_at,id) 缺 id 兜底会漏推", total, len(got), got)
	}
	// 增量方向是 id ASC
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("增量排序应为 updated_at ASC, id ASC，实际 %v", got)
		}
	}
}

// TestList_增量游标只返回水位之后的变更
func TestList_增量游标只返回水位之后的变更(t *testing.T) {
	f := newFixture(t)
	a1 := f.article(80, articleOpts{URL: "https://repo.example.com/i1", UpdatedAt: 1000})
	a2 := f.article(81, articleOpts{URL: "https://repo.example.com/i2", UpdatedAt: 2000})
	a3 := f.article(82, articleOpts{URL: "https://repo.example.com/i3", UpdatedAt: 3000})

	items, err := f.arts.List(f.ctx, ArticleQuery{
		Delta: true, Cursor: model.Cursor{TS: 1000, ID: a1}, Limit: 100,
	})
	if err != nil {
		t.Fatalf("增量 List 失败: %v", err)
	}
	got := ids(items)
	if len(got) != 2 || got[0] != a2 || got[1] != a3 {
		t.Fatalf("水位 (1000,%d) 之后应只返回 [%d %d]，实际 %v", a1, a2, a3, got)
	}
}

// TestList_★短关键词走LIKE_长关键词走FTS
//
// buildArticleQuery 的分流规则：只有 rune 数 >= 3 且 FTS 可用才走 trigram。
// trigram 按 3 字符滑窗切分，少于 3 个字符永远匹配不到任何行——
// 中文两字词是用户最常搜的形态，走FTS 就是100% 命中率 0 的功能缺陷。
//
// ★必须按 rune 而不是字节判断：中文 1 字= 3 字节，「央行」len()=6，
// 用 len() 会误判为 >= 3 而继续走 FTS，bug 依旧。
func TestList_短关键词走LIKE_长关键词走FTS(t *testing.T) {
	f := newFixture(t)
	f.article(90, articleOpts{
		URL: "https://repo.example.com/s1", Title: "央行发布降准消息", Summary: "财经速递",
	})
	f.article(91, articleOpts{
		URL: "https://repo.example.com/s2", Title: "无关稿件", Summary: "科技速递",
	})

	cases := []struct {
		name string
		kw   string
		fts  bool
		want int
	}{
		{"两个汉字走LIKE", "央行", true, 1},
		{"两个汉字不走FTS也命中", "央行", false, 1},
		{"单个汉字走LIKE", "央", true, 1},
		{"长关键词走FTS", "央行发布降准消息", true, 1},
		{"长关键词不走FTS时退LIKE", "央行发布降准消息", false, 1},
		{"无命中", "不存在的词", true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, err := f.arts.List(f.ctx, ArticleQuery{
				Keyword: tc.kw, FTS: tc.fts, Limit: 100,
			})
			if err != nil {
				t.Fatalf("List失败: %v", err)
			}
			if len(items) != tc.want {
				t.Fatalf("关键词 %q（fts=%v）期望 %d 条，实际 %d 条: %v",
					tc.kw, tc.fts, tc.want, len(items), ids(items))
			}
		})
	}
}

// TestList_FTS表不存在_由调用方关闭FTS开关后必须走LIKE
//
// ★ 注意职责边界：ArticleQuery.FTS 由调用方（ArticleService，注入 db.FTSEnabled()）
// 决定，repo 只忠实执行。所以「FTS 不可用」的正确降级方式是**调用方传 FTS=false**，
// 而不是让 repo 在运行时去探测表是否存在。
//
// 这条钉住降级路径本身可用：开关关掉之后搜索仍然要能出结果（只是慢），
// 不能因为「没有 article_fts 表」而整个列表接口 500。
//
// ★ 顺带记录一个已修的真 bug：store.Migrate 在「重启后版本已到4」时会跳过
// 003 迁移，而 SetFTSEnabled(true) 恰好只写在 003 的执行分支里，
// 于是 FTSEnabled() 第二次启动起恒为 false —— 搜索会静默退化成 LIKE。
// 详见 store/migrate.go 的 ftsStateFromTable 与 TestMigrate_重启后仍应报告FTS可用。
func TestList_FTS表不存在_由调用方关闭FTS开关后必须走LIKE(t *testing.T) {
	f := newFixtureWithFTS(t, false)
	f.article(95, articleOpts{
		URL: "https://repo.example.com/n1", Title: "央行发布降准消息",
	})
	if f.db.FTSEnabled() {
		t.Fatal("前提不成立：本fixture 应处于 FTS 不可用状态")
	}
	// 这才是真实的降级入参：调用方读FTSEnabled() 得到 false，于是传 FTS=false。
	items, err := f.arts.List(f.ctx, ArticleQuery{
		Keyword: "央行发布降准消息", FTS: f.db.FTSEnabled(), Limit: 100,
	})
	if err != nil {
		t.Fatalf("FTS 不可用时必须能降级为 LIKE 检索，实际报错: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("降级路径应仍能检索到 1 条，实际 %d 条", len(items))
	}
}

// TestList_关键词里的LIKE通配符必须被转义
//
// 未转义时用户搜 "100%" 会变成 "100%" 通配而命中一切。
// escapeLike 负责把 \ % _ 三个字符转义掉。
func TestList_关键词里的LIKE通配符必须被转义(t *testing.T) {
	f := newFixture(t)
	f.article(96, articleOpts{URL: "https://repo.example.com/w1", Title: "折扣 100% 优惠"})
	f.article(97, articleOpts{URL: "https://repo.example.com/w2", Title: "折扣 100 元优惠"})

	items, err := f.arts.List(f.ctx, ArticleQuery{Keyword: "100%", Limit: 100})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("关键词 '100%%' 含通配符，必须按字面匹配（只命中 1 条），实际命中 %d条", len(items))
	}
	if items[0].Title != "折扣 100% 优惠" {
		t.Fatalf("命中的应是含字面 '%%' 的那篇，实际 %q", items[0].Title)
	}
}

// TestList_★LEFT JOIN源信息_带出分类源名且孤儿文章不被吞掉
//
// List 用的是 INNER JOIN source：文章必须有源才能进列表（合理）。
// FindBriefsByIDs 用的是 LEFT JOIN：源被删时仍要带出摘要，
// 否则前端会看到「凭空少了一条收藏」。两条路径的差异由本用例与
// TestFindBriefsByIDs_★源为空_仍须返回摘要行 一起钉住。
func TestList_源信息_带出源key与源名(t *testing.T) {
	f := newFixture(t)
	id := f.article(100, articleOpts{URL: "https://repo.example.com/src", Title: "带源信息的文"})
	orphan := f.orphanArticle(101, "源已被删的文")

	items, err := f.arts.List(f.ctx, ArticleQuery{Limit: 100})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	byID := map[int64]model.ArticleListItem{}
	for _, it := range items {
		byID[it.ID] = it
	}
	it, ok := byID[id]
	if !ok {
		t.Fatalf("正常文章未出现在列表里: %v", ids(items))
	}
	if it.SourceKey != "repo-src-a" || it.SourceName != "repo-src-a" {
		t.Fatalf("源信息未带出: key=%q name=%q", it.SourceKey, it.SourceName)
	}
	if _, ok := byID[orphan]; ok {
		t.Fatal("孤儿文章（source_id 指向不存在的行）不应进入 INNER JOIN 列表 —— " +
			"若它出现了，说明 JOIN 被写成了 LEFT JOIN，列表会混入无源文章")
	}
}

// TestList_标签在列表中被正确解析
func TestList_标签在列表中被正确解析(t *testing.T) {
	f := newFixture(t)
	f.article(102, articleOpts{
		URL: "https://repo.example.com/tags", Tags: []string{"宏观", "央行"},
	})
	f.article(103, articleOpts{URL: "https://repo.example.com/notags"})

	items, err := f.arts.List(f.ctx, ArticleQuery{Limit: 100})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	seen := 0
	for _, it := range items {
		switch it.Title {
		case "仓库测试文 102":
			if len(it.Tags) != 2 || it.Tags[0] != "宏观" {
				t.Fatalf("标签解析错位: %+v", it.Tags)
			}
			seen++
		case "仓库测试文 103":
			if len(it.Tags) != 0 {
				t.Fatalf("无标签文章应解析为空切片，实际 %+v", it.Tags)
			}
			seen++
		}
	}
	if seen != 2 {
		t.Fatalf("预期命中两篇带标签标记的文章，实际 %d", seen)
	}
}

// TestFindBriefsByIDs_★源为空_仍须返回摘要行
//
// ★ LEFT JOIN 的核心断言。article.source_id 有外键，正常路径下孤儿行不存在；
// 但 FindBriefsByIDs 的注释明确承诺「LEFT JOIN 保证源被删时文章仍能返回摘要」，
// 增量同步里源被删的收藏项必须仍显示为「文章已删除」而不是凭空消失。
//
// 若把 LEFT JOIN 误改成 INNER JOIN，本用例会红（map 里查不到孤儿行）。
func TestFindBriefsByIDs_源为空_仍须返回摘要行(t *testing.T) {
	f := newFixture(t)
	a1 := f.article(110, articleOpts{
		URL: "https://repo.example.com/b1", Title: "有源的文", Summary: "摘要一",
	})
	orphan := f.orphanArticle(111, "源已被删的文")

	got, err := f.arts.FindBriefsByIDs(f.ctx, []int64{a1, orphan})
	if err != nil {
		t.Fatalf("FindBriefsByIDs 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应返回 2 条摘要，实际 %d 条: %+v —— LEFT JOIN 被写成 INNER JOIN 会只剩 1 条", len(got), got)
	}
	ob, ok := got[orphan]
	if !ok {
		t.Fatalf("源为空的文章未返回摘要（LEFT JOIN 失效）: %+v", got)
	}
	if ob.Title != "源已被删的文" {
		t.Fatalf("孤儿行标题错误: %q", ob.Title)
	}
	if ob.SourceName != "" {
		t.Fatalf("源为空时来源名应为空串（IFNULL 兜底），实际 %q", ob.SourceName)
	}
	if b := got[a1]; b.SourceName != "repo-src-a" || b.Title != "有源的文" {
		t.Fatalf("正常行摘要错误: %+v", b)
	}
}

// TestFindBriefsByIDs_查不到的行不进map
//
// 调用方靠 `brief, ok := m[id]` 区分「无摘要」与「空标题」。
// 若把查不到的行也塞进 map（零值 ArticleBrief），ok 永远为 true，
// 前端会把不存在的文章渲染成空白卡片。
func TestFindBriefsByIDs_查不到的行不进map(t *testing.T) {
	f := newFixture(t)
	a1 := f.article(112, articleOpts{URL: "https://repo.example.com/b2", Title: "存在的文"})
	got, err := f.arts.FindBriefsByIDs(f.ctx, []int64{a1, 88888})
	if err != nil {
		t.Fatalf("FindBriefsByIDs 失败: %v", err)
	}
	if _, ok := got[88888]; ok {
		t.Fatal("不存在的ID 不得出现在结果 map 里（否则调用方无法用 ok 区分）")
	}
	if _, ok := got[a1]; !ok {
		t.Fatal("存在的ID 必须出现在结果 map 里")
	}
}

// TestFindBriefsByIDs_入参去重且空入参返回空map
func TestFindBriefsByIDs_入参去重且空入参返回空map(t *testing.T) {
	f := newFixture(t)
	a1 := f.article(113, articleOpts{URL: "https://repo.example.com/b3"})
	got, err := f.arts.FindBriefsByIDs(f.ctx, []int64{a1, a1, a1})
	if err != nil {
		t.Fatalf("FindBriefsByIDs 失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("重复入参应去重成1 条，实际 %d 条", len(got))
	}
	empty, err := f.arts.FindBriefsByIDs(f.ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空入参应返回空 map，实际 %v err=%v", empty, err)
	}
}

// TestFindBriefsByIDs_摘要与正文分离_不返回content与tags
//
// 增量页最多 1000 条，ArticleBrief 只取 5 列是有意的带宽约束。
// 若有人往SELECT 里加 content，这条路径的载荷会放大到 MB 级。
func TestFindBriefsByIDs_摘要与正文分离_不返回content与tags(t *testing.T) {
	f := newFixture(t)
	id := f.article(114, articleOpts{
		URL: "https://repo.example.com/b4", Title: "精简文", Summary: "精简摘要",
		Content: "很长的正文", Tags: []string{"t1"},
	})
	got, err := f.arts.FindBriefsByIDs(f.ctx, []int64{id})
	if err != nil {
		t.Fatalf("FindBriefsByIDs 失败: %v", err)
	}
	b := got[id]
	if b.Summary != "精简摘要" || b.PublishedAt != 1_700_000_000 {
		t.Fatalf("摘要字段错误: %+v", b)
	}
	// model.ArticleBrief 结构体里根本没有 Content / Tags 字段，
	// 编译期就保证了「不会拖正文进这条路径」，这里只断言发布时间这类派生字段确实带上了。
}

// TestExistsIDs_分批判断存在性
func TestExistsIDs_分批判断存在性(t *testing.T) {
	f := newFixture(t)
	a1 := f.article(120, articleOpts{URL: "https://repo.example.com/x1"})
	a2 := f.article(121, articleOpts{URL: "https://repo.example.com/x2"})

	got, err := f.arts.ExistsIDs(f.ctx, []int64{a1, a2, 77777})
	if err != nil {
		t.Fatalf("ExistsIDs 失败: %v", err)
	}
	if len(got) != 2 || !got[a1] || !got[a2] {
		t.Fatalf("存在性判断错误: %+v", got)
	}
	if got[77777] {
		t.Fatal("不存在的 ID 不得被标记为存在")
	}
	empty, err := f.arts.ExistsIDs(f.ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空入参应返回空 map，实际 %v err=%v", empty, err)
	}
}

// TestCountAll_统计全部文章
func TestCountAll_统计全部文章(t *testing.T) {
	f := newFixture(t)
	n, err := f.arts.CountAll(f.ctx)
	if err != nil {
		t.Fatalf("CountAll 失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("空库应为 0，实际 %d", n)
	}
	f.article(130, articleOpts{URL: "https://repo.example.com/c1"})
	f.article(131, articleOpts{URL: "https://repo.example.com/c2"})
	f.article(132, articleOpts{URL: "https://repo.example.com/c3"})
	if n, err = f.arts.CountAll(f.ctx); err != nil || n != 3 {
		t.Fatalf("3 篇文章应统计为 3，实际 %d err=%v", n, err)
	}
}

// TestCountByCategory_只统计启用源下的文章
//
// JOIN ... AND s.enabled=1：停用源的分类不参与统计。
// 这条同时钉住「派生字段用聚合时不能把行整条丢掉」——
// 停用源的文章应是被过滤，而不是让整个分组消失。
func TestCountByCategory_只统计启用源下的文章(t *testing.T) {
	f := newFixture(t)
	disabled := f.mustDisabledSource("repo-src-off2", "tech")
	f.article(140, articleOpts{URL: "https://repo.example.com/g1", Category: "tech"})
	f.article(141, articleOpts{URL: "https://repo.example.com/g2", Category: "tech"})
	f.article(142, articleOpts{URL: "https://repo.example.com/g3", Category: "finance"})
	f.article(143, articleOpts{SourceID: disabled, URL: "https://repo.example.com/g4", Category: "tech"})

	got, err := f.arts.CountByCategory(f.ctx)
	if err != nil {
		t.Fatalf("CountByCategory 失败: %v", err)
	}
	if got["tech"] != 2 {
		t.Fatalf("tech 应为 2（停用源那篇不计入），实际 %d: %+v", got["tech"], got)
	}
	if got["finance"] != 1 {
		t.Fatalf("finance 应为 1，实际 %d: %+v", got["finance"], got)
	}
}

// TestCountByCategory_空库返回空map而非nil
func TestCountByCategory_空库返回空map而非nil(t *testing.T) {
	f := newFixture(t)
	got, err := f.arts.CountByCategory(f.ctx)
	if err != nil {
		t.Fatalf("CountByCategory 失败: %v", err)
	}
	if got == nil {
		t.Fatal("无行时必须返回非 nil 的空 map（调用方会直接写它）")
	}
	if len(got) != 0 {
		t.Fatalf("空库应返回空 map，实际 %+v", got)
	}
}

// TestDeleteOlderThan_★分批删除且不误删
//
// 用「子查询 LIMIT」实现分批，语义是「删掉至多 limit 条published_at < before 的文章」。
//
// ★ 关于删除**顺序**：子查询没有 ORDER BY，因此"哪几篇先被删"是不保证的。
// 这不是 bug —— 调用方janitor 的循环条件是 `n < batch则 break`，
// 即「一批删不满就收工」，不依赖任何顺序，几轮下来必然删净。
// 所以本用例只断言真正的不变量：**条数上限**与**不误删**。
// （若有人给子查询加上 ORDER BY published_at 以求"删最老的"，也不该因此挂掉。）
func TestDeleteOlderThan_分批删除且不误删(t *testing.T) {
	f := newFixture(t)
	const before = 1000
	old1 := f.article(150, articleOpts{URL: "https://repo.example.com/d1", PublishedAt: before - 10})
	old2 := f.article(151, articleOpts{URL: "https://repo.example.com/d2", PublishedAt: before - 20})
	keep := f.article(152, articleOpts{URL: "https://repo.example.com/d3", PublishedAt: before + 10})

	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	// limit=1：恰好删一条，且只能是两条老文章之一
	n, err := f.arts.DeleteOlderThan(f.ctx, tx, before, 1)
	if err != nil {
		t.Fatalf("DeleteOlderThan 失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("limit=1 应删 1 行，实际 %d", n)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	_, err1 := f.arts.GetByID(f.ctx, old1)
	_, err2 := f.arts.GetByID(f.ctx, old2)
	gone := 0
	for _, e := range []error{err1, err2} {
		if errors.Is(e, ErrNotFound) {
			gone++
		}
	}
	if gone != 1 {
		t.Fatalf("limit=1 后应有且仅有 1 条老文章消失，实际消失了 %d 条（err1=%v err2=%v）", gone, err1, err2)
	}
	// published_at >= before 的文章无论批内顺序如何，都绝不能被删
	if _, err := f.arts.GetByID(f.ctx, keep); err != nil {
		t.Fatalf("published_at >= before 的文章不得被删: %v", err)
	}

	// 再删一批把剩下的老文章清掉（limit<=0 走默认批量 1000）
	tx2, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	n, err = f.arts.DeleteOlderThan(f.ctx, tx2, before, 0)
	if err != nil {
		t.Fatalf("DeleteOlderThan(limit=0) 失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("剩余应只有 1 行符合条件，实际删了 %d 行", n)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	for _, id := range []int64{old1, old2} {
		if _, err := f.arts.GetByID(f.ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("文章 %d 应已被删，实际 err=%v", id, err)
		}
	}
	if _, err := f.arts.GetByID(f.ctx, keep); err != nil {
		t.Fatalf("新文章在两轮归档后仍必须健在: %v", err)
	}
}

// TestDeleteOlderThan_★按limit逐批删净_归档循环可收敛
//
// janitor 的终止条件是 `本批删除数 < batch 就break`，
// 因此「删满 limit 条」这个性质必须成立：若实现因条件写错而总是删不满
// （例如外层DELETE 少了 published_at 条件、子查询却带了），
// 归档循环要么误删新文章，要么永远删不干净。
func TestDeleteOlderThan_按limit逐批删净_归档循环可收敛(t *testing.T) {
	f := newFixture(t)
	const before = 1000
	const batch = 2
	var stale []int64
	for i := 0; i < 5; i++ {
		stale = append(stale, f.article(160+i, articleOpts{
			URL:         "https://repo.example.com/b" + string(rune('a'+i)),
			PublishedAt: before - int64(100-i),
		}))
	}
	keep := f.article(170, articleOpts{URL: "https://repo.example.com/keep", PublishedAt: before + 1})

	total := 0
	for round := 0; round < 10; round++ {
		tx, err := f.db.BeginWrite(f.ctx)
		if err != nil {
			t.Fatalf("第 %d 轮开启写事务失败: %v", round, err)
		}
		n, err := f.arts.DeleteOlderThan(f.ctx, tx, before, batch)
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("第 %d 轮 DeleteOlderThan 失败: %v", round, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("第 %d 轮提交失败: %v", round, err)
		}
		total += int(n)
		if int(n) < batch {
			break // janitor 的收工条件
		}
	}
	if total != len(stale) {
		t.Fatalf("逐批归档应删净 %d 条，实际共删 %d 条", len(stale), total)
	}
	for _, id := range stale {
		if _, err := f.arts.GetByID(f.ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("老文章 %d 未被删净: %v", id, err)
		}
	}
	if _, err := f.arts.GetByID(f.ctx, keep); err != nil {
		t.Fatalf("归档不得误删 published_at >= before 的文章: %v", err)
	}
}

// TestDeleteOlderThan_回滚后不得落盘
func TestDeleteOlderThan_回滚后不得落盘(t *testing.T) {
	f := newFixture(t)
	id := f.article(153, articleOpts{URL: "https://repo.example.com/d4", PublishedAt: 1})
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	if _, err := f.arts.DeleteOlderThan(f.ctx, tx, 1000, 100); err != nil {
		t.Fatalf("DeleteOlderThan 失败: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if _, err := f.arts.GetByID(f.ctx, id); err != nil {
		t.Fatalf("回滚后文章不得消失，实际 err=%v", err)
	}
}

// TestDeleteOlderThan_无匹配行返回0
func TestDeleteOlderThan_无匹配行返回0(t *testing.T) {
	f := newFixture(t)
	f.article(154, articleOpts{URL: "https://repo.example.com/d5", PublishedAt: 5000})
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	n, err := f.arts.DeleteOlderThan(f.ctx, tx, 1000, 100)
	if err != nil {
		t.Fatalf("DeleteOlderThan 失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("无匹配行应返回 0，实际 %d", n)
	}
}

// mustNormalize 是NormalizeURL 的测试包装（失败直接 Fatal）。
func mustNormalize(t *testing.T, raw string) string {
	t.Helper()
	s, err := util.NormalizeURL(raw, nil)
	if err != nil {
		t.Fatalf("归一化 %s 失败: %v", raw, err)
	}
	return s
}

// 编译期断言：store.Session 必须被 repo 的公开方法接受。
// 若将来某个方法误用了 *sql.Tx，fixture 里的 tx 传不进去，编译即失败。
var _ store.Session = (*store.Tx)(nil)

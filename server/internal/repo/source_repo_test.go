package repo

import (
	"errors"
	"testing"

	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/util"
)

// TestSourceCreateGet_往返_布尔与可空列都被正确转换
//
// sourceColumns 里有 3 个可空列（icon_url / language / remark）与 1 个可空外键
// （owner_user_id）。scanSource 用 sql.NullString / NullInt64 接，
// boolToInt 把 bool 落成 0/1。任一处的类型不匹配都会让Scan 静默错位。
func TestSourceCreateGet_往返_布尔与可空列都被正确转换(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	id, err := f.srcs.Create(f.ctx, &model.Source{
		Key: "repo-rt", Name: "往返源", URL: "https://rt.example.com/feed", Type: model.SourceTypeAtom,
		Category: "science", IsDefault: true, Enabled: false, SuggestInterval: 900,
		IconURL: "https://rt.example.com/icon.png", Language: "en-US", Remark: "备注",
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("创建源失败: %v", err)
	}
	s, err := f.srcs.GetByID(f.ctx, id)
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if s.Key != "repo-rt" || s.Name != "往返源" || s.URL != "https://rt.example.com/feed" {
		t.Fatalf("文本列错位: %+v", s)
	}
	if s.Type != model.SourceTypeAtom || s.Category != "science" {
		t.Fatalf("枚举列错位: type=%q category=%q", s.Type, s.Category)
	}
	if !s.IsDefault {
		t.Fatal("is_default=1 应读成 true")
	}
	if s.Enabled {
		t.Fatal("enabled=0 应读成 false")
	}
	if s.SuggestInterval != 900 {
		t.Fatalf("suggest_interval 错位: %d", s.SuggestInterval)
	}
	if s.IconURL != "https://rt.example.com/icon.png" || s.Language != "en-US" || s.Remark != "备注" {
		t.Fatalf("可空列错位: icon=%q lang=%q remark=%q", s.IconURL, s.Language, s.Remark)
	}
	if s.OwnerUserID != 0 {
		t.Fatalf("owner_user_id 为 NULL 时应读成 0，实际 %d", s.OwnerUserID)
	}
	// 按 key 查到的必须是同一行
	byKey, err := f.srcs.GetByKey(f.ctx, "repo-rt")
	if err != nil {
		t.Fatalf("GetByKey 失败: %v", err)
	}
	if byKey.ID != id {
		t.Fatalf("GetByKey 返回了不同的行: %d != %d", byKey.ID, id)
	}
}

// TestSourceCreate_可空列留空_应写NULL
//
// nullString 把 "" 转成 NULL，而不是写空串。这条对「有值/无值」两种语义
// 很关键：若写成空串，等值查询与前端「未设置」判断都会歧义。
//
// language 是例外：schema 里它是 NOT NULL DEFAULT 'zh-CN'，兜底由 service 层的
// defaultLanguage 做，repo 层不兜。所以本用例同时把这条契约钉住——
// repo 收到空 language 必须直接报 NOT NULL 错误，而不是偷偷写入默认值，
// 否则「service 忘了兜底」这类 bug 会被 repo 静默吞掉。
func TestSourceCreate_可空列留空_应写NULL(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	id, err := f.srcs.Create(f.ctx, &model.Source{
		Key: "repo-nullable", Name: "空列源", URL: "https://nullable.example.com",
		Type: model.SourceTypeRSS, Category: "tech", Enabled: true,
		SuggestInterval: 1800, Language: "en-US", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("创建源失败: %v", err)
	}
	var iconURL, language, remark *string
	var ownerID *int64
	if err := f.db.QueryRowContext(f.ctx,
		"SELECT icon_url, language, remark, owner_user_id FROM source WHERE id=?", id).
		Scan(&iconURL, &language, &remark, &ownerID); err != nil {
		t.Fatalf("直查失败: %v", err)
	}
	if iconURL != nil || remark != nil || ownerID != nil {
		t.Fatalf("留空的可空列应写成 NULL，实际 icon=%v remark=%v owner=%v",
			iconURL, remark, ownerID)
	}
	// 显式给了值就要写成值，不能被 nullString 吃掉
	if language == nil || *language != "en-US" {
		t.Fatalf("显式设置的语言必须落库，实际 %v", language)
	}
}

// TestSourceCreate_语言为空_必须被非空约束拒绝而不是偷偷用默认值
//
// language 的 NOT NULL 兜底责任在 service 层 defaultLanguage，repo 层刻意不兜。
func TestSourceCreate_语言为空_必须被非空约束拒绝而不是偷偷用默认值(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	_, err := f.srcs.Create(f.ctx, &model.Source{
		Key: "repo-empty-lang", Name: "空语言源", URL: "https://elang.example.com",
		Type: model.SourceTypeRSS, Category: "tech", Enabled: true,
		SuggestInterval: 1800, CreatedAt: now, UpdatedAt: now,
	})
	if err == nil {
		t.Fatal("language 为空时应被 NOT NULL 约束拒绝（兜底责任在 service 层）")
	}
	// 16 个种子源 + fixture 自己造的 1 个 = 17；插入失败不能留下半行
	if n := f.rows("source"); n != 17 {
		t.Fatalf("插入失败不应留下半行，实际 source 表有 %d 行（应为 16 种子 + 1 fixture）", n)
	}
}

// TestSourceCreate_业务键重复_必须报错
//
// ux_source_key 是采集器引用源的锚：key 重复会让两个源指向同一份订阅，
// 采集时互相覆盖。
func TestSourceCreate_业务键重复_必须报错(t *testing.T) {
	f := newFixture(t)
	f.mustSource("repo-dup-key", "tech")
	now := util.NowMs()
	_, err := f.srcs.Create(f.ctx, &model.Source{
		Key: "repo-dup-key", Name: "重复", URL: "https://x.example.com",
		Type: model.SourceTypeRSS, Category: "tech", Enabled: true,
		SuggestInterval: 1800, Language: "zh-CN", CreatedAt: now, UpdatedAt: now,
	})
	if err == nil {
		t.Fatal("重复的 source.key 必须被唯一索引拒绝")
	}
}

// TestSourceGet_不存在_返回ErrNotFound
func TestSourceGet_不存在_返回ErrNotFound(t *testing.T) {
	f := newFixture(t)
	if _, err := f.srcs.GetByID(f.ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID 不存在应返回 ErrNotFound，实际 %v", err)
	}
	if _, err := f.srcs.GetByKey(f.ctx, "no-such-key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByKey 不存在应返回 ErrNotFound，实际 %v", err)
	}
}

// TestSourceList_★默认只返回启用源且停用源必须能显式带出
//
// 停用源是「暂停采集」而非「删除」，所以默认列表要滤掉它，
// 但管理端必须能通过 includeDisabled 把它捞回来。
func TestSourceList_默认只返回启用源且停用源必须能显式带出(t *testing.T) {
	f := newFixture(t)
	on := f.mustSource("repo-list-on", "tech")
	off := f.mustDisabledSource("repo-list-off", "tech")

	def, err := f.srcs.List(f.ctx, false)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if hasSource(def, on) == false {
		t.Fatal("启用源应出现在默认列表")
	}
	if hasSource(def, off) {
		t.Fatal("停用源不得出现在默认列表")
	}

	all, err := f.srcs.List(f.ctx, true)
	if err != nil {
		t.Fatalf("List(includeDisabled) 失败: %v", err)
	}
	if !hasSource(all, on) || !hasSource(all, off) {
		t.Fatal("includeDisabled=true 时停用源也必须被带出（管理端要能看到并重新启用）")
	}
	if len(all) <= len(def) {
		t.Fatalf("带出停用源后总数应变多，实际 all=%d def=%d（含 16 个种子源）", len(all), len(def))
	}
}

// TestSourceList_★排序_默认源优先再按分类与名称
//
// 排序键是 (is_default DESC, category, name)：
// 系统默认源排前面，同分类内按名称（中文按 UTF-8 码位，稳定的字典序）。
// 若排序键写错，前端「源管理」页的分组就会乱跳。
func TestSourceList_排序_默认源优先再按分类与名称(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	mk := func(key, name, cat string, def bool) int64 {
		id, err := f.srcs.Create(f.ctx, &model.Source{
			Key: key, Name: name, URL: "https://" + key + ".example.com",
			Type: model.SourceTypeRSS, Category: cat, IsDefault: def, Enabled: true,
			SuggestInterval: 1800, Language: "zh-CN", CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			t.Fatalf("创建源 %s 失败: %v", key, err)
		}
		return id
	}
	//刻意乱序创建：排序必须由 SQL 保证而不是插入顺序
	plainB := mk("repo-o1", "B源", "world", false)
	plainA := mk("repo-o2", "A源", "world", false)
	other := mk("repo-o3", "C源", "china", false)
	def := mk("repo-o4", "默认源", "world", true)

	all, err := f.srcs.List(f.ctx, true)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	pos := map[int64]int{}
	for i, s := range all {
		pos[s.ID] = i
	}
	// 默认源必须在所有非默认源之前
	for _, id := range []int64{plainA, plainB, other} {
		if pos[def] > pos[id] {
			t.Fatalf("is_default=1 的源必须排在非默认源之前（default=%d pos=%d, other=%d pos=%d）",
				def, pos[def], id, pos[id])
		}
	}
	//同分类内按名称：A 源在 B 源之前
	if pos[plainA] > pos[plainB] {
		t.Fatalf("同分类内应按名称升序，A源=%d pos=%d B源=%d pos=%d",
			plainA, pos[plainA], plainB, pos[plainB])
	}
	// china 分类排在 world 之前（分类升序）
	if pos[other] > pos[plainA] {
		t.Fatalf("分类应升序，china=%d pos=%d world=%d pos=%d",
			other, pos[other], plainA, pos[plainA])
	}
}

// TestSourceBatchGetByKeys_批量命中且只返回入参的键
func TestSourceBatchGetByKeys_批量命中且只返回入参的键(t *testing.T) {
	f := newFixture(t)
	k1 := f.mustSource("repo-b1", "tech")
	f.mustSource("repo-b2", "finance")

	got, err := f.srcs.BatchGetByKeys(f.ctx, []string{"repo-b1", "不存在的键"})
	if err != nil {
		t.Fatalf("BatchGetByKeys 失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("只应返回命中的 1 条，实际 %d 条", len(got))
	}
	if got["repo-b1"].ID != k1 {
		t.Fatalf("命中的源错位: %d != %d", got["repo-b1"].ID, k1)
	}
	// 重复键不应报错或产生重复
	dup, err := f.srcs.BatchGetByKeys(f.ctx, []string{"repo-b1", "repo-b1"})
	if err != nil || len(dup) != 1 {
		t.Fatalf("重复键应正常返回 1 条，实际 %d 条 err=%v", len(dup), err)
	}
	empty, err := f.srcs.BatchGetByKeys(f.ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空入参应返回空 map，实际 %v err=%v", empty, err)
	}
}

// TestSourceUpdate_只改可编辑字段且key与is_default不变
//
// key 是采集器引用的稳定锚，is_default 决定能否删除；
// Update 的 SET 列表刻意不含这两列（由 service 层保证不可变）。
func TestSourceUpdate_只改可编辑字段且key与is_default不变(t *testing.T) {
	f := newFixture(t)
	now := util.NowMs()
	id, err := f.srcs.Create(f.ctx, &model.Source{
		Key: "repo-upd", Name: "旧名", URL: "https://old.example.com", Type: model.SourceTypeRSS,
		Category: "tech", IsDefault: true, Enabled: true, SuggestInterval: 1800,
		Language: "zh-CN", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("创建源失败: %v", err)
	}
	if err := f.srcs.Update(f.ctx, &model.Source{
		ID: id, Key: "试图改掉的key", Name: "新名", URL: "https://new.example.com",
		Type: model.SourceTypeAPI, Category: "finance", Enabled: false, SuggestInterval: 60,
		IconURL: "https://new.example.com/i.png", Language: "en-US", Remark: "备注",
		UpdatedAt: now + 1000,
	}); err != nil {
		t.Fatalf("Update 失败: %v", err)
	}
	s, err := f.srcs.GetByID(f.ctx, id)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if s.Key != "repo-upd" {
		t.Fatalf("key 不可变，被改成了 %q", s.Key)
	}
	if !s.IsDefault {
		t.Fatal("is_default 不可变，被改掉了")
	}
	if s.Name != "新名" || s.URL != "https://new.example.com" || s.Category != "finance" {
		t.Fatalf("可编辑字段未更新: %+v", s)
	}
	if s.Type != model.SourceTypeAPI || s.SuggestInterval != 60 {
		t.Fatalf("type/interval 未更新: %+v", s)
	}
	if s.Enabled {
		t.Fatal("enabled 未更新")
	}
	if s.IconURL != "https://new.example.com/i.png" || s.Remark != "备注" {
		t.Fatalf("icon/remark 未更新: %+v", s)
	}
	if s.UpdatedAt != now+1000 {
		t.Fatalf("updated_at 未更新: %d", s.UpdatedAt)
	}
}

// TestSourceSetEnabled_启停开关
func TestSourceSetEnabled_启停开关(t *testing.T) {
	f := newFixture(t)
	id := f.mustSource("repo-toggle", "tech")
	if err := f.srcs.SetEnabled(f.ctx, id, false, 12345); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	s, err := f.srcs.GetByID(f.ctx, id)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if s.Enabled {
		t.Fatal("SetEnabled(false) 未生效")
	}
	if s.UpdatedAt != 12345 {
		t.Fatalf("SetEnabled 必须刷新 updated_at，实际 %d", s.UpdatedAt)
	}
	if err := f.srcs.SetEnabled(f.ctx, id, true, 12346); err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	s, _ = f.srcs.GetByID(f.ctx, id)
	if !s.Enabled {
		t.Fatal("SetEnabled(true) 未生效")
	}
}

// TestSourceDelete_★tx为nil必须显式报错而不是静默回退到连接池
//
// ★ 这条防的是回归：删除源是复合写（先删 article 再删 source），
// service 层用 BeginWrite 持有全局写锁。若Delete 内部绕过 tx从连接池
// 另取连接，新事务拿不到写锁，而原事务又在等本方法返回才提交 ——
// 互相等待直到 busyTimeout 耗尽，必然 SQLITE_BUSY。
// 静默回退等于把这个坑留在原地，所以 tx 为 nil 时必须显式报错。
func TestSourceDelete_tx为nil必须显式报错而不是静默回退到连接池(t *testing.T) {
	f := newFixture(t)
	id := f.mustSource("repo-del-nil", "tech")
	err := f.srcs.Delete(f.ctx, nil, id)
	if err == nil {
		t.Fatal("tx 为 nil 时必须报错（防止绕过写事务导致 SQLITE_BUSY 死锁）")
	}
	// 且不得真的删掉
	if _, err := f.srcs.GetByID(f.ctx, id); err != nil {
		t.Fatalf("报错的同时不得删除任何行，实际 err=%v", err)
	}
}

// TestSourceDelete_不存在的行_返回ErrNotFound
func TestSourceDelete_不存在的行_返回ErrNotFound(t *testing.T) {
	f := newFixture(t)
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := f.srcs.Delete(f.ctx, tx, 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除不存在的源应返回 ErrNotFound，实际 %v", err)
	}
}

// TestSourceDelete_★级联删除文章与音频
//
// article.source_id 是 ON DELETE CASCADE，audio 又是级联到 article。
// 删源必须把整条链上的数据都带走，否则会留下指向不存在源的文章，
// 而 List 用 INNER JOIN source 会让它们静默消失（数据被孤立但没人报错）。
func TestSourceDelete_级联删除文章与音频(t *testing.T) {
	f := newFixture(t)
	id := f.mustSource("repo-del-cascade", "tech")
	a1 := f.article(500, articleOpts{SourceID: id, URL: "https://repo.example.com/c1"})
	a2 := f.article(501, articleOpts{SourceID: id, URL: "https://repo.example.com/c2"})

	// 给其中一篇造音频
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	taskID, err := f.audio.InsertTask(f.ctx, tx, &model.AudioTask{
		ArticleID: a1, Voice: "zh", Speed: 1.0, Status: model.TaskStatusPending,
		TextChars: 5, CreatedAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("创建合成任务失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	tx2, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	if err := f.srcs.Delete(f.ctx, tx2, id); err != nil {
		_ = tx2.Rollback()
		t.Fatalf("删除源失败: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	if got := f.rows("article"); got != 0 {
		t.Fatalf("删除源后其文章必须级联删除，实际残留 %d 篇", got)
	}
	if got := f.rows("audio_task"); got != 0 {
		t.Fatalf("删除源后其合成任务必须级联删除，实际残留 %d 行", got)
	}
	for _, aid := range []int64{a1, a2} {
		if _, err := f.arts.GetByID(f.ctx, aid); !errors.Is(err, ErrNotFound) {
			t.Fatalf("文章 %d 应已随源删除而消失，实际 err=%v", aid, err)
		}
	}
	if _, err := f.audio.GetTask(f.ctx, taskID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("合成任务 %d 应已级联删除，实际 err=%v", taskID, err)
	}
}

// TestSourceDelete_回滚后源仍在
func TestSourceDelete_回滚后源仍在(t *testing.T) {
	f := newFixture(t)
	id := f.mustSource("repo-del-rollback", "tech")
	tx, err := f.db.BeginWrite(f.ctx)
	if err != nil {
		t.Fatalf("开启写事务失败: %v", err)
	}
	if err := f.srcs.Delete(f.ctx, tx, id); err != nil {
		_ = tx.Rollback()
		t.Fatalf("删除源失败: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if _, err := f.srcs.GetByID(f.ctx, id); err != nil {
		t.Fatalf("回滚后源必须仍在，实际 err=%v", err)
	}
}

// TestSourceCountArticles_按源分组且无文章的源不出现在结果里
//
// GROUP BY 只返回有行的分组，所以「0 篇文章的源」不在 map 里。
// 调用方必须用 map 取值（缺失即0），不能把「缺失」当成错误。
func TestSourceCountArticles_按源分组且无文章的源不出现在结果里(t *testing.T) {
	f := newFixture(t)
	s1 := f.mustSource("repo-c1", "tech")
	s2 := f.mustSource("repo-c2", "tech")
	s3 := f.mustSource("repo-c3-empty", "tech") // 故意不建文章
	f.article(510, articleOpts{SourceID: s1, URL: "https://repo.example.com/n1"})
	f.article(511, articleOpts{SourceID: s1, URL: "https://repo.example.com/n2"})
	f.article(512, articleOpts{SourceID: s2, URL: "https://repo.example.com/n3"})

	got, err := f.srcs.CountArticles(f.ctx)
	if err != nil {
		t.Fatalf("CountArticles 失败: %v", err)
	}
	if got[s1] != 2 {
		t.Fatalf("源 %d 应统计到 2 篇，实际 %d", s1, got[s1])
	}
	if got[s2] != 1 {
		t.Fatalf("源 %d 应统计到 1篇，实际 %d", s2, got[s2])
	}
	if _, ok := got[s3]; ok {
		t.Fatal("无文章的源不应出现在结果 map 里（缺失即0，调用方按缺省处理）")
	}
	// 种子源没有文章，也不应出现
	if len(got) != 2 {
		t.Fatalf("只应有 2 个源有文章，实际 %d 个: %+v", len(got), got)
	}
}

// TestSourceCountArticles_★空库返回空map
func TestSourceCountArticles_空库返回空map(t *testing.T) {
	f := newFixture(t)
	got, err := f.srcs.CountArticles(f.ctx)
	if err != nil {
		t.Fatalf("CountArticles 失败: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("无文章时应返回非nil 空 map，实际 %+v", got)
	}
}

// hasSource 判断源列表里是否含某个 ID。
func hasSource(list []model.Source, id int64) bool {
	for _, s := range list {
		if s.ID == id {
			return true
		}
	}
	return false
}

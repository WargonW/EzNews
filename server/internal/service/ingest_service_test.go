package service

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eznews/eznews/internal/config"
	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/repo"
	"github.com/eznews/eznews/internal/store"
	"github.com/eznews/eznews/internal/util"
)

// newTestSvc 构造一个跑在临时 SQLite 文件上的 IngestService（含完整迁移）。
//
// 刻意走真实 DB + 真实迁移而不是 mock：本次修复的核心正确性依赖
// 「继承后的 category 与重算后的 content_hash 是否与写库值一致」，
// 这只有真正落库再读回才能验证。
func newTestSvc(t *testing.T) (*IngestService, *repo.SourceRepo, *repo.ArticleRepo) {
	t.Helper()
	db, err := store.Open(store.Options{Path: filepath.Join(t.TempDir(), "t.db")})
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db, false); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}
	articles := repo.NewArticleRepo(db)
	sources := repo.NewSourceRepo(db)
	svc, err := NewIngestService(db, articles, sources,
		&config.IngestConf{MaxBatchItems: 200, StoreContent: false, DefaultTimezone: "UTC"}, nil)
	if err != nil {
		t.Fatalf("构造 IngestService 失败: %v", err)
	}
	return svc, sources, articles
}

// mustCreateSource 建一个源并返回其 ID。
func mustCreateSource(t *testing.T, sources *repo.SourceRepo, key, category string) int64 {
	t.Helper()
	now := util.NowMs()
	id, err := sources.Create(context.Background(), &model.Source{
		Key: key, Name: key, URL: "https://example.com/" + key, Type: model.SourceTypeAPI,
		Category: category, Enabled: true, SuggestInterval: 1800, Language: "zh-CN",
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("创建源 %s 失败: %v", key, err)
	}
	return id
}

// fixedPublishedAt 返回一个稳定的发布时间字符串（必须在 now-365d ~ now+24h 内）。
func fixedPublishedAt() string {
	return time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
}

func baseItem(sourceKey, url string) model.IngestItem {
	return model.IngestItem{
		SourceKey:   sourceKey,
		ExternalID:  "ext-1",
		URL:         url,
		Title:       "测试标题",
		Summary:     "测试摘要",
		PublishedAt: fixedPublishedAt(),
	}
}

// articleCategory 读回文章的 category（用List 查询，避免依赖未导出的扫描函数）。
func articleCategory(t *testing.T, articles *repo.ArticleRepo, id int64) (string, int64) {
	t.Helper()
	a, err := articles.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("读取文章 %d 失败: %v", id, err)
	}
	return a.Category, a.UpdatedAt
}

// TestInheritCategory_条目缺省category_应继承源分类
//
// 修复的核心场景：采集器不带 category 时，文章不再静默落other，而是继承源分类。
// 修复前落other，导致 GET /api/v1/articles?category=finance 返回 0 条。
func TestInheritCategory_条目缺省category_应继承源分类(t *testing.T) {
	svc, sources, articles := newTestSvc(t)
	srcID := mustCreateSource(t, sources, "custom-mv4b", "finance")
	ctx := context.Background()

	item := baseItem("custom-mv4b", "https://example.com/a1")
	item.Category = "" // 缺省
	rcpt, err := svc.IngestBatch(ctx, []model.IngestItem{item}, true)
	if err != nil {
		t.Fatalf("ingest 失败: %v", err)
	}
	if rcpt.Created != 1 {
		t.Fatalf("首投应 created，实际回执: %+v", rcpt)
	}
	res := rcpt.Results[0]
	if res.ArticleID == nil {
		t.Fatalf("created 应返回 articleId: %+v", res)
	}
	got, _ := articleCategory(t, articles, *res.ArticleID)
	if got != "finance" {
		t.Fatalf("条目缺省 category 时应继承源的 finance，实际落库 category=%q（源 id=%d）", got, srcID)
	}
}

// TestInheritCategory_条目显式传other_不得被源分类覆盖
//
// 显式值（含显式 "other"）是权威值：继承只作用于「缺省」，不得反向覆盖显式提交。
func TestInheritCategory_条目显式传other_不得被源分类覆盖(t *testing.T) {
	svc, sources, articles := newTestSvc(t)
	mustCreateSource(t, sources, "custom-mv4b", "finance")
	ctx := context.Background()

	item := baseItem("custom-mv4b", "https://example.com/a1")
	item.Category = "other" // 显式 other
	rcpt, err := svc.IngestBatch(ctx, []model.IngestItem{item}, true)
	if err != nil {
		t.Fatalf("ingest 失败: %v", err)
	}
	if rcpt.Created != 1 {
		t.Fatalf("首投应 created，实际回执: %+v", rcpt)
	}
	id := *rcpt.Results[0].ArticleID
	got, _ := articleCategory(t, articles, id)
	if got != "other" {
		t.Fatalf("显式传 other 时应以采集器值为准，实际落库 category=%q（被错误继承为 finance）", got)
	}

	// 再投一次仍不带 category：语义是「每轮提交的 category 都是权威值，缺省时= 源分类」，
	// 因此这一轮会继承 finance，属于 updated（而非静默保持）。
	item2 := baseItem("custom-mv4b", "https://example.com/a1")
	rcpt2, err := svc.IngestBatch(ctx, []model.IngestItem{item2}, true)
	if err != nil {
		t.Fatalf("二次ingest 失败: %v", err)
	}
	if rcpt2.Updated != 1 {
		t.Fatalf("缺省重投应记为 updated（权威值由 other 变为继承的 finance），实际: %+v", rcpt2)
	}
}

// TestInheritCategory_继承后重投_必须skipped且updated_at不变
//
// 这条用例专门锁死「继承 + 重算指纹」的计算位置。
// 若把继承逻辑错放进 normalizeItem（Pass 1），指纹会算成不含源分类的值，
// 重投时必然失配 → 回执变成 updated 且 updated_at 被刷新 → 增量游标被污染。
// 修复前的实测正是如此（显式 finance → 不带 category 重投 → updated，分类被降级为 other）。
func TestInheritCategory_继承后重投_必须skipped且updated_at不变(t *testing.T) {
	svc, sources, articles := newTestSvc(t)
	mustCreateSource(t, sources, "custom-mv4b", "finance")
	ctx := context.Background()

	// 轮1：显式传 finance（模拟采集器已正确分类过的存量文章）
	item1 := baseItem("custom-mv4b", "https://example.com/a1")
	item1.Category = "finance"
	rcpt1, err := svc.IngestBatch(ctx, []model.IngestItem{item1}, true)
	if err != nil {
		t.Fatalf("轮1 ingest 失败: %v", err)
	}
	if rcpt1.Created != 1 {
		t.Fatalf("轮1 应 created，实际: %+v", rcpt1)
	}
	id := *rcpt1.Results[0].ArticleID
	cat1, updated1 := articleCategory(t, articles, id)
	if cat1 != "finance" {
		t.Fatalf("轮1 落库 category 应为 finance，实际 %q", cat1)
	}

	// 轮2：同一篇不传 category → 继承 finance → 指纹与轮1 完全一致 → 必须 skipped
	item2 := baseItem("custom-mv4b", "https://example.com/a1")
	rcpt2, err := svc.IngestBatch(ctx, []model.IngestItem{item2}, true)
	if err != nil {
		t.Fatalf("轮2 ingest 失败: %v", err)
	}
	if rcpt2.Skipped != 1 || rcpt2.Updated != 0 || rcpt2.Created != 0 {
		t.Fatalf("轮2 缺省重投必须 skipped(UNCHANGED)，实际回执: created=%d updated=%d skipped=%d results=%+v",
			rcpt2.Created, rcpt2.Updated, rcpt2.Skipped, rcpt2.Results)
	}
	if rcpt2.Results[0].Reason == nil || *rcpt2.Results[0].Reason != model.ReasonUnchanged {
		t.Fatalf("轮2 回执原因应为 UNCHANGED，实际: %+v", rcpt2.Results[0].Reason)
	}
	cat2, updated2 := articleCategory(t, articles, id)
	if cat2 != "finance" {
		t.Fatalf("轮2 不得把分类静默降级，实际 category=%q", cat2)
	}
	if updated2 != updated1 {
		t.Fatalf("skipped 时 updated_at 不得被刷新：轮1=%d 轮2=%d", updated1, updated2)
	}

	// 轮3：再来一次同样缺省 → 依然 skipped（继承是幂等的）
	rcpt3, err := svc.IngestBatch(ctx, []model.IngestItem{item2}, true)
	if err != nil {
		t.Fatalf("轮3 ingest 失败: %v", err)
	}
	if rcpt3.Skipped != 1 {
		t.Fatalf("轮3 应skipped，实际: %+v", rcpt3)
	}
	cat3, updated3 := articleCategory(t, articles, id)
	if cat3 != "finance" || updated3 != updated1 {
		t.Fatalf("轮3 状态漂移: category=%q updated_at=%d（期望 finance / %d）", cat3, updated3, updated1)
	}
}

// TestInheritCategory_源分类为空或非法_应回落other且不panic
//
// 继承来源缺失时必须回落 other，而不是空串入库或 panic。
func TestInheritCategory_源分类为空或非法_应回落other且不panic(t *testing.T) {
	cases := []struct {
		name string
		key  string
		cat  string
	}{
		{"空串", "custom-empty", ""},
		{"仅空白", "custom-blank", "   "},
		{"非法值", "custom-invalid", "not-a-category"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, sources, articles := newTestSvc(t)
			mustCreateSource(t, sources, tc.key, tc.cat)
			ctx := context.Background()

			item := baseItem(tc.key, "https://example.com/x1")
			rcpt, err := svc.IngestBatch(ctx, []model.IngestItem{item}, true)
			if err != nil {
				t.Fatalf("ingest 失败: %v", err)
			}
			if rcpt.Created != 1 {
				t.Fatalf("应 created，实际: %+v", receiptDetail(rcpt))
			}
			got, _ := articleCategory(t, articles, *rcpt.Results[0].ArticleID)
			if got != "other" {
				t.Fatalf("源分类 %q（%s）时条目缺省 category 应回落 other，实际 %q", tc.cat, tc.name, got)
			}

			// 继承后重投必须幂等（指纹已按 other 重算过）
			rcpt2, err := svc.IngestBatch(ctx, []model.IngestItem{baseItem(tc.key, "https://example.com/x1")}, true)
			if err != nil {
				t.Fatalf("二次 ingest 失败: %v", err)
			}
			if rcpt2.Skipped != 1 {
				t.Fatalf("回落 other 后重投应 skipped，实际: %+v", receiptDetail(rcpt2))
			}
		})
	}
}

// receiptDetail 把回执里的指针字段解引用，便于断言失败时直接可读。
func receiptDetail(r *model.IngestReceipt) string {
	var b strings.Builder
	fmt.Fprintf(&b, "received=%d created=%d updated=%d skipped=%d failed=%d", r.Received, r.Created, r.Updated, r.Skipped, r.Failed)
	for _, res := range r.Results {
		b.WriteString("\n  ")
		fmt.Fprintf(&b, "index=%d status=%s", res.Index, res.Status)
		if res.Reason != nil {
			fmt.Fprintf(&b, " reason=%s", *res.Reason)
		}
		if res.Field != nil {
			fmt.Fprintf(&b, " field=%s", *res.Field)
		}
		if res.Message != nil {
			fmt.Fprintf(&b, " message=%s", *res.Message)
		}
	}
	return b.String()
}

// TestNormalizeItem_categoryInherited判定
//
// 直接锁住「缺省 vs 显式」的判定：只有字段缺失/null/空白才算缺省，
// 显式 "other" 与显式合法值都不算。
func TestNormalizeItem_categoryInherited判定(t *testing.T) {
	svc, err := NewIngestService(nil, nil, nil,
		&config.IngestConf{MaxBatchItems: 200, StoreContent: false, DefaultTimezone: "UTC"}, nil)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	now := util.NowMs()
	cases := []struct {
		cat         string
		wantInherit bool
	}{
		{"", true},
		{"   ", true},
		{"other", false},
		{"finance", false},
		{"FINANCE", false},
	}
	for _, tc := range cases {
		item := baseItem("k", "https://example.com/z1")
		item.Category = tc.cat
		p, err := svc.normalizeItem(item, now)
		if err != nil {
			t.Fatalf("normalizeItem 失败: %v", err)
		}
		if p.categoryInherited != tc.wantInherit {
			t.Errorf("category=%q: categoryInherited=%v，期望 %v", tc.cat, p.categoryInherited, tc.wantInherit)
		}
	}
}

// TestInheritCategory_重算指纹_与直接构造同分类文章一致
//
// 验证 inheritCategory 重算出的指纹 == 用最终 category 直接算出的指纹。
// 两者不等就说明重算漏了某个字段，去重判定会失配。
func TestInheritCategory_重算指纹_与直接构造同分类文章一致(t *testing.T) {
	svc, err := NewIngestService(nil, nil, nil,
		&config.IngestConf{MaxBatchItems: 200, StoreContent: false, DefaultTimezone: "UTC"}, nil)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	now := util.NowMs()

	// 路径A：条目缺省 → Pass 3 继承 finance
	inherited, err := svc.normalizeItem(baseItem("k", "https://example.com/z1"), now)
	if err != nil {
		t.Fatalf("normalizeItem 失败: %v", err)
	}
	if !inherited.categoryInherited {
		t.Fatal("缺省条目应标记 categoryInherited=true")
	}
	inheritCategory(inherited.article, "finance")

	// 路径 B：条目直接显式传 finance
	explicitItem := baseItem("k", "https://example.com/z1")
	explicitItem.Category = "finance"
	explicit, err := svc.normalizeItem(explicitItem, now)
	if err != nil {
		t.Fatalf("normalizeItem 失败: %v", err)
	}

	if inherited.article.Category != explicit.article.Category {
		t.Fatalf("继承后 category=%q，显式路径 category=%q，应一致",
			inherited.article.Category, explicit.article.Category)
	}
	if inherited.article.ContentHash != explicit.article.ContentHash {
		t.Fatalf("继承路径算出的指纹 %s 与显式路径 %s 不一致 → 重投会被误判为真实变更",
			inherited.article.ContentHash, explicit.article.ContentHash)
	}
}

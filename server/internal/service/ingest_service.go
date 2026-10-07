package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/config"
	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/observe"
	"github.com/eznews/eznews/internal/repo"
	"github.com/eznews/eznews/internal/store"
	"github.com/eznews/eznews/internal/util"
)

// 时间区间校验常量（§3.3）：now-365d ≤ publishedAt ≤ now+24h
const (
	maxPastDuration   = 365 * 24 * time.Hour
	maxFutureDuration = 24 * time.Hour
)

// IngestService 实现 ingest 的去重判定与三态回执（★ ARCHITECTURE.md §3.4）。
type IngestService struct {
	db       *store.DB
	articles *repo.ArticleRepo
	sources  *repo.SourceRepo
	cfg      *config.IngestConf
	loc      *time.Location
	metrics  *observe.Metrics
}

// NewIngestService 创建 IngestService。
// defaultTimezone 用于解析不带时区的 publishedAt（默认 UTC）。
func NewIngestService(db *store.DB, articles *repo.ArticleRepo, sources *repo.SourceRepo,
	cfg *config.IngestConf, metrics *observe.Metrics) (*IngestService, error) {
	loc := time.UTC
	if cfg != nil && strings.TrimSpace(cfg.DefaultTimezone) != "" {
		if parsed, err := time.LoadLocation(strings.TrimSpace(cfg.DefaultTimezone)); err == nil {
			loc = parsed
		}
	}
	if cfg == nil {
		cfg = &config.IngestConf{MaxBatchItems: 200, StoreContent: false}
	}
	return &IngestService{db: db, articles: articles, sources: sources, cfg: cfg, loc: loc, metrics: metrics}, nil
}

// IngestBatch 在一个写事务内批量处理条目，返回三态回执。
//
// 语义（§3.4.2）：
//   - 未命中 → INSERT，回执 created，updated_at = now
//   - 命中且内容有变化 → UPDATE 业务字段，回执 updated，updated_at = now
//   - 命中且无变化 → 仅刷新 last_seen_at，回执 skipped(UNCHANGED)，updated_at 不变
//   - 两个去重键命中不同行 → 不写库，回执 failed(DEDUP_CONFLICT)
func (s *IngestService) IngestBatch(ctx context.Context, items []model.IngestItem, verbose bool) (*model.IngestReceipt, error) {
	start := time.Now()
	now := util.NowMs()
	receipt := &model.IngestReceipt{Received: len(items), Results: []model.IngestResult{}}

	if s.metrics != nil {
		s.metrics.AddIngested(int64(len(items)))
	}
	if len(items) == 0 {
		receipt.DurationMs = time.Since(start).Milliseconds()
		return receipt, nil
	}

	// ---------- Pass 1：纯字段校验（无 DB 访问） ----------
	prepared := make(map[int]*preparedItem)
	for i := range items {
		item := items[i]
		if fe := s.validateItem(item, i); fe != nil {
			receipt.Failed++
			receipt.Results = append(receipt.Results, failedResult(i, model.ReasonValidation, fe.Field, fe.Message, &item))
			continue
		}
		p, err := s.normalizeItem(item, now)
		if err != nil {
			// 时间超区间属于语义错误 → UNPROCESSABLE；其余按校验失败处理。
			if errors.Is(err, errPublishedAtOutOfRange) {
				receipt.Failed++
				receipt.Results = append(receipt.Results, failedResult(i, model.ReasonUnprocessable,
					fieldName(i, "publishedAt"), err.Error(), &item))
				continue
			}
			receipt.Failed++
			msg := err.Error()
			receipt.Results = append(receipt.Results, failedResult(i, model.ReasonValidation, fieldName(i, "url"), msg, &item))
			continue
		}
		prepared[i] = p
	}
	if len(prepared) == 0 {
		receipt.DurationMs = time.Since(start).Milliseconds()
		return receipt, nil
	}

	// ---------- Pass 2：解析源 ----------
	sourceByID, err := s.resolveSources(ctx, prepared, now)
	if err != nil {
		return nil, err
	}

	// ---------- Pass 3：单事务批写 ----------
	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return nil, apierr.Wrap(503, apierr.CodeDBBusy, "数据库繁忙，请重试", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// 收集去重键并批量查重
	var extKeys []repo.ExtKey
	var urlHashes []string
	for _, p := range prepared {
		if p.article.ExternalID != "" {
			extKeys = append(extKeys, repo.ExtKey{SourceID: p.article.SourceID, ExternalID: p.article.ExternalID})
		}
		urlHashes = append(urlHashes, p.article.URLHash)
	}
	mapExt, err := s.articles.BatchFindByExt(ctx, tx, dedupExtKeys(extKeys))
	if err != nil {
		return nil, err
	}
	mapURL, err := s.articles.BatchFindByURL(ctx, tx, dedupStrings(urlHashes))
	if err != nil {
		return nil, err
	}

	// 逐条判定（按 index 升序保证回执顺序稳定）
	for i := 0; i < len(items); i++ {
		p, ok := prepared[i]
		if !ok {
			continue // 已在 Pass 1 记为 failed
		}
		src := sourceByID[p.article.SourceID]
		if src == nil {
			receipt.Failed++
			receipt.Results = append(receipt.Results, failedResult(i, model.ReasonSourceNotFound,
				fieldName(i, "sourceKey"), sourceNotFoundMessage(items[i]), &items[i]))
			continue
		}
		// category 缺省时继承源分类（须在 applyOne 之前，重算后的指纹才是真正用于去重的指纹）
		if p.categoryInherited && src.Category != "" {
			inheritCategory(p.article, src.Category)
		}
		status, articleID, failure := s.applyOne(ctx, tx, p, mapExt, mapURL, now)
		res := model.IngestResult{Index: i, Status: status}
		if failure != nil {
			res.Reason = &failure.reason
			res.Message = &failure.message
			res.ExternalID = nullStr(items[i].ExternalID)
			res.URL = nullStr(items[i].URL)
			receipt.Failed++
		} else {
			reason := model.ReasonUnchanged
			if status == model.IngestStatusSkipped {
				res.Reason = &reason
			}
			id := articleID
			res.ArticleID = &id
			res.ExternalID = nullStr(items[i].ExternalID)
			res.URL = nullStr(items[i].URL)
			switch status {
			case model.IngestStatusCreated:
				receipt.Created++
			case model.IngestStatusUpdated:
				receipt.Updated++
			case model.IngestStatusSkipped:
				receipt.Skipped++
			}
		}
		receipt.Results = append(receipt.Results, res)
	}

	if err := tx.Commit(); err != nil {
		if apierr.IsDBBusy(err) {
			return nil, apierr.DBBusy()
		}
		return nil, apierr.Internal(fmt.Errorf("提交 ingest 事务失败: %w", err))
	}
	committed = true

	// ---------- 回执裁剪：默认只返回 failed 明细 ----------
	if !verbose {
		filtered := make([]model.IngestResult, 0, receipt.Failed)
		for _, r := range receipt.Results {
			if r.Status == model.IngestStatusFailed {
				filtered = append(filtered, r)
			}
		}
		receipt.Results = filtered
	}
	receipt.DurationMs = time.Since(start).Milliseconds()
	return receipt, nil
}

// preparedItem 是校验通过并完成规范化的待写入条目。
type preparedItem struct {
	article   *model.Article
	item      *model.IngestItem
	sourceKey string
	sourceID  int64
	// categoryInherited 标记 category 是「缺省」而非采集器显式提交。
	// 为 true 时在 Pass 3 拿到源之后用source.category 覆盖，并重算内容指纹。
	categoryInherited bool
}

type ingestFailure struct {
	reason  model.IngestReason
	message string
}

// applyOne 执行单条判定与写入，返回状态、（成功时的）文章 ID 或失败原因。
func (s *IngestService) applyOne(ctx context.Context, tx store.Session, p *preparedItem,
	mapExt map[repo.ExtKey]*model.Article, mapURL map[string]*model.Article, now int64) (
	model.IngestStatus, int64, *ingestFailure) {

	a := p.article
	var hitExt, hitURL *model.Article
	if a.ExternalID != "" {
		hitExt = mapExt[repo.ExtKey{SourceID: a.SourceID, ExternalID: a.ExternalID}]
	}
	hitURL = mapURL[a.URLHash]

	// 两个去重键命中不同记录 → 冲突，不写库
	if hitExt != nil && hitURL != nil && hitExt.ID != hitURL.ID {
		return model.IngestStatusFailed, 0, &ingestFailure{
			reason:  model.ReasonDedupConflict,
			message: fmt.Sprintf("去重键冲突：externalId 命中文章 %d，url 命中文章 %d", hitExt.ID, hitURL.ID),
		}
	}
	hit := hitExt
	if hit == nil {
		hit = hitURL
	}

	if hit == nil {
		a.CreatedAt = now
		a.UpdatedAt = now
		a.LastSeenAt = now
		id, err := s.articles.Insert(ctx, tx, a)
		if err != nil {
			return model.IngestStatusFailed, 0, &ingestFailure{reason: model.ReasonInternal, message: "写入失败"}
		}
		a.ID = id
		// 注册进批次内映射，避免同批重复条目产生唯一约束冲突
		mapURL[a.URLHash] = a
		if a.ExternalID != "" {
			mapExt[repo.ExtKey{SourceID: a.SourceID, ExternalID: a.ExternalID}] = a
		}
		return model.IngestStatusCreated, id, nil
	}

	// 命中：内容指纹与发布时间均未变 → skipped（★ 不刷新 updated_at）
	if hit.ContentHash == a.ContentHash && hit.PublishedAt == a.PublishedAt {
		if err := s.articles.TouchLastSeen(ctx, tx, hit.ID, now); err != nil {
			return model.IngestStatusFailed, 0, &ingestFailure{reason: model.ReasonInternal, message: "刷新 last_seen_at 失败"}
		}
		return model.IngestStatusSkipped, hit.ID, nil
	}

	// 命中且真实变更 → UPDATE（WHERE content_hash <> ? 保证幂等与并发安全）
	update := *hit
	update.URL = a.URL
	update.Title = a.Title
	update.Summary = a.Summary
	update.Content = a.Content
	update.ContentHash = a.ContentHash
	update.Category = a.Category
	update.Author = a.Author
	update.ImageURL = a.ImageURL
	update.Language = a.Language
	update.Tags = a.Tags
	update.PublishedAt = a.PublishedAt
	update.UpdatedAt = now
	update.LastSeenAt = now

	changed, err := s.articles.UpdateIfChanged(ctx, tx, &update)
	if err != nil {
		return model.IngestStatusFailed, 0, &ingestFailure{reason: model.ReasonInternal, message: "更新失败"}
	}
	if !changed {
		// 并发下已被其他提交更新为相同指纹 → 视为无变化
		if err := s.articles.TouchLastSeen(ctx, tx, hit.ID, now); err != nil {
			return model.IngestStatusFailed, 0, &ingestFailure{reason: model.ReasonInternal, message: "刷新 last_seen_at 失败"}
		}
		return model.IngestStatusSkipped, hit.ID, nil
	}
	// 同步批次内映射，供后续同批条目判定
	mapURL[hit.URLHash] = &update
	if hit.ExternalID != "" {
		mapExt[repo.ExtKey{SourceID: hit.SourceID, ExternalID: hit.ExternalID}] = &update
	}
	return model.IngestStatusUpdated, hit.ID, nil
}

// fieldError 是字段级校验错误。
type fieldError struct {
	Field   string
	Message string
}

// validateItem 校验单条目的字段约束（长度/格式/枚举）。
func (s *IngestService) validateItem(item model.IngestItem, index int) *fieldError {
	field := func(name string) string { return fieldName(index, name) }

	if strings.TrimSpace(item.URL) == "" {
		return &fieldError{Field: field("url"), Message: "url 不能为空"}
	}
	if len(item.URL) > 2048 {
		return &fieldError{Field: field("url"), Message: "url 长度不能超过 2048"}
	}
	if !util.IsHTTPURL(item.URL) {
		return &fieldError{Field: field("url"), Message: "必须是合法的 http/https URL"}
	}
	title := strings.TrimSpace(item.Title)
	if title == "" {
		return &fieldError{Field: field("title"), Message: "title 不能为空"}
	}
	if len(title) > 512 {
		return &fieldError{Field: field("title"), Message: "title 长度不能超过 512"}
	}
	if strings.TrimSpace(item.PublishedAt) == "" {
		return &fieldError{Field: field("publishedAt"), Message: "publishedAt 不能为空"}
	}
	if item.SourceKey == "" && item.SourceID <= 0 {
		return &fieldError{Field: field("sourceKey"), Message: "sourceKey 与 sourceId 至少提供一个"}
	}
	if item.SourceKey != "" && !isValidSourceKey(item.SourceKey) {
		return &fieldError{Field: field("sourceKey"), Message: "sourceKey 只能包含小写字母、数字、下划线与连字符，长度 1–64 且以字母或数字开头"}
	}
	if len(item.ExternalID) > 128 {
		return &fieldError{Field: field("externalId"), Message: "externalId 长度不能超过 128"}
	}
	if len(item.Summary) > 2000 {
		return &fieldError{Field: field("summary"), Message: "summary 长度不能超过 2000"}
	}
	if len(item.Content) > 20000 {
		return &fieldError{Field: field("content"), Message: "content 长度不能超过 20000"}
	}
	if len(item.Author) > 128 {
		return &fieldError{Field: field("author"), Message: "author 长度不能超过 128"}
	}
	if item.ImageURL != "" {
		if len(item.ImageURL) > 2048 {
			return &fieldError{Field: field("imageUrl"), Message: "imageUrl 长度不能超过 2048"}
		}
		if !util.IsHTTPURL(item.ImageURL) {
			return &fieldError{Field: field("imageUrl"), Message: "必须是合法的 http/https URL"}
		}
	}
	if item.Language != "" && len(item.Language) > 16 {
		return &fieldError{Field: field("language"), Message: "language 长度不能超过 16"}
	}
	if len(item.Tags) > 10 {
		return &fieldError{Field: field("tags"), Message: "tags 最多 10 项"}
	}
	for _, tg := range item.Tags {
		if len(strings.TrimSpace(tg)) == 0 || len(tg) > 32 {
			return &fieldError{Field: field("tags"), Message: "每个 tag 长度需在 1–32 之间"}
		}
	}
	return nil
}

// normalizeItem 规范化条目：URL 规范化 + 内容指纹 + 时间解析 + 枚举回落。
func (s *IngestService) normalizeItem(item model.IngestItem, now int64) (*preparedItem, error) {
	urlNorm, err := util.NormalizeURL(item.URL, s.cfg.TrackParamBlacklist)
	if err != nil {
		return nil, fmt.Errorf("url 规范化失败")
	}
	publishedAt, err := util.ParseTime(item.PublishedAt, s.loc)
	if err != nil {
		return nil, fmt.Errorf("publishedAt 格式非法")
	}
	if publishedAt < now-int64(maxPastDuration/time.Millisecond) ||
		publishedAt > now+int64(maxFutureDuration/time.Millisecond) {
		return nil, errPublishedAtOutOfRange
	}
	content := item.Content
	if !s.cfg.StoreContent {
		// PRD-Q3：默认不落库正文（正文不参与指纹，避免存储策略影响去重判定）
		content = ""
	}
	category := model.NormalizeCategory(item.Category)
	// 缺省（字段缺失 / null / 空白串）→ 记为待继承，在 Pass 3 用source.category 补齐。
	// 显式传值（含显式 "other"）一律以采集器为准。
	categoryInherited := strings.TrimSpace(item.Category) == ""
	language := strings.TrimSpace(item.Language)
	if language == "" {
		language = "zh-CN"
	}
	tags := make([]string, 0, len(item.Tags))
	for _, t := range item.Tags {
		t = strings.TrimSpace(t)
		if t != "" {
			tags = append(tags, t)
		}
	}
	summary := item.Summary
	if len(summary) > 2000 {
		summary = summary[:2000]
	}
	a := &model.Article{
		ExternalID:  strings.TrimSpace(item.ExternalID),
		URL:         strings.TrimSpace(item.URL),
		URLNorm:     urlNorm,
		URLHash:     util.URLHash(urlNorm),
		Title:       strings.TrimSpace(item.Title),
		Summary:     summary,
		Content:     content,
		Category:    category,
		Author:      strings.TrimSpace(item.Author),
		ImageURL:    strings.TrimSpace(item.ImageURL),
		Language:    language,
		Tags:        tags,
		PublishedAt: publishedAt,
	}
	a.ContentHash = util.ContentHashV1(util.CanonicalContent(
		a.Title, a.Summary, a.Content, a.ImageURL, a.Author, a.Category, a.PublishedAt))
	return &preparedItem{
		article:           a,
		sourceKey:         strings.TrimSpace(item.SourceKey),
		sourceID:          item.SourceID,
		categoryInherited: categoryInherited,
	}, nil
}

// inheritCategory 用所属源的分类补齐「缺省」的 category，并按新分类重算内容指纹。
//
// 必须在这里（Pass 3，拿到 src 之后）调用而不是 normalizeItem：category 参与内容指纹，
// 而指纹在 Pass 1 就已用于去重判定；若在 Pass 1 就算一个不含源分类的指纹，
// 继承后指纹与写库值不一致，重投会被误判为「真实变更」→ 每次都刷 updated_at 污染增量游标。
//
// 语义：源分类为空或非法时回落 other（由NormalizeCategory 兜底）。
func inheritCategory(a *model.Article, srcCategory string) {
	a.Category = model.NormalizeCategory(srcCategory)
	a.ContentHash = util.ContentHashV1(util.CanonicalContent(
		a.Title, a.Summary, a.Content, a.ImageURL, a.Author, a.Category, a.PublishedAt))
}

// errPublishedAtOutOfRange 表示发布时间超出允许区间。
var errPublishedAtOutOfRange = errors.New("发布时间超出允许区间（now-365d ~ now+24h）")

// isValidSourceKey 校验源业务键格式：^[a-z0-9][a-z0-9_-]*$，长度 1–64。
func isValidSourceKey(k string) bool {
	if k == "" || len(k) > 64 {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	first := k[0]
	return (first >= 'a' && first <= 'z') || (first >= '0' && first <= '9')
}

// sourceNotFoundMessage 生成"源未找到"的提示（区分 sourceKey / sourceId 两种引用方式）。
func sourceNotFoundMessage(item model.IngestItem) string {
	if item.SourceKey != "" {
		return fmt.Sprintf("未找到 sourceKey=%s", item.SourceKey)
	}
	return fmt.Sprintf("未找到 sourceId=%d", item.SourceID)
}

// resolveSources 解析条目引用的源；必要时按 autoCreateSource 自动建源。
// 返回 sourceID → Source 映射；未能解析的条目其 article.SourceID 保持 0。
func (s *IngestService) resolveSources(ctx context.Context, prepared map[int]*preparedItem, now int64) (
	map[int64]*model.Source, error) {
	// key 为 preparedItem 的临时标识：优先用 sourceID，其次用 sourceKey 解析后的 ID
	var keys []string
	var ids []int64
	for _, p := range prepared {
		if p.sourceKey != "" {
			keys = append(keys, p.sourceKey)
		}
		if p.sourceID > 0 {
			ids = append(ids, p.sourceID)
		}
	}
	byKey, err := s.sources.BatchGetByKeys(ctx, dedupStrings(keys))
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*model.Source)
	for _, id := range dedupInt64(ids) {
		src, err := s.sources.GetByID(ctx, id)
		if err == nil {
			byID[id] = src
		}
	}

	// 自动建源（默认关闭）
	if s.cfg.AutoCreateSource {
		for _, k := range dedupStrings(keys) {
			if _, ok := byKey[k]; ok {
				continue
			}
			created := &model.Source{
				Key:             k,
				Name:            k,
				URL:             "https://" + k,
				Type:            model.SourceTypeAPI,
				Category:        "other",
				Enabled:         true,
				SuggestInterval: 1800,
				Language:        "zh-CN",
				CreatedAt:       now,
				UpdatedAt:       now,
			}
			id, err := s.sources.Create(ctx, created)
			if err != nil {
				continue
			}
			created.ID = id
			byKey[k] = created
		}
	}

	// 将条目映射到具体 sourceID；同时收集所有引用到的源（供主流程回执使用）
	resolved := make(map[int64]*model.Source, len(byKey)+len(byID))
	for _, src := range byKey {
		resolved[src.ID] = src
	}
	for _, src := range byID {
		resolved[src.ID] = src
	}
	for _, p := range prepared {
		var src *model.Source
		if p.sourceKey != "" {
			src = byKey[p.sourceKey]
		}
		if p.sourceID > 0 {
			byIDSource := byID[p.sourceID]
			switch {
			case src == nil:
				src = byIDSource
			case byIDSource != nil && byIDSource.ID != src.ID:
				// sourceKey 与 sourceId 指向不同源 → 视为源不存在，记为 SOURCE_NOT_FOUND
				src = nil
			}
		}
		if src == nil {
			continue
		}
		p.article.SourceID = src.ID
	}
	return resolved, nil
}

// 辅助函数 ------------------------------------------------------------------

func failedResult(index int, reason model.IngestReason, field, message string, item *model.IngestItem) model.IngestResult {
	res := model.IngestResult{Index: index, Status: model.IngestStatusFailed, Reason: &reason}
	f := field
	res.Field = &f
	m := message
	res.Message = &m
	if item != nil {
		res.ExternalID = nullStr(item.ExternalID)
		res.URL = nullStr(item.URL)
	}
	return res
}

// fieldName 生成 "items[i].field" 形式的字段名。
func fieldName(index int, name string) string {
	return fmt.Sprintf("items[%d].%s", index, name)
}

// nullStr 将空串转为 nil（JSON 中省略）。
func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

func dedupStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func dedupInt64(in []int64) []int64 {
	seen := make(map[int64]struct{}, len(in))
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if v <= 0 {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func dedupExtKeys(in []repo.ExtKey) []repo.ExtKey {
	seen := make(map[repo.ExtKey]struct{}, len(in))
	out := make([]repo.ExtKey, 0, len(in))
	for _, v := range in {
		if v.ExternalID == "" || v.SourceID <= 0 {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

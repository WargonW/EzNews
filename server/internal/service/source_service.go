package service

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/repo"
	"github.com/eznews/eznews/internal/store"
	"github.com/eznews/eznews/internal/util"
)

// SourceService 提供源的查询与管理。
type SourceService struct {
	db       *store.DB
	sources  *repo.SourceRepo
	articles *repo.ArticleRepo
}

// NewSourceService 创建 SourceService。
func NewSourceService(db *store.DB, sources *repo.SourceRepo, articles *repo.ArticleRepo) *SourceService {
	return &SourceService{db: db, sources: sources, articles: articles}
}

// SourceInput 是新增/编辑源的输入。
type SourceInput struct {
	Name            string
	URL             string
	Type            string
	Category        string
	SuggestInterval int
	IconURL         string
	Language        string
	Remark          string
	Enabled         *bool
}

// List 返回源列表（含文章计数）。
func (s *SourceService) List(ctx context.Context, includeDisabled bool) ([]model.Source, error) {
	list, err := s.sources.List(ctx, includeDisabled)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	counts, err := s.sources.CountArticles(ctx)
	if err == nil {
		for i := range list {
			list[i].ArticleCount = counts[list[i].ID]
		}
	}
	return list, nil
}

// Create 新增自定义源（自动生成稳定 key）。
func (s *SourceService) Create(ctx context.Context, in SourceInput) (*model.Source, error) {
	if errs := validateSourceInput(in, false); len(errs) > 0 {
		return nil, apierr.Validation("源参数校验失败", errs)
	}
	now := util.NowMs()
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	src := &model.Source{
		Key:             generateSourceKey(in.Name),
		Name:            strings.TrimSpace(in.Name),
		URL:             strings.TrimSpace(in.URL),
		Type:            model.SourceType(in.Type),
		Category:        model.NormalizeCategory(in.Category),
		Enabled:         enabled,
		SuggestInterval: normalizeInterval(in.SuggestInterval),
		IconURL:         strings.TrimSpace(in.IconURL),
		Language:        defaultLanguage(in.Language),
		Remark:          strings.TrimSpace(in.Remark),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	// key 冲突时追加随机后缀重试（最多 5 次）
	for i := 0; i < 5; i++ {
		id, err := s.sources.Create(ctx, src)
		if err == nil {
			src.ID = id
			return src, nil
		}
		if isUniqueViolation(err) {
			src.Key = fmt.Sprintf("%s-%s", generateSourceKey(in.Name), randomLower(4))
			continue
		}
		return nil, apierr.Wrap(500, apierr.CodeInternal, "创建源失败", err)
	}
	return nil, apierr.Conflict("源已存在（key 冲突）")
}

// Update 编辑源；默认源仅允许修改 enabled。
func (s *SourceService) Update(ctx context.Context, id int64, in SourceInput) (*model.Source, error) {
	existing, err := s.sources.GetByID(ctx, id)
	if err != nil {
		return nil, mapRepoError(err, "源不存在")
	}
	if errs := validateSourceInput(in, true); len(errs) > 0 {
		return nil, apierr.Validation("源参数校验失败", errs)
	}
	if existing.IsDefault {
		if in.Enabled == nil {
			return nil, apierr.Validation("默认源仅允许修改 enabled", []apierr.Details{
				{Field: "enabled", Message: "默认源只能启用/停用，不能修改其他字段"},
			})
		}
		now := util.NowMs()
		if err := s.sources.SetEnabled(ctx, id, *in.Enabled, now); err != nil {
			return nil, apierr.Internal(err)
		}
		existing.Enabled = *in.Enabled
		existing.UpdatedAt = now
		return existing, nil
	}
	now := util.NowMs()
	existing.Name = strings.TrimSpace(in.Name)
	existing.URL = strings.TrimSpace(in.URL)
	existing.Type = model.SourceType(in.Type)
	existing.Category = model.NormalizeCategory(in.Category)
	existing.SuggestInterval = normalizeInterval(in.SuggestInterval)
	existing.IconURL = strings.TrimSpace(in.IconURL)
	existing.Language = defaultLanguage(in.Language)
	existing.Remark = strings.TrimSpace(in.Remark)
	if in.Enabled != nil {
		existing.Enabled = *in.Enabled
	}
	existing.UpdatedAt = now
	if err := s.sources.Update(ctx, existing); err != nil {
		return nil, apierr.Internal(err)
	}
	return existing, nil
}

// SetEnabled 启停源。
func (s *SourceService) SetEnabled(ctx context.Context, id int64, enabled bool) (*model.Source, error) {
	existing, err := s.sources.GetByID(ctx, id)
	if err != nil {
		return nil, mapRepoError(err, "源不存在")
	}
	now := util.NowMs()
	if err := s.sources.SetEnabled(ctx, id, enabled, now); err != nil {
		return nil, apierr.Internal(err)
	}
	existing.Enabled = enabled
	existing.UpdatedAt = now
	return existing, nil
}

// Delete 删除自定义源（默认源返回 409）。
func (s *SourceService) Delete(ctx context.Context, id int64) error {
	existing, err := s.sources.GetByID(ctx, id)
	if err != nil {
		return mapRepoError(err, "源不存在")
	}
	if existing.IsDefault {
		return apierr.Conflict("默认源不可删除，只能停用")
	}
	// 显式删除该源的文章与音频，兼容 foreign_keys 未生效的连接
	tx, derr := s.db.BeginWrite(ctx)
	if derr != nil {
		return apierr.DBBusy()
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, "DELETE FROM audio WHERE article_id IN (SELECT id FROM article WHERE source_id=?)", id); err != nil {
		return apierr.Internal(err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM audio_task WHERE article_id IN (SELECT id FROM article WHERE source_id=?)", id); err != nil {
		return apierr.Internal(err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM article WHERE source_id=?", id); err != nil {
		return apierr.Internal(err)
	}
	// 必须传 tx：这四句删除要原子生效。若 sources.Delete 走连接池（r.db）而非本事务，
	// 它会因拿不到写锁而与本事务互相等待，最终 SQLITE_BUSY（详见 SourceRepo.Delete 注释）。
	if err := s.sources.Delete(ctx, tx, id); err != nil {
		return mapRepoError(err, "源不存在")
	}
	if err := tx.Commit(); err != nil {
		return apierr.Internal(err)
	}
	committed = true
	return nil
}

// validateSourceInput 校验源输入字段。
func validateSourceInput(in SourceInput, allowEmptyName bool) []apierr.Details {
	var errs []apierr.Details
	name := strings.TrimSpace(in.Name)
	if name == "" && !allowEmptyName {
		errs = append(errs, apierr.Details{Field: "name", Message: "name 不能为空"})
	}
	if len(name) > 128 {
		errs = append(errs, apierr.Details{Field: "name", Message: "name 长度不能超过 128"})
	}
	url := strings.TrimSpace(in.URL)
	if url == "" {
		errs = append(errs, apierr.Details{Field: "url", Message: "url 不能为空"})
	} else if len(url) > 2048 || !util.IsHTTPURL(url) {
		errs = append(errs, apierr.Details{Field: "url", Message: "必须是合法的 http/https URL，且长度不超过 2048"})
	}
	if in.Type != "" && !model.SourceType(in.Type).IsValid() {
		errs = append(errs, apierr.Details{Field: "type", Message: "type 必须是 rss|atom|api|manual"})
	}
	if in.SuggestInterval != 0 && in.SuggestInterval < 60 {
		errs = append(errs, apierr.Details{Field: "suggestInterval", Message: "suggestInterval 不能小于 60"})
	}
	if in.IconURL != "" && (len(in.IconURL) > 2048 || !util.IsHTTPURL(in.IconURL)) {
		errs = append(errs, apierr.Details{Field: "iconUrl", Message: "必须是合法的 http/https URL"})
	}
	if len(in.Language) > 16 {
		errs = append(errs, apierr.Details{Field: "language", Message: "language 长度不能超过 16"})
	}
	if len(in.Remark) > 500 {
		errs = append(errs, apierr.Details{Field: "remark", Message: "remark 长度不能超过 500"})
	}
	return errs
}

// normalizeInterval 归一建议采集间隔（默认 1800 秒）。
func normalizeInterval(v int) int {
	if v <= 0 {
		return 1800
	}
	if v < 60 {
		return 60
	}
	return v
}

// defaultLanguage 归一语言（默认 zh-CN）。
func defaultLanguage(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "zh-CN"
	}
	return v
}

// generateSourceKey 由名称生成稳定的 ASCII 业务键（非 ASCII 字符转写为拼音不可行，退化为过滤）。
func generateSourceKey(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteRune('-')
		case unicode.Is(unicode.Han, r):
			// 中文字符无法安全转写，直接跳过（保证 key 合法）
			continue
		}
	}
	key := strings.Trim(b.String(), "-")
	if key == "" {
		key = "custom-" + randomLower(4)
	}
	// 首字符必须是字母或数字
	for len(key) > 0 && !((key[0] >= 'a' && key[0] <= 'z') || (key[0] >= '0' && key[0] <= '9')) {
		key = key[1:]
	}
	if key == "" {
		key = "custom-" + randomLower(4)
	}
	if len(key) > 48 {
		key = key[:48]
	}
	return key
}

// randomLower 生成 n 字符的小写字母数字随机串（用于构造合法的源 key）。
func randomLower(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, n)
	src := util.RandomString(n)
	for i := 0; i < n; i++ {
		out[i] = alphabet[int(src[i])%len(alphabet)]
	}
	return string(out)
}

// isUniqueViolation 判断是否唯一约束冲突。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed")
}

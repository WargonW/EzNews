package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/store"
)

// ArticleRepo 是 article 表的数据访问对象。
type ArticleRepo struct {
	db *store.DB
}

// NewArticleRepo 创建 ArticleRepo。
func NewArticleRepo(db *store.DB) *ArticleRepo {
	return &ArticleRepo{db: db}
}

// articleCols 是无表前缀的列清单（单表查询用）。
const articleCols = `id, source_id, IFNULL(external_id,'') AS external_id, url, url_norm, url_hash,
	title, summary, IFNULL(content,'') AS content, content_hash, category,
	IFNULL(author,'') AS author, IFNULL(image_url,'') AS image_url, language, tags_json,
	published_at, created_at, updated_at, last_seen_at`

// articleColsA 是带 "a." 前缀的列清单（JOIN 查询用）。
const articleColsA = `a.id, a.source_id, IFNULL(a.external_id,'') AS external_id, a.url, a.url_norm,
	a.url_hash, a.title, a.summary, IFNULL(a.content,'') AS content, a.content_hash, a.category,
	IFNULL(a.author,'') AS author, IFNULL(a.image_url,'') AS image_url, a.language, a.tags_json,
	a.published_at, a.created_at, a.updated_at, a.last_seen_at`

// ExtKey 是去重键 1：(sourceID, externalID)。
type ExtKey struct {
	SourceID   int64
	ExternalID string
}

// ArticleQuery 是列表/增量查询条件（只含 SQL 关心的字段，业务规则在 service 层）。
type ArticleQuery struct {
	Cursor          model.Cursor // 增量模式水位 (updated_at, id)
	PageCursor      model.Cursor // 翻页模式游标 (published_at, id)
	Delta           bool         // true=增量模式
	Limit           int
	SourceID        int64
	Category        string
	From            int64 // published_at 下界（0 表示不限）
	To              int64 // published_at 上界（0 表示不限）
	Keyword         string
	IncludeDisabled bool
	FTS             bool // 是否可用 FTS5 检索
}

// GetByID 按主键查询文章。
func (r *ArticleRepo) GetByID(ctx context.Context, id int64) (*model.Article, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+articleCols+" FROM article WHERE id=?", id)
	a := &model.Article{}
	var tagsRaw string
	err := row.Scan(&a.ID, &a.SourceID, &a.ExternalID, &a.URL, &a.URLNorm, &a.URLHash, &a.Title,
		&a.Summary, &a.Content, &a.ContentHash, &a.Category, &a.Author, &a.ImageURL, &a.Language,
		&tagsRaw, &a.PublishedAt, &a.CreatedAt, &a.UpdatedAt, &a.LastSeenAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("查询文章失败: %w", err)
	}
	a.Tags = model.ParseTags(tagsRaw)
	return a, nil
}

// BatchFindByExt 按 (source_id, external_id) 批量查重。
func (r *ArticleRepo) BatchFindByExt(ctx context.Context, tx store.Session, keys []ExtKey) (map[ExtKey]*model.Article, error) {
	out := make(map[ExtKey]*model.Article, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	for _, chunk := range chunkExtKeys(keys, 500) {
		conds := make([]string, 0, len(chunk))
		args := make([]any, 0, len(chunk)*2)
		for _, k := range chunk {
			conds = append(conds, "(source_id=? AND external_id=?)")
			args = append(args, k.SourceID, k.ExternalID)
		}
		rows, err := tx.QueryContext(ctx,
			"SELECT "+articleCols+" FROM article WHERE "+strings.Join(conds, " OR "), args...)
		if err != nil {
			return nil, fmt.Errorf("批量查重(externalId)失败: %w", err)
		}
		err = collectArticles(rows, func(a *model.Article) {
			out[ExtKey{SourceID: a.SourceID, ExternalID: a.ExternalID}] = a
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// BatchFindByURL 按 url_hash 批量查重。
func (r *ArticleRepo) BatchFindByURL(ctx context.Context, tx store.Session, hashes []string) (map[string]*model.Article, error) {
	out := make(map[string]*model.Article, len(hashes))
	if len(hashes) == 0 {
		return out, nil
	}
	for _, chunk := range chunkStrings(hashes, 500) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		rows, err := tx.QueryContext(ctx,
			"SELECT "+articleCols+" FROM article WHERE url_hash IN ("+placeholders+")", toAnySlice(chunk)...)
		if err != nil {
			return nil, fmt.Errorf("批量查重(url)失败: %w", err)
		}
		err = collectArticles(rows, func(a *model.Article) { out[a.URLHash] = a })
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Insert 插入文章并返回主键。updated_at 由调用方设置为 now（语义：新文章即一次变更）。
func (r *ArticleRepo) Insert(ctx context.Context, tx store.Session, a *model.Article) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO article (source_id, external_id, url, url_norm, url_hash, title, summary, content,
			content_hash, category, author, image_url, language, tags_json,
			published_at, created_at, updated_at, last_seen_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.SourceID, nullString(a.ExternalID), a.URL, a.URLNorm, a.URLHash, a.Title, a.Summary,
		nullString(a.Content), a.ContentHash, a.Category, nullString(a.Author), nullString(a.ImageURL),
		a.Language, a.TagsJSON(), a.PublishedAt, a.CreatedAt, a.UpdatedAt, a.LastSeenAt)
	if err != nil {
		return 0, fmt.Errorf("插入文章失败: %w", err)
	}
	return res.LastInsertId()
}

// UpdateIfChanged 仅在 contentHash 变化时才更新业务字段（利用 WHERE 条件避免 TOCTOU 竞争）。
// 返回 true 表示确实发生了更新（回执 updated），false 表示无变化（回执 skipped）。
//
// 注意：url_norm / url_hash 不参与更新，避免与另一行的去重键冲突（去重键保持稳定）。
func (r *ArticleRepo) UpdateIfChanged(ctx context.Context, tx store.Session, a *model.Article) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE article SET url=?, title=?, summary=?, content=?, content_hash=?, category=?, author=?,
			image_url=?, language=?, tags_json=?, published_at=?, updated_at=?, last_seen_at=?
		 WHERE id=? AND content_hash<>?`,
		a.URL, a.Title, a.Summary, nullString(a.Content), a.ContentHash, a.Category,
		nullString(a.Author), nullString(a.ImageURL), a.Language, a.TagsJSON(),
		a.PublishedAt, a.UpdatedAt, a.LastSeenAt, a.ID, a.ContentHash)
	if err != nil {
		return false, fmt.Errorf("更新文章失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// TouchLastSeen 仅刷新 last_seen_at（不参与增量游标）。
func (r *ArticleRepo) TouchLastSeen(ctx context.Context, tx store.Session, id, now int64) error {
	if _, err := tx.ExecContext(ctx, "UPDATE article SET last_seen_at=? WHERE id=?", now, id); err != nil {
		return fmt.Errorf("刷新 last_seen_at 失败: %w", err)
	}
	return nil
}

// List 按查询条件返回文章行（含源信息）。
func (r *ArticleRepo) List(ctx context.Context, q ArticleQuery) ([]model.ArticleListItem, error) {
	query, args := buildArticleQuery(q)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询文章列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.ArticleListItem
	for rows.Next() {
		var (
			item    model.ArticleListItem
			tagsRaw string
		)
		err := rows.Scan(&item.ID, &item.SourceID, &item.ExternalID, &item.URL, &item.URLNorm, &item.URLHash,
			&item.Title, &item.Summary, &item.Content, &item.ContentHash, &item.Category, &item.Author,
			&item.ImageURL, &item.Language, &tagsRaw, &item.PublishedAt, &item.CreatedAt, &item.UpdatedAt,
			&item.LastSeenAt, &item.SourceKey, &item.SourceName)
		if err != nil {
			return nil, fmt.Errorf("扫描文章行失败: %w", err)
		}
		item.Tags = model.ParseTags(tagsRaw)
		out = append(out, item)
	}
	return out, rows.Err()
}

// ExistsIDs 批量判断文章 ID 是否存在，返回存在的 ID 集合。
func (r *ArticleRepo) ExistsIDs(ctx context.Context, ids []int64) (map[int64]bool, error) {
	out := make(map[int64]bool, len(ids))
	for _, chunk := range chunkInt64(ids, 500) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		rows, err := r.db.QueryContext(ctx, "SELECT id FROM article WHERE id IN ("+placeholders+")", toAnyInt64(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[id] = true
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// briefChunkSize 是摘要批量查询的单批条数上限。
//
// SQLite 默认 SQLITE_MAX_VARIABLE_NUMBER=999，单批 500 留足余量
// （ArticleRepo 的其他 IN 查询同样按 500 分批，口径一致）。
const briefChunkSize = 500

// FindBriefsByIDs 按主键批量取回文章摘要，供收藏/已读增量流附加标题等信息。
//
// ★ 批量而非逐条：增量页最多 1000 条，逐条查就是 1000 次往返（典型 N+1）。
// 这里去重后按 briefChunkSize 分批，每批一条 `id IN (?,?,...)`，
// 故查询次数是 ceil(去重后条数/500)，与页大小无关地保持极低。
//
// ★ 不按 deleted_at / enabled 过滤：墓碑项（收藏已取消）仍需带出摘要，
// 前端要据此显示"文章已删除"而非空白行。article 表无软删列，
// 归档删除是物理 DELETE + FK 级联，那时查不到行 —— 调用方按缺字段处理即可。
//
// 查不到的行**不会**出现在返回的 map 里（不是置空结构体），
// 这样调用方能用 `brief, ok := m[id]` 精确区分"无摘要"与"空标题"。
func (r *ArticleRepo) FindBriefsByIDs(ctx context.Context, ids []int64) (map[int64]model.ArticleBrief, error) {
	out := make(map[int64]model.ArticleBrief, len(ids))
	uniq := dedupInt64(ids)
	for _, chunk := range chunkInt64(uniq, briefChunkSize) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		// JOIN source 取来源名；LEFT JOIN 保证源被删时文章仍能返回摘要。
		rows, err := r.db.QueryContext(ctx,
			`SELECT a.id, a.title, IFNULL(a.summary,''), IFNULL(s.name,''), a.published_at
			 FROM article a LEFT JOIN source s ON s.id = a.source_id
			 WHERE a.id IN (`+placeholders+`)`, toAnyInt64(chunk)...)
		if err != nil {
			return nil, fmt.Errorf("批量查询文章摘要失败: %w", err)
		}
		for rows.Next() {
			var (
				id int64
				b  model.ArticleBrief
			)
			if err := rows.Scan(&id, &b.Title, &b.Summary, &b.SourceName, &b.PublishedAt); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[id] = b
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// dedupInt64 保序去重（同一 id 在增量页里理论上不重复，但去重能把
// 批次数压到理论最小，且避免同一条被覆盖两次）。
func dedupInt64(in []int64) []int64 {
	if len(in) <= 1 {
		return in
	}
	seen := make(map[int64]struct{}, len(in))
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// DeleteOlderThan 分批删除 publishedAt 早于阈值的文章，返回本批删除行数。
func (r *ArticleRepo) DeleteOlderThan(ctx context.Context, tx store.Session, before int64, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	res, err := tx.ExecContext(ctx,
		"DELETE FROM article WHERE id IN (SELECT id FROM article WHERE published_at < ? LIMIT ?)", before, limit)
	if err != nil {
		return 0, fmt.Errorf("归档删除文章失败: %w", err)
	}
	return res.RowsAffected()
}

// CountAll 统计文章总数（/metrics 用）。
func (r *ArticleRepo) CountAll(ctx context.Context) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM article").Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// CountByCategory 按分类统计启用源下的文章数。
func (r *ArticleRepo) CountByCategory(ctx context.Context) (map[string]int64, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT a.category, COUNT(*) FROM article a
		 JOIN source s ON s.id = a.source_id AND s.enabled = 1
		 GROUP BY a.category`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]int64)
	for rows.Next() {
		var cat string
		var n int64
		if err := rows.Scan(&cat, &n); err != nil {
			return nil, err
		}
		out[cat] = n
	}
	return out, rows.Err()
}

// collectArticles 迭代结果集并把每行交给 fn（负责关闭 rows）。
func collectArticles(rows *sql.Rows, fn func(*model.Article)) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		a := &model.Article{}
		var tagsRaw string
		if err := rows.Scan(&a.ID, &a.SourceID, &a.ExternalID, &a.URL, &a.URLNorm, &a.URLHash, &a.Title,
			&a.Summary, &a.Content, &a.ContentHash, &a.Category, &a.Author, &a.ImageURL, &a.Language,
			&tagsRaw, &a.PublishedAt, &a.CreatedAt, &a.UpdatedAt, &a.LastSeenAt); err != nil {
			return fmt.Errorf("扫描文章失败: %w", err)
		}
		a.Tags = model.ParseTags(tagsRaw)
		fn(a)
	}
	return rows.Err()
}

// ftsMinRunes 是走 FTS5 trigram 索引的最小 rune 长度（见 buildArticleQuery）。
const ftsMinRunes = 3

// buildArticleQuery 组装列表/增量查询（全部参数化，禁止拼接用户数据）。
func buildArticleQuery(q ArticleQuery) (string, []any) {
	var (
		sb   strings.Builder
		args []any
	)
	// ★ 搜索策略按关键词的 rune 数量分流，而不是「有无关键词」。
	//
	// article_fts 用 tokenize='trigram' 建索引，trigram 分词器按 3 字符滑窗切分，
	// 因此少于 3 个字符的查询串永远匹配不到任何行（中文两字词是用户最常搜的形态，
	// 命中率为 0 属于功能缺陷而非「搜不到」）。所以长度 <3 的词必须落到 LIKE 分支。
	//
	// ★ 必须用 RuneCountInString，不能用 len()：中文 1 字 = 3 字节，
	//「央行」len()=6 会被误判为 >=3 而继续走 FTS，bug 依旧。
	useFTS := q.Keyword != "" && q.FTS && utf8.RuneCountInString(q.Keyword) >= ftsMinRunes
	if useFTS {
		sb.WriteString("SELECT " + articleColsA + ", s.key, s.name FROM article_fts " +
			"JOIN article a ON a.id = article_fts.rowid JOIN source s ON s.id = a.source_id " +
			"WHERE article_fts MATCH ?")
		args = append(args, ftsMatchPhrase(q.Keyword))
	} else {
		sb.WriteString("SELECT " + articleColsA + ", s.key, s.name FROM article a " +
			"JOIN source s ON s.id = a.source_id WHERE 1=1")
	}
	if !q.IncludeDisabled {
		sb.WriteString(" AND s.enabled = 1")
	}
	switch {
	case q.Delta:
		// 增量模式：行值比较 (updated_at, id) > (?, ?)，命中 idx_article_cursor
		sb.WriteString(" AND (a.updated_at, a.id) > (?, ?)")
		args = append(args, q.Cursor.TS, q.Cursor.ID)
	case q.PageCursor.TS > 0 || q.PageCursor.ID > 0:
		// 翻页模式：keyset 分页 (published_at, id) < (?, ?)
		sb.WriteString(" AND (a.published_at, a.id) < (?, ?)")
		args = append(args, q.PageCursor.TS, q.PageCursor.ID)
	}
	if q.SourceID > 0 {
		sb.WriteString(" AND a.source_id = ?")
		args = append(args, q.SourceID)
	}
	if q.Category != "" {
		sb.WriteString(" AND a.category = ?")
		args = append(args, q.Category)
	}
	if q.From > 0 {
		sb.WriteString(" AND a.published_at >= ?")
		args = append(args, q.From)
	}
	if q.To > 0 {
		sb.WriteString(" AND a.published_at <= ?")
		args = append(args, q.To)
	}
	if q.Keyword != "" && !useFTS {
		like := "%" + escapeLike(q.Keyword) + "%"
		sb.WriteString(" AND (a.title LIKE ? ESCAPE '\\' OR a.summary LIKE ? ESCAPE '\\')")
		args = append(args, like, like)
	}
	if q.Delta {
		sb.WriteString(" ORDER BY a.updated_at ASC, a.id ASC")
	} else {
		sb.WriteString(" ORDER BY a.published_at DESC, a.id DESC")
	}
	sb.WriteString(" LIMIT ?")
	args = append(args, q.Limit)
	return sb.String(), args
}

// ftsMatchPhrase 将关键词构造为 FTS5 短语查询（转义双引号）。
func ftsMatchPhrase(kw string) string {
	return `"` + strings.ReplaceAll(kw, `"`, `""`) + `"`
}

// escapeLike 转义 LIKE 模式中的通配符与转义符。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// chunkExtKeys 将去重键按 size 分批。
func chunkExtKeys(in []ExtKey, size int) [][]ExtKey {
	if size <= 0 {
		size = 500
	}
	var out [][]ExtKey
	for start := 0; start < len(in); start += size {
		end := start + size
		if end > len(in) {
			end = len(in)
		}
		out = append(out, in[start:end])
	}
	return out
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/eznews/eznews/internal/migrations"
)

// Migrate 按版本号顺序执行未应用的迁移脚本，并维护 schema_meta.schema_version。
//
// enableFTS 为 false 时跳过 003_fts.sql；脚本执行失败时按 §4 的降级方案回退：
// trigram → unicode61 → 跳过（仅警告，不阻断启动）。
func Migrate(ctx context.Context, d *DB, enableFTS bool) error {
	current, err := currentVersion(ctx, d)
	if err != nil {
		return err
	}
	list, err := migrations.List()
	if err != nil {
		return fmt.Errorf("读取迁移脚本失败: %w", err)
	}
	for _, m := range list {
		if m.Version <= current {
			continue
		}
		if m.Version == 3 && !enableFTS {
			slog.Info("跳过 FTS 迁移（search.enableFts=false）")
			if err := setVersion(ctx, d, m.Version); err != nil {
				return err
			}
			continue
		}
		if err := execScript(ctx, d, m.SQL); err != nil {
			// 仅 FTS 迁移允许降级失败；其余迁移失败必须阻断启动。
			if m.Version != 3 {
				return fmt.Errorf("执行迁移 %s 失败: %w", m.Name, err)
			}
			slog.Warn("FTS5(trigram) 初始化失败，尝试降级", slog.String("err", err.Error()))
			fallback := strings.Replace(m.SQL, "tokenize = 'trigram'", "tokenize = 'unicode61'", 1)
			if strings.Contains(fallback, "unicode61") {
				if err2 := execScript(ctx, d, fallback); err2 != nil {
					slog.Warn("FTS5 降级仍失败，搜索将退化为 LIKE", slog.String("err", err2.Error()))
					d.SetFTSEnabled(false)
				} else {
					d.SetFTSEnabled(true)
					slog.Info("FTS5 已降级为 unicode61 tokenizer")
				}
			} else {
				d.SetFTSEnabled(false)
			}
		} else if m.Version == 3 {
			d.SetFTSEnabled(true)
		}
		if err := setVersion(ctx, d, m.Version); err != nil {
			return err
		}
		slog.Info("迁移已应用", slog.String("name", m.Name), slog.Int("version", m.Version))
	}
	// ★ 迁移全部跳过后（重启场景，currentVersion 已是最新）上面的循环一次都不会
	// 走到 m.Version==3 分支，ftsEnabled 会保持零值 false。
	// 后果是重启后 db.FTSEnabled() 恒为 false，ArticleService 被注入
	// search.enableFts=false，搜索静默退化为 LIKE——表还在、性能却掉了，
	// 且没有任何报错。所以循环结束后统一按"表是否真的存在"回填一次，
	// 让标志位只取决于客观事实，不取决于本次进程有没有执行过 003。
	d.SetFTSEnabled(ftsTableExists(ctx, d))
	return nil
}

// ftsTableExists 检查 article_fts 表是否真的存在。
//
// 这是 FTS 可用性的唯一权威判据：迁移执行成功、降级执行成功都会留下这张表；
// 跳过 FTS 迁移或降级彻底失败则不会。故意不缓存、每次 Migrate 都重新查——
// 代价是一次sqlite_master 点查，可以忽略。
func ftsTableExists(ctx context.Context, d *DB) bool {
	var n int
	err := d.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='article_fts'").Scan(&n)
	if err != nil {
		slog.Warn("探测 article_fts 失败，搜索将退化为 LIKE", slog.String("err", err.Error()))
		return false
	}
	return n > 0
}

// currentVersion 读取当前 schema 版本；表不存在时返回 0（全新库）。
func currentVersion(ctx context.Context, d *DB) (int, error) {
	var value string
	err := d.QueryRowContext(ctx, "SELECT value FROM schema_meta WHERE key='schema_version'").Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		// schema_meta 尚未创建（全新库）
		if strings.Contains(err.Error(), "no such table") {
			return 0, nil
		}
		return 0, fmt.Errorf("读取 schema_version 失败: %w", err)
	}
	v, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("schema_version 非法: %w", err)
	}
	return v, nil
}

// setVersion 写入最新 schema 版本。
func setVersion(ctx context.Context, d *DB, v int) error {
	if _, err := d.ExecContext(ctx,
		"INSERT INTO schema_meta(key, value) VALUES('schema_version', ?) "+
			"ON CONFLICT(key) DO UPDATE SET value=excluded.value", strconv.Itoa(v)); err != nil {
		return fmt.Errorf("写入 schema_version 失败: %w", err)
	}
	return nil
}

// execScript 按语句逐条执行脚本（避免依赖驱动的多语句支持，失败信息更精确）。
func execScript(ctx context.Context, d *DB, script string) error {
	stmts := splitStatements(script)
	if len(stmts) == 0 {
		return nil
	}
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%w\n语句: %s", err, truncate(stmt, 120))
		}
	}
	return nil
}

// splitStatements 将 SQL 脚本切分为单条语句：跳过注释，并正确处理触发器内的 BEGIN...END; 块。
func splitStatements(script string) []string {
	var stmts []string
	var buf strings.Builder
	inTrigger := false

	flush := func() {
		s := strings.TrimSpace(buf.String())
		buf.Reset()
		if s != "" {
			stmts = append(stmts, s)
		}
	}

	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		if buf.Len() > 0 {
			buf.WriteString("\n")
		}
		buf.WriteString(line)

		upper := strings.ToUpper(trimmed)
		if strings.Contains(upper, "CREATE TRIGGER") {
			inTrigger = true
		}
		if inTrigger {
			if strings.HasSuffix(upper, "END;") {
				inTrigger = false
				flush()
			}
			continue
		}
		if strings.HasSuffix(trimmed, ";") {
			flush()
		}
	}
	flush()
	return stmts
}

// truncate 截断长文本用于错误输出。
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

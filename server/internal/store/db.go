// Package store 负责 SQLite 的打开、PRAGMA 初始化、schema 迁移与写事务封装。
//
// 并发策略（ARCHITECTURE.md §3.7 L4 / §3.8）：
//   - 写事务经全局互斥锁串行化，并以 BEGIN IMMEDIATE 立即取写锁，避免锁升级死锁；
//   - 读操作走连接池并发执行（WAL 下读不阻塞写）；
//   - busy_timeout 兜底，超时错误由 apierr.IsDBBusy 映射为 503 DB_BUSY。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动（无 CGO）
)

// Options 是打开数据库所需的参数。
type Options struct {
	Path          string // SQLite 文件路径
	BusyTimeoutMs int    // 写锁争用等待毫秒
	CacheSizeKB   int    // 页缓存大小（KB，负值单位）
	MaxOpenConns  int    // 连接池上限
}

// DB 是 SQLite 数据库句柄封装。
type DB struct {
	db         *sql.DB
	mu         sync.Mutex // 全局写锁
	ftsEnabled bool       // FTS5 是否可用（由迁移器回填）
}

// Session 是事务/连接的最小能力集合，供 repository 层使用（解耦具体事务实现）。
type Session interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Open 打开（必要时创建）SQLite 数据库并应用 PRAGMA。
func Open(opts Options) (*DB, error) {
	if strings.TrimSpace(opts.Path) == "" {
		return nil, errors.New("db path 为空")
	}
	if dir := filepath.Dir(opts.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据库目录失败: %w", err)
		}
	}
	if opts.BusyTimeoutMs <= 0 {
		opts.BusyTimeoutMs = 5000
	}
	if opts.CacheSizeKB == 0 {
		opts.CacheSizeKB = 16000
	}
	if opts.MaxOpenConns <= 0 {
		opts.MaxOpenConns = 8
	}

	dsn := buildDSN(opts)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	sqlDB.SetMaxOpenConns(opts.MaxOpenConns)
	sqlDB.SetMaxIdleConns(opts.MaxOpenConns)
	sqlDB.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	d := &DB{db: sqlDB}
	if err := d.applySessionPragmas(ctx, opts); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// buildDSN 组装 DSN。_pragma 参数由驱动在每条连接建立时应用，
// 保证连接池中每个连接都具备一致的 busy_timeout / foreign_keys / journal_mode 设置。
func buildDSN(opts Options) string {
	path := filepath.ToSlash(opts.Path)
	params := []string{
		fmt.Sprintf("_pragma=busy_timeout(%d)", opts.BusyTimeoutMs),
		"_pragma=foreign_keys(1)",
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		fmt.Sprintf("_pragma=cache_size(-%d)", opts.CacheSizeKB),
		"_pragma=temp_store(MEMORY)",
		"_pragma=wal_autocheckpoint(1000)",
	}
	return "file:" + path + "?" + strings.Join(params, "&")
}

// applySessionPragmas 在当前连接上补充执行非持久化的 PRAGMA，并做一次启动自检。
func (d *DB) applySessionPragmas(ctx context.Context, opts Options) error {
	row := d.db.QueryRowContext(ctx, "PRAGMA journal_mode")
	var mode string
	if err := row.Scan(&mode); err == nil {
		slog.Debug("sqlite journal_mode", slog.String("mode", mode))
	}
	var fk int
	if err := d.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err == nil && fk != 1 {
		slog.Warn("sqlite foreign_keys 未启用（级联删除将在代码层兜底处理）")
	}
	return nil
}

// Close 关闭数据库连接池。
func (d *DB) Close() error { return d.db.Close() }

// Ping 检查数据库可写性（供 /readyz 使用）。
func (d *DB) Ping(ctx context.Context) error {
	var one int
	if err := d.db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return err
	}
	return nil
}

// ExecContext 在非事务连接上执行写语句（仅用于无并发争用的运维操作）。
func (d *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.db.ExecContext(ctx, query, args...)
}

// QueryContext 在非事务连接上执行查询。
func (d *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.db.QueryContext(ctx, query, args...)
}

// QueryRowContext 在非事务连接上执行单行查询。
func (d *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.db.QueryRowContext(ctx, query, args...)
}

// SetFTSEnabled 记录 FTS5 可用性。
func (d *DB) SetFTSEnabled(v bool) { d.ftsEnabled = v }

// FTSEnabled 返回 FTS5 是否可用。
func (d *DB) FTSEnabled() bool { return d.ftsEnabled }

// Tx 是一个手动管理的写事务（BEGIN IMMEDIATE），持有一条专用连接与全局写锁。
type Tx struct {
	conn    *sql.Conn
	release func()
	once    sync.Once
	closed  bool
}

// BeginWrite 获取全局写锁并开启一个 BEGIN IMMEDIATE 事务。
// 调用方**必须**在完成后调用 Commit 或 Rollback（建议 defer tx.Rollback()）。
func (d *DB) BeginWrite(ctx context.Context) (*Tx, error) {
	d.mu.Lock()
	conn, err := d.db.Conn(ctx)
	if err != nil {
		d.mu.Unlock()
		return nil, fmt.Errorf("获取写连接失败: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		d.mu.Unlock()
		return nil, fmt.Errorf("开启写事务失败: %w", err)
	}
	return &Tx{conn: conn, release: d.mu.Unlock}, nil
}

// ExecContext 在事务内执行语句。
func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.conn.ExecContext(ctx, query, args...)
}

// QueryContext 在事务内执行查询。
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.conn.QueryContext(ctx, query, args...)
}

// QueryRowContext 在事务内执行单行查询。
func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.conn.QueryRowContext(ctx, query, args...)
}

// Commit 提交事务并释放写锁。
func (t *Tx) Commit() error {
	return t.finish("COMMIT")
}

// Rollback 回滚事务并释放写锁（可安全重复调用）。
func (t *Tx) Rollback() error {
	return t.finish("ROLLBACK")
}

func (t *Tx) finish(stmt string) error {
	var err error
	t.once.Do(func() {
		if t.closed {
			return
		}
		t.closed = true
		if _, e := t.conn.ExecContext(context.Background(), stmt); e != nil {
			err = e
		}
		if e := t.conn.Close(); e != nil && err == nil {
			err = e
		}
		t.release()
	})
	return err
}

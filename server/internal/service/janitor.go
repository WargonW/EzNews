// Package service 的后台清理任务：音频 LRU 清理、文章归档、会话清理、TTS 重试重入队。
package service

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eznews/eznews/internal/config"
	"github.com/eznews/eznews/internal/repo"
	"github.com/eznews/eznews/internal/store"
	"github.com/eznews/eznews/internal/util"
)

// Janitor 周期执行资源回收，是"轻量但健壮"的关键：
// 长期运行的单体服务如果不做 LRU 清理，音频目录会在几个月内撑爆磁盘，
// 而 SQLite 文件只增不减最终会吃满分区。这不是可选优化。
type Janitor struct {
	db        *store.DB
	articles  *repo.ArticleRepo
	audio     *repo.AudioRepo
	audioSvc  *AudioService
	auth      *AuthService
	cfg       *config.Config
	stopCh    chan struct{}
	doneCh    chan struct{}
	closeOnce chan struct{}
}

// NewJanitor 创建 Janitor。
func NewJanitor(db *store.DB, articles *repo.ArticleRepo, audio *repo.AudioRepo,
	audioSvc *AudioService, auth *AuthService, cfg *config.Config) *Janitor {
	return &Janitor{
		db: db, articles: articles, audio: audio, audioSvc: audioSvc, auth: auth, cfg: cfg,
		stopCh: make(chan struct{}), doneCh: make(chan struct{}), closeOnce: make(chan struct{}),
	}
}

// Start 启动后台循环。每轮各项清理独立失败，单项失败不影响其他项。
func (j *Janitor) Start() {
	interval := time.Duration(j.cfg.Janitor.IntervalMin) * time.Minute
	if interval <= 0 {
		interval = time.Hour
	}
	slog.Info("Janitor 已启动", slog.Duration("interval", interval))

	go func() {
		defer close(j.doneCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-j.stopCh:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				j.RunOnce(ctx)
				cancel()
			}
		}
	}()
}

// Stop 停止后台循环并等待当前轮次结束。
func (j *Janitor) Stop() {
	select {
	case <-j.stopCh: // 已停止
	default:
		close(j.stopCh)
	}
	select {
	case <-j.doneCh:
	case <-time.After(10 * time.Second):
		slog.Warn("Janitor 停止超时（当前清理尚未结束，将随进程退出）")
	}
}

// RunOnce 执行一轮全部清理任务。
func (j *Janitor) RunOnce(ctx context.Context) {
	j.requeueStuckTTSTasks(ctx)
	j.cleanupAudio(ctx)
	if j.cfg.Retention.Enabled {
		j.archiveArticles(ctx)
	}
	if j.auth != nil {
		j.cleanupSessions(ctx)
		j.auth.ExpiredFails()
	}
}

// cleanupAudio 按 LRU + 总量双约束清理音频。
//
// 触发条件是"或"关系：既看单条是否超过 maxAgeDays，也看总量是否超过 maxTotalBytes。
// 只看时间的话，用户只听最近 3 天的新闻，磁盘照样会满（每天 1000 条 × 800 字 TTS）。
func (j *Janitor) cleanupAudio(ctx context.Context) {
	now := util.NowMs()
	maxAge := j.cfg.Audio.MaxAgeDays
	if maxAge <= 0 {
		maxAge = 30
	}
	ageBefore := now - int64(maxAge)*24*3600*1000

	total, err := j.audio.TotalAudioBytes(ctx)
	if err != nil {
		slog.Warn("统计音频总量失败，跳过清理", slog.String("err", err.Error()))
		return
	}
	maxTotal := j.cfg.Audio.MaxTotalBytes
	overBudget := maxTotal > 0 && total > maxTotal

	if total <= int64(0) {
		return
	}
	// 时间维度总是执行；总量超限时把 ageBefore 放宽到 now（即清最久未访问的）。
	if !overBudget {
		// 只清超龄的
	} else {
		slog.Info("音频总量超预算，转为 LRU 清理",
			slog.Int64("totalBytes", total), slog.Int64("maxBytes", maxTotal))
		ageBefore = now
	}

	const batch = 200
	removed := 0
	for round := 0; round < 20; round++ { // 最多 20 轮，避免单次清理长期占用写锁
		rows, err := j.audio.ListAudioForCleanup(ctx, ageBefore, batch)
		if err != nil {
			slog.Warn("查询待清理音频失败", slog.String("err", err.Error()))
			break
		}
		if len(rows) == 0 {
			break
		}
		ids := make([]int64, 0, len(rows))
		for _, a := range rows {
			ids = append(ids, a.ID)
			// 先删文件再删行：反过来会出现"行没了但文件还在"的永久泄漏。
			// ★ FilePath 是**相对** audioDir 的路径（便于整体迁移数据目录），
			//   直接 os.Remove(relativePath) 会因进程 CWD 不同而误删/漏删。
			if p := j.absAudioPath(a.FilePath); p != "" {
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					slog.Warn("删除音频文件失败", slog.String("path", p), slog.String("err", err.Error()))
				}
			}
		}
		tx, err := j.db.BeginWrite(ctx)
		if err != nil {
			slog.Warn("开启写事务失败", slog.String("err", err.Error()))
			break
		}
		derr := j.audio.DeleteAudioIDs(ctx, tx, ids)
		cerr := tx.Commit()
		if derr != nil || cerr != nil {
			_ = tx.Rollback()
			slog.Warn("删除音频记录失败", slog.String("err", errText(derr, cerr)))
			break
		}
		removed += len(ids)
		if len(ids) < batch {
			break
		}
	}
	if removed > 0 {
		slog.Info("音频清理完成", slog.Int("removed", removed))
	}
}

// archiveArticles 删除超过保留期的文章及其关联音频。
func (j *Janitor) archiveArticles(ctx context.Context) {
	days := j.cfg.Retention.Days
	if days <= 0 {
		days = 90
	}
	before := util.NowMs() - int64(days)*24*3600*1000

	const batch = 500
	removed := 0
	for round := 0; round < 10; round++ {
		tx, err := j.db.BeginWrite(ctx)
		if err != nil {
			slog.Warn("开启写事务失败", slog.String("err", err.Error()))
			break
		}
		// 先取出这批文章的音频文件路径。
		//
		// ★ 这行读的是 j.articles.List，走 ArticleRepo 自己的连接池（r.db），
		//   **不是**上面这个 tx ——注释原先写"事务内读"是错的。之所以仍然安全：
		//   本函数持的是全局写锁（BeginWrite），其他写事务进不来；而这里是纯读，
		//   WAL 下读不阻塞写、也不被写阻塞。
		//   代价是读到的批次可能略滞后于before 边界，但归档是幂等的，
		//   下一轮会补上，不影响正确性。改成事务内读反而会踩WAL 事务边界不变量。
		rows, err := j.articles.List(ctx, repo.ArticleQuery{To: before, Limit: batch})
		if err != nil {
			_ = tx.Rollback()
			slog.Warn("查询待归档文章失败", slog.String("err", err.Error()))
			break
		}
		if len(rows) == 0 {
			_ = tx.Rollback()
			break
		}
		ids := make([]int64, 0, len(rows))
		for _, a := range rows {
			ids = append(ids, a.ID)
		}
		audios, err := j.audio.FindAudioByArticleIDs(ctx, tx, ids)
		if err != nil {
			_ = tx.Rollback()
			slog.Warn("查询待归档音频失败", slog.String("err", err.Error()))
			break
		}
		audioIDs := make([]int64, 0, len(audios))
		for _, a := range audios {
			audioIDs = append(audioIDs, a.ID)
		}
		if len(audioIDs) > 0 {
			if err := j.audio.DeleteAudioIDs(ctx, tx, audioIDs); err != nil {
				_ = tx.Rollback()
				slog.Warn("删除归档音频记录失败", slog.String("err", err.Error()))
				break
			}
		}
		n, err := j.articles.DeleteOlderThan(ctx, tx, before, batch)
		if cerr := tx.Commit(); err != nil || cerr != nil {
			_ = tx.Rollback()
			slog.Warn("归档文章失败", slog.String("err", errText(err, cerr)))
			break
		}
		// 文件删除放在提交之后：即使失败也只是留下孤儿文件，下次扫描仍会命中（行已删则不会）。
		// 反过来先删文件再提交，若提交失败就会出现"行在、文件没了"的坏数据。
		for _, a := range audios {
			if p := j.absAudioPath(a.FilePath); p != "" {
				_ = os.Remove(p)
			}
		}
		removed += int(n)
		if n < batch {
			break
		}
	}
	if removed > 0 {
		slog.Info("文章归档完成", slog.Int("removed", removed), slog.Int("retentionDays", days))
	}
}

// cleanupSessions 清理过期会话。
// revokedRetention 取 refresh token TTL 的 2 倍：泄露后需要留一段时间供审计与 reuse 判定。
func (j *Janitor) cleanupSessions(ctx context.Context) {
	retention := int64(j.cfg.Auth.RefreshTokenTTL.Std().Milliseconds()) * 2
	n, err := j.auth.CleanupExpiredSessions(ctx, retention)
	if err != nil {
		slog.Warn("清理过期会话失败", slog.String("err", err.Error()))
		return
	}
	if n > 0 {
		slog.Info("会话清理完成", slog.Int64("removed", n))
	}
}

// requeueStuckTTSTasks 处理两类"卡住"的任务：
//  1. 重试到期（next_retry_at <= now）但没进内存队列的——进程重启后队列会丢，
//     必须靠扫描捞回来，否则任务永久停在 processing。
//  2. processing 状态超过 30 分钟的僵尸任务——worker 崩溃时的兜底。
func (j *Janitor) requeueStuckTTSTasks(ctx context.Context) {
	if j.audioSvc == nil {
		return
	}
	now := util.NowMs()
	due, err := j.audio.ListRetryDue(ctx, now, 100)
	if err != nil {
		slog.Warn("查询待重试 TTS 任务失败", slog.String("err", err.Error()))
		return
	}
	for _, id := range due {
		j.audioSvc.Enqueue(id)
	}

	pending, err := j.audio.ListPending(ctx, 100)
	if err != nil {
		slog.Warn("查询 pending TTS 任务失败", slog.String("err", err.Error()))
		return
	}
	enqueued := 0
	for _, id := range pending {
		if j.audioSvc.Enqueue(id) {
			enqueued++
		}
	}
	if len(due) > 0 || enqueued > 0 {
		slog.Info("TTS 任务重新入队", slog.Int("retryDue", len(due)), slog.Int("pending", enqueued))
	}
}

// absAudioPath 把库中相对 audioDir 的路径还原为绝对路径。
//
// ★ 库里的 file_path 是相对路径（数据目录可整体迁移），所以这里必须 join audioDir。
//
//	若直接 os.Remove(a.FilePath)，进程 CWD 一变就会删错文件或全部失败，
//	而删除失败只打一条 warn 日志——磁盘占用悄悄涨到撑爆分区，极难排查。
//
// 空路径或绝对路径原样返回。
func (j *Janitor) absAudioPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(j.cfg.Audio.Dir, p)
}

// errText 合并两个错误中非nil 的一个，用于日志。
func errText(errs ...error) string {
	for _, e := range errs {
		if e != nil {
			return e.Error()
		}
	}
	return ""
}

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/config"
	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/observe"
	"github.com/eznews/eznews/internal/provider/tts"
	"github.com/eznews/eznews/internal/repo"
	"github.com/eznews/eznews/internal/store"
	"github.com/eznews/eznews/internal/util"
)

// 退避策略（§6.3）：1s → 5s → 20s（带 ±20% jitter）
var retryBackoff = []time.Duration{1 * time.Second, 5 * time.Second, 20 * time.Second}

// AudioService 编排合成任务：缓存键判定、入队、落盘、查询与流式读取。
type AudioService struct {
	db       *store.DB
	audio    *repo.AudioRepo
	articles *repo.ArticleRepo
	ttsMgr   *tts.Manager
	ttsCfg   *config.TTSConf
	cfg      *config.AudioConf
	metrics  *observe.Metrics

	queue chan int64

	accessMu  sync.Mutex
	accessSet map[int64]struct{}

	closeOnce sync.Once
	// queueOnce 单独守卫 queue 的关闭（Stop 只管 stopCh，两者语义不同，不能共用一个 Once）。
	queueOnce sync.Once
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

// NewAudioService 创建 AudioService。queueSize 来自 tts.queueSize（默认 512）。
func NewAudioService(db *store.DB, audio *repo.AudioRepo, articles *repo.ArticleRepo,
	ttsMgr *tts.Manager, ttsCfg *config.TTSConf, cfg *config.AudioConf, metrics *observe.Metrics) *AudioService {
	queueSize := ttsCfg.QueueSize
	if queueSize <= 0 {
		queueSize = 512
	}
	return &AudioService{
		db: db, audio: audio, articles: articles, ttsMgr: ttsMgr, ttsCfg: ttsCfg, cfg: cfg,
		metrics: metrics, queue: make(chan int64, queueSize),
		accessSet: make(map[int64]struct{}),
		stopCh:    make(chan struct{}),
	}
}

// Queue 返回任务队列的只读通道（供 worker pool 消费）。
func (s *AudioService) Queue() <-chan int64 { return s.queue }

// Enqueue 尝试将任务入队；队列满时返回 false（调用方据此返回 429）。
func (s *AudioService) Enqueue(taskID int64) bool {
	select {
	case s.queue <- taskID:
		return true
	default:
		return false
	}
}

// NormalizeVoice 归一音色（空值回落到配置默认）。
func (s *AudioService) NormalizeVoice(voice string) string {
	voice = trimStr(voice)
	if voice == "" {
		return s.ttsCfg.DefaultVoice
	}
	return voice
}

// NormalizeSpeed 归一语速：缺省 1.0，钳制到 [0.5, 2.0] 并保留 2 位小数（保证缓存键稳定）。
func (s *AudioService) NormalizeSpeed(speed float64) float64 {
	if speed <= 0 {
		speed = s.ttsCfg.DefaultSpeed
	}
	if speed <= 0 {
		speed = 1.0
	}
	if speed < 0.5 {
		speed = 0.5
	}
	if speed > 2.0 {
		speed = 2.0
	}
	return math.Round(speed*100) / 100
}

// EnsureTask 按缓存键 (articleID, voice, speed) 确保存在合成任务。
// 返回任务视图与是否命中缓存（命中缓存时客户端可直接播放）。
func (s *AudioService) EnsureTask(ctx context.Context, articleID int64, voice string, speed float64) (*model.AudioTask, bool, error) {
	voice = s.NormalizeVoice(voice)
	speed = s.NormalizeSpeed(speed)

	if _, err := s.articles.GetByID(ctx, articleID); err != nil {
		return nil, false, mapRepoError(err, "文章不存在")
	}
	now := util.NowMs()

	task, err := s.audio.FindTask(ctx, articleID, voice, speed)
	if err == nil {
		switch task.Status {
		case model.TaskStatusReady:
			if s.metrics != nil {
				s.metrics.AddTTS(true, true, 0)
			}
			return task, true, nil
		case model.TaskStatusFailed:
			// 重置失败任务后重新入队
			if err := s.audio.RequeueTask(ctx, task.ID, now); err != nil {
				return nil, false, apierr.Internal(err)
			}
			task.Status = model.TaskStatusPending
			task.RetryCount = 0
			s.tryEnqueue(task.ID)
			return task, false, nil
		default:
			// pending / processing：直接返回，由 worker 或 scanner 推进
			s.tryEnqueue(task.ID)
			return task, false, nil
		}
	}
	if !errors.Is(err, repo.ErrNotFound) {
		return nil, false, apierr.Internal(err)
	}

	if !s.ttsMgr.Available() {
		return nil, false, apierr.TTSUnavailable("未配置可用的 TTS provider，请在配置中设置 tts.credentials 或改用 tts.provider=mock")
	}

	// 新建任务（在写事务内插入，避免并发重复创建）
	tx, terr := s.db.BeginWrite(ctx)
	if terr != nil {
		return nil, false, apierr.DBBusy()
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	// 双重检查：并发下可能已被其他请求创建
	if existing, eerr := s.audio.FindTask(ctx, articleID, voice, speed); eerr == nil {
		if cerr := tx.Rollback(); cerr == nil {
			committed = true // 已回滚，避免 defer 二次回滚
		}
		s.tryEnqueue(existing.ID)
		return existing, existing.Status == model.TaskStatusReady, nil
	}
	newTask := &model.AudioTask{
		ArticleID: articleID,
		Voice:     voice,
		Speed:     speed,
		Status:    model.TaskStatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	id, ierr := s.audio.InsertTask(ctx, tx, newTask)
	if ierr != nil {
		if isUniqueViolation(ierr) {
			_ = tx.Rollback()
			committed = true
			if existing, eerr := s.audio.FindTask(ctx, articleID, voice, speed); eerr == nil {
				s.tryEnqueue(existing.ID)
				return existing, existing.Status == model.TaskStatusReady, nil
			}
		}
		return nil, false, apierr.Internal(ierr)
	}
	if cerr := tx.Commit(); cerr != nil {
		return nil, false, apierr.Internal(cerr)
	}
	committed = true

	newTask.ID = id
	// 队列满并不算失败：scanner 会在 5 秒后兜底重投
	s.tryEnqueue(id)
	return newTask, false, nil
}

// tryEnqueue 尽力入队，失败仅记 debug 日志（scanner 兜底）。
func (s *AudioService) tryEnqueue(taskID int64) {
	if !s.Enqueue(taskID) {
		slog.Debug("合成队列已满，任务将由 scanner 兜底重投", slog.Int64("taskId", taskID))
	}
}

// GetTask 查询任务状态。
func (s *AudioService) GetTask(ctx context.Context, taskID int64) (*model.AudioTask, error) {
	task, err := s.audio.GetTask(ctx, taskID)
	if err != nil {
		return nil, mapRepoError(err, "合成任务不存在")
	}
	return task, nil
}

// RetryTask 显式重试失败任务。
func (s *AudioService) RetryTask(ctx context.Context, taskID int64) (*model.AudioTask, error) {
	task, err := s.audio.GetTask(ctx, taskID)
	if err != nil {
		return nil, mapRepoError(err, "合成任务不存在")
	}
	if err := s.audio.RequeueTask(ctx, taskID, util.NowMs()); err != nil {
		return nil, apierr.Internal(err)
	}
	s.tryEnqueue(taskID)
	task.Status = model.TaskStatusPending
	task.RetryCount = 0
	task.UpdatedAt = util.NowMs()
	return task, nil
}

// ArticleAudio 缓存直查：返回该文章在指定音色/语速下已合成的音频（未合成返回 404）。
func (s *AudioService) ArticleAudio(ctx context.Context, articleID int64, voice string, speed float64) (*model.Audio, error) {
	a, err := s.audio.FindAudio(ctx, articleID, s.NormalizeVoice(voice), s.NormalizeSpeed(speed))
	if err != nil {
		return nil, mapRepoError(err, "该文章尚未合成音频")
	}
	return a, nil
}

// OpenAudio 打开音频文件用于 Range 流式返回，并记录一次访问（内存去重后批量刷盘）。
func (s *AudioService) OpenAudio(ctx context.Context, audioID int64) (*model.Audio, string, error) {
	a, err := s.audio.GetAudio(ctx, audioID)
	if err != nil {
		return nil, "", mapRepoError(err, "音频不存在")
	}
	if a.FilePath == "" {
		return nil, "", apierr.NotFound("音频不存在")
	}
	abs := filepath.Join(s.cfg.Dir, filepath.FromSlash(a.FilePath))
	if _, err := os.Stat(abs); err != nil {
		return nil, "", apierr.NotFound("音频不存在")
	}
	s.trackAccess(audioID)
	return a, abs, nil
}

// trackAccess 记录音频访问（写入内存集合，由后台协程批量刷盘）。
func (s *AudioService) trackAccess(audioID int64) {
	s.accessMu.Lock()
	s.accessSet[audioID] = struct{}{}
	s.accessMu.Unlock()
}

// StartAccessFlusher 启动访问统计刷盘协程（每 interval 秒一次）。
func (s *AudioService) StartAccessFlusher(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stopCh:
				s.flushAccess()
				return
			case <-ctx.Done():
				s.flushAccess()
				return
			case <-ticker.C:
				s.flushAccess()
			}
		}
	}()
}

// flushAccess 将内存中的访问集合批量写入数据库。
func (s *AudioService) flushAccess() {
	s.accessMu.Lock()
	if len(s.accessSet) == 0 {
		s.accessMu.Unlock()
		return
	}
	ids := make([]int64, 0, len(s.accessSet))
	for id := range s.accessSet {
		ids = append(ids, id)
	}
	s.accessSet = make(map[int64]struct{}, len(ids))
	s.accessMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.audio.TouchAccess(ctx, ids, util.NowMs()); err != nil {
		slog.Warn("刷新音频访问统计失败", slog.String("err", err.Error()))
	}
}

// Stop 停止后台协程。
func (s *AudioService) Stop() {
	s.closeOnce.Do(func() {
		close(s.stopCh)
		s.wg.Wait()
	})
}

// CloseQueue 关闭任务队列，通知所有 worker 退出。
//
// ★ 只能在 HTTP 已停止接收请求之后调用。提前关闭会让仍在处理的请求
// 向已关闭的 channel 发送 —— 那是 Go 的运行时致命错误（fatal error: send on closed channel），
// recover 中间件拦不住，会直接拖垮整个进程。
func (s *AudioService) CloseQueue() {
	s.queueOnce.Do(func() {
		close(s.queue)
	})
}

// ProcessTask 是 worker 执行单元：抢占 → 组装文本 → 合成 → 落盘 → 回写状态。
// 抢占失败（已被其他 worker 或并发请求处理）时静默返回 nil。
func (s *AudioService) ProcessTask(ctx context.Context, taskID int64) error {
	now := util.NowMs()

	// 1) CAS 抢占
	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return apierr.DBBusy()
	}
	claimed, err := s.audio.ClaimTask(ctx, tx, taskID, now)
	if cerr := tx.Commit(); cerr != nil {
		slog.Warn("提交抢占事务失败", slog.Int64("taskId", taskID), slog.String("err", cerr.Error()))
	}
	if err != nil {
		return apierr.Internal(err)
	}
	if !claimed {
		return nil
	}

	task, err := s.audio.GetTask(ctx, taskID)
	if err != nil {
		return apierr.Internal(err)
	}
	article, err := s.articles.GetByID(ctx, task.ArticleID)
	if err != nil {
		_ = s.failTask(ctx, task, "ARTICLE_NOT_FOUND", "文章不存在或已被归档")
		return nil
	}

	// 2) 组装并清洗文本
	maxChars := s.ttsCfg.MaxChars
	if maxChars <= 0 {
		maxChars = s.ttsMgr.MaxChars(800)
	}
	text := tts.BuildText(article.Title, article.Summary, maxChars)
	if text == "" {
		text = tts.BuildText(article.Title, "", maxChars)
	}
	if text == "" {
		_ = s.failTask(ctx, task, "EMPTY_TEXT", "待合成文本为空")
		return nil
	}

	// 3) 调用 provider 合成
	synthCtx, cancel := context.WithTimeout(ctx, s.ttsCfg.Timeout.Std())
	defer cancel()
	data, providerName, err := s.ttsMgr.Synth(synthCtx, tts.SynthRequest{
		Text:       text,
		Voice:      task.Voice,
		Speed:      task.Speed,
		Format:     s.ttsCfg.Format,
		SampleRate: s.ttsCfg.SampleRate,
	})
	if err != nil {
		if s.metrics != nil {
			s.metrics.AddTTS(false, false, 0)
		}
		return s.handleSynthError(ctx, task, err)
	}

	// 4) 落盘（先 .part 再 rename，保证原子）
	nowTime := time.UnixMilli(util.NowMs()).UTC()
	yyyymm := nowTime.Format("200601")
	format := audioFormatOf(providerName, s.ttsCfg.Format)
	audioID, err := s.persist(ctx, task, data, providerName, format, yyyymm)
	if err != nil {
		slog.Error("音频落盘失败", slog.Int64("taskId", task.ID), slog.String("err", err.Error()))
		_ = s.failTask(ctx, task, "PERSIST_FAILED", "音频落盘失败")
		return nil
	}
	_ = audioID
	if s.metrics != nil {
		s.metrics.AddTTS(true, false, int64(len(data)))
	}
	return nil
}

// persist 写入 audio 行并落盘文件，返回音频 ID。
func (s *AudioService) persist(ctx context.Context, task *model.AudioTask, data []byte,
	providerName, format, yyyymm string) (int64, error) {
	now := util.NowMs()
	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return 0, apierr.DBBusy()
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	audioRow := &model.Audio{
		TaskID:       task.ID,
		ArticleID:    task.ArticleID,
		Voice:        task.Voice,
		Speed:        task.Speed,
		Format:       format,
		FilePath:     "", // 待拿到 audioID 后回填
		SizeBytes:    int64(len(data)),
		DurationMs:   estimateDuration(data, format, s.ttsCfg.SampleRate),
		SampleRate:   s.ttsCfg.SampleRate,
		Provider:     providerName,
		HitCount:     0,
		LastAccessAt: now,
		CreatedAt:    now,
	}
	audioID, err := s.audio.MarkReady(ctx, tx, task.ID, audioRow, now)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	committed = true

	// 落盘：{audioDir}/{yyyyMM}/{audioId}.{ext}
	rel := yyyymm + "/" + strconv.FormatInt(audioID, 10) + "." + format
	abs := filepath.Join(s.cfg.Dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return audioID, fmt.Errorf("创建音频目录失败: %w", err)
	}
	if err := writeFileAtomic(abs, data); err != nil {
		return audioID, err
	}
	// 回填真实路径
	if _, err := s.db.ExecContext(ctx, "UPDATE audio SET file_path=?, size_bytes=? WHERE id=?", rel, len(data), audioID); err != nil {
		return audioID, fmt.Errorf("回填音频路径失败: %w", err)
	}
	// 记录文本长度（成本核算/统计）
	if _, err := s.db.ExecContext(ctx, "UPDATE audio_task SET text_chars=?, updated_at=? WHERE id=?",
		len([]rune(textOfTask(task))), util.NowMs(), task.ID); err != nil {
		slog.Warn("回填 text_chars 失败", slog.Int64("taskId", task.ID))
	}
	return audioID, nil
}

// handleSynthError 处理合成失败：未达上限则退避重投，否则标记最终失败。
func (s *AudioService) handleSynthError(ctx context.Context, task *model.AudioTask, cause error) error {
	now := util.NowMs()
	retryCount := task.RetryCount + 1
	errCode := "TTS_FAILED"
	msg := cause.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return apierr.DBBusy()
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if retryCount >= s.ttsCfg.MaxRetries {
		if err := s.audio.MarkFailed(ctx, tx, task.ID, errCode, msg, now); err != nil {
			return apierr.Internal(err)
		}
	} else {
		delay := retryBackoff[minInt(retryCount-1, len(retryBackoff)-1)]
		next := now + int64(delay/time.Millisecond)
		next = jitter(next, now)
		if err := s.audio.MarkRetry(ctx, tx, task.ID, retryCount, next, now, errCode, msg); err != nil {
			return apierr.Internal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return apierr.Internal(err)
	}
	committed = true
	slog.Warn("合成任务失败，已安排重试或终止",
		slog.Int64("taskId", task.ID), slog.Int("retry", retryCount), slog.String("err", msg))
	return nil
}

// failTask 直接标记任务为最终失败。
func (s *AudioService) failTask(ctx context.Context, task *model.AudioTask, code, msg string) error {
	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return apierr.DBBusy()
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := s.audio.MarkFailed(ctx, tx, task.ID, code, msg, util.NowMs()); err != nil {
		return apierr.Internal(err)
	}
	if err := tx.Commit(); err != nil {
		return apierr.Internal(err)
	}
	committed = true
	return nil
}

// ---------------- 辅助函数 ----------------

// writeFileAtomic 先写 .part 临时文件、fsync，再 rename 原子替换。
func writeFileAtomic(abs string, data []byte) error {
	tmp := abs + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("fsync 失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, abs); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("原子替换失败: %w", err)
	}
	return nil
}

// audioFormatOf 决定实际落盘的音频扩展名（mock 产出 wav）。
func audioFormatOf(providerName, configured string) string {
	if providerName == "mock" {
		return "wav"
	}
	if configured == "" {
		return "mp3"
	}
	return configured
}

// estimateDuration 估算音频时长：WAV 可精确计算，其余格式返回 0（未知）。
func estimateDuration(data []byte, format string, sampleRate int) int64 {
	if format != "wav" || len(data) <= 44 || sampleRate <= 0 {
		return 0
	}
	dataBytes := int64(len(data) - 44)
	return int64(float64(dataBytes) / float64(sampleRate*2) * 1000)
}

// jitter 对重投时间施加 ±20% 抖动，避免多个任务同时重投。
func jitter(next, base int64) int64 {
	delta := next - base
	if delta <= 0 {
		return next
	}
	// 用时间纳秒低位做伪随机偏移（±20%）
	offset := int64((time.Now().UnixNano()/1000)%41) - 20 // [-20, 20]
	return next + delta*offset/100
}

// textOfTask 取任务关联文本（用于 text_chars 统计，失败时返回空串）。
func textOfTask(task *model.AudioTask) string {
	if task == nil {
		return ""
	}
	return ""
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func trimStr(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

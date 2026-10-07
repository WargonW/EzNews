package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/store"
)

// AudioRepo 是 audio_task / audio 两张表的数据访问对象。
type AudioRepo struct {
	db *store.DB
}

// NewAudioRepo 创建 AudioRepo。
func NewAudioRepo(db *store.DB) *AudioRepo {
	return &AudioRepo{db: db}
}

const audioTaskCols = `id, article_id, voice, speed, status, IFNULL(provider,'') AS provider,
	IFNULL(audio_id,0) AS audio_id, IFNULL(error_code,'') AS error_code, IFNULL(error_msg,'') AS error_msg,
	retry_count, IFNULL(next_retry_at,0) AS next_retry_at, text_chars, created_at, updated_at`

const audioCols = `id, IFNULL(task_id,0) AS task_id, article_id, voice, speed, format, file_path,
	size_bytes, duration_ms, sample_rate, IFNULL(provider,'') AS provider, hit_count, last_access_at, created_at`

// FindTask 按缓存键 (articleID, voice, speed) 查询合成任务。
func (r *AudioRepo) FindTask(ctx context.Context, articleID int64, voice string, speed float64) (*model.AudioTask, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+audioTaskCols+" FROM audio_task WHERE article_id=? AND voice=? AND speed=?",
		articleID, voice, speed)
	return scanAudioTask(row)
}

// GetTask 按主键查询合成任务。
func (r *AudioRepo) GetTask(ctx context.Context, id int64) (*model.AudioTask, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+audioTaskCols+" FROM audio_task WHERE id=?", id)
	return scanAudioTask(row)
}

// InsertTask 插入合成任务并返回主键。
func (r *AudioRepo) InsertTask(ctx context.Context, tx store.Session, t *model.AudioTask) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO audio_task (article_id, voice, speed, status, provider, audio_id, error_code,
			error_msg, retry_count, next_retry_at, text_chars, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ArticleID, t.Voice, t.Speed, string(t.Status), nullString(t.Provider), nullInt64(t.AudioID),
		nullString(t.ErrorCode), nullString(t.ErrorMsg), t.RetryCount, nullInt64(t.NextRetryAt),
		t.TextChars, t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return 0, fmt.Errorf("创建合成任务失败: %w", err)
	}
	return res.LastInsertId()
}

// ClaimTask 以 CAS 方式抢占任务：仅当 status='pending' 时才置为 processing。
// 返回 true 表示抢占成功。
func (r *AudioRepo) ClaimTask(ctx context.Context, tx store.Session, id, now int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		"UPDATE audio_task SET status=?, updated_at=? WHERE id=? AND status=?",
		string(model.TaskStatusProcessing), now, id, string(model.TaskStatusPending))
	if err != nil {
		return false, fmt.Errorf("抢占合成任务失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkReady 在一个事务内写入 audio 行并把任务置为 ready（回填 audio_id）。
func (r *AudioRepo) MarkReady(ctx context.Context, tx store.Session, taskID int64, a *model.Audio, now int64) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO audio (task_id, article_id, voice, speed, format, file_path, size_bytes,
			duration_ms, sample_rate, provider, hit_count, last_access_at, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(article_id, voice, speed) DO UPDATE SET
			task_id=excluded.task_id, format=excluded.format,
			-- 重新合成时先保留原路径，待文件落盘后再由 UPDATE 修正
			file_path=CASE WHEN excluded.file_path <> '' THEN excluded.file_path ELSE audio.file_path END,
			size_bytes=excluded.size_bytes, duration_ms=excluded.duration_ms,
			sample_rate=excluded.sample_rate, provider=excluded.provider,
			last_access_at=excluded.last_access_at`,
		a.TaskID, a.ArticleID, a.Voice, a.Speed, a.Format, a.FilePath, a.SizeBytes, a.DurationMs,
		a.SampleRate, nullString(a.Provider), a.HitCount, a.LastAccessAt, a.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("写入音频记录失败: %w", err)
	}
	audioID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// 冲突更新路径下 LastInsertId 可能不为新行 ID，显式回查以确保准确。
	if err := tx.QueryRowContext(ctx,
		"SELECT id FROM audio WHERE article_id=? AND voice=? AND speed=?",
		a.ArticleID, a.Voice, a.Speed).Scan(&audioID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("回查音频 ID 失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE audio_task SET status=?, audio_id=?, provider=?, error_code=NULL, error_msg=NULL, updated_at=? WHERE id=?",
		string(model.TaskStatusReady), audioID, nullString(a.Provider), now, taskID); err != nil {
		return 0, fmt.Errorf("更新任务为 ready 失败: %w", err)
	}
	return audioID, nil
}

// MarkRetry 将任务回退为 pending 并设置退避重投时间。
func (r *AudioRepo) MarkRetry(ctx context.Context, tx store.Session, id int64, retryCount int, nextRetryAt, now int64, errCode, errMsg string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE audio_task SET status=?, retry_count=?, next_retry_at=?, error_code=?, error_msg=?, updated_at=?
		 WHERE id=?`,
		string(model.TaskStatusPending), retryCount, nextRetryAt, nullString(errCode), nullString(errMsg), now, id)
	if err != nil {
		return fmt.Errorf("标记任务重试失败: %w", err)
	}
	return nil
}

// MarkFailed 将任务置为最终失败（重试耗尽）。
func (r *AudioRepo) MarkFailed(ctx context.Context, tx store.Session, id int64, errCode, errMsg string, now int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE audio_task SET status=?, error_code=?, error_msg=?, next_retry_at=NULL, updated_at=? WHERE id=?`,
		string(model.TaskStatusFailed), nullString(errCode), nullString(errMsg), now, id)
	if err != nil {
		return fmt.Errorf("标记任务失败失败: %w", err)
	}
	return nil
}

// RequeueTask 将失败任务重置为 pending 并清空重试计数（显式重试接口）。
func (r *AudioRepo) RequeueTask(ctx context.Context, id, now int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE audio_task SET status=?, retry_count=0, next_retry_at=NULL, error_code=NULL, error_msg=NULL, updated_at=?
		 WHERE id=? AND status=?`,
		string(model.TaskStatusPending), now, id, string(model.TaskStatusFailed))
	if err != nil {
		return fmt.Errorf("重投任务失败: %w", err)
	}
	return nil
}

// GetAudio 按主键查询音频。
func (r *AudioRepo) GetAudio(ctx context.Context, id int64) (*model.Audio, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+audioCols+" FROM audio WHERE id=?", id)
	return scanAudio(row)
}

// FindAudio 按缓存键查询已合成音频（缓存直查，不触发合成）。
func (r *AudioRepo) FindAudio(ctx context.Context, articleID int64, voice string, speed float64) (*model.Audio, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+audioCols+" FROM audio WHERE article_id=? AND voice=? AND speed=?", articleID, voice, speed)
	return scanAudio(row)
}

// BatchTaskStatus 批量返回文章在指定音色/语速下的合成状态（供 withAudio 使用，避免 N+1）。
func (r *AudioRepo) BatchTaskStatus(ctx context.Context, articleIDs []int64, voice string, speed float64) (map[int64]model.AudioBrief, error) {
	out := make(map[int64]model.AudioBrief, len(articleIDs))
	if len(articleIDs) == 0 {
		return out, nil
	}
	for _, chunk := range chunkInt64(articleIDs, 500) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		args := append(toAnyInt64(chunk), voice, speed)
		rows, err := r.db.QueryContext(ctx,
			"SELECT article_id, status, audio_id FROM audio_task WHERE article_id IN ("+placeholders+") AND voice=? AND speed=?",
			args...)
		if err != nil {
			return nil, fmt.Errorf("批量查询合成状态失败: %w", err)
		}
		for rows.Next() {
			var (
				articleID int64
				status    string
				audioID   sql.NullInt64
			)
			if err := rows.Scan(&articleID, &status, &audioID); err != nil {
				_ = rows.Close()
				return nil, err
			}
			// SQL 已按 voice=? AND speed=? 过滤，命中的行必然是这个组合，
			// 直接回填入参即可（不必再查一次 voice/speed 列，省一次字段传输）。
			// ★ 必须逐条拷贝值再取地址：直接写 &voice 会让 map 里所有条目共享
			// 同一个变量地址，虽然当前只读无害，但一旦将来有人原地改就会串改。
			v, sp := voice, speed
			brief := model.AudioBrief{
				Status: status,
				Voice:  &v,
				Speed:  &sp,
			}
			if audioID.Int64 > 0 {
				id := audioID.Int64
				brief.AudioID = &id
			}
			out[articleID] = brief
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ListRetryDue 返回退避到期、需要重新入队的任务 ID。
func (r *AudioRepo) ListRetryDue(ctx context.Context, now, limit int64) ([]int64, error) {
	// 两类任务都需要重新入队：① 退避到期；② 从未入队（如队列满时创建）且已静默 5 秒
	rows, err := r.db.QueryContext(ctx,
		`SELECT id FROM audio_task WHERE status='pending' AND (
			(next_retry_at IS NOT NULL AND next_retry_at <= ?) OR
			(next_retry_at IS NULL AND updated_at <= ?))
		 ORDER BY next_retry_at, id LIMIT ?`, now, now-retryGraceMs, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListPending 返回全部待处理任务 ID（进程启动时恢复队列用）。
func (r *AudioRepo) ListPending(ctx context.Context, limit int64) ([]int64, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id FROM audio_task WHERE status='pending'
		 ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ResetStaleProcessing 将超时未完成的 processing 任务重置为 pending（进程重启恢复）。
func (r *AudioRepo) ResetStaleProcessing(ctx context.Context, before, now int64) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		"UPDATE audio_task SET status=?, next_retry_at=NULL, updated_at=? WHERE status=? AND updated_at < ?",
		string(model.TaskStatusPending), now, string(model.TaskStatusProcessing), before)
	if err != nil {
		return 0, fmt.Errorf("重置超时任务失败: %w", err)
	}
	return res.RowsAffected()
}

// retryGraceMs 是"从未入队"任务被 scanner 兜底重投前的静默期（5 秒）。
const retryGraceMs = 5000

// TotalAudioBytes 统计音频总字节数（LRU 清理依据）。
func (r *AudioRepo) TotalAudioBytes(ctx context.Context) (int64, error) {
	var total sql.NullInt64
	if err := r.db.QueryRowContext(ctx, "SELECT SUM(size_bytes) FROM audio").Scan(&total); err != nil {
		return 0, err
	}
	return total.Int64, nil
}

// ListAudioForCleanup 按 last_access_at 升序返回待清理音频（LRU）。
func (r *AudioRepo) ListAudioForCleanup(ctx context.Context, lastAccessBefore int64, limit int) ([]model.Audio, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+audioCols+" FROM audio WHERE last_access_at < ? ORDER BY last_access_at ASC, id ASC LIMIT ?",
		lastAccessBefore, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.Audio
	for rows.Next() {
		a, err := scanAudio(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// FindAudioByArticleIDs 查询指定文章关联的音频（归档时用于删除文件）。
func (r *AudioRepo) FindAudioByArticleIDs(ctx context.Context, tx store.Session, articleIDs []int64) ([]model.Audio, error) {
	out := make([]model.Audio, 0, len(articleIDs))
	for _, chunk := range chunkInt64(articleIDs, 500) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		rows, err := tx.QueryContext(ctx,
			"SELECT "+audioCols+" FROM audio WHERE article_id IN ("+placeholders+")", toAnyInt64(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			a, err := scanAudio(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			out = append(out, *a)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteAudioIDs 删除音频行（文件由调用方负责删除）。
func (r *AudioRepo) DeleteAudioIDs(ctx context.Context, tx store.Session, ids []int64) error {
	for _, chunk := range chunkInt64(ids, 500) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM audio WHERE id IN ("+placeholders+")", toAnyInt64(chunk)...); err != nil {
			return fmt.Errorf("删除音频记录失败: %w", err)
		}
	}
	return nil
}

// TouchAccess 批量累加访问次数并刷新 last_access_at（由内存去重集合定期刷盘）。
func (r *AudioRepo) TouchAccess(ctx context.Context, ids []int64, now int64) error {
	for _, chunk := range chunkInt64(ids, 500) {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		if _, err := r.db.ExecContext(ctx,
			"UPDATE audio SET hit_count = hit_count + 1, last_access_at=? WHERE id IN ("+placeholders+")",
			append([]any{now}, toAnyInt64(chunk)...)...); err != nil {
			return fmt.Errorf("刷新音频访问统计失败: %w", err)
		}
	}
	return nil
}

// scanAudioTask 扫描一行 audio_task。
func scanAudioTask(row scanner) (*model.AudioTask, error) {
	t := &model.AudioTask{}
	var status string
	err := row.Scan(&t.ID, &t.ArticleID, &t.Voice, &t.Speed, &status, &t.Provider, &t.AudioID,
		&t.ErrorCode, &t.ErrorMsg, &t.RetryCount, &t.NextRetryAt, &t.TextChars, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("扫描合成任务失败: %w", err)
	}
	t.Status = model.AudioTaskStatus(status)
	return t, nil
}

// scanAudio 扫描一行 audio。
func scanAudio(row scanner) (*model.Audio, error) {
	a := &model.Audio{}
	err := row.Scan(&a.ID, &a.TaskID, &a.ArticleID, &a.Voice, &a.Speed, &a.Format, &a.FilePath,
		&a.SizeBytes, &a.DurationMs, &a.SampleRate, &a.Provider, &a.HitCount, &a.LastAccessAt, &a.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("扫描音频失败: %w", err)
	}
	return a, nil
}

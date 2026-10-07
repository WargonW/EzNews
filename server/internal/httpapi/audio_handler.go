package httpapi

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/util"
)

// 音频缓存策略（openapi §getAudioFile 示例值）：
// 音频文件不可变（内容由 articleId+voice+speed 唯一确定），可长期强缓存。
const audioFileCacheControl = "public, max-age=31536000, immutable"

// AudioDTO 是音频对外视图（openapi AudioDTO）。
type AudioDTO struct {
	ID         int64   `json:"id"`
	ArticleID  int64   `json:"articleId"`
	Voice      string  `json:"voice"`
	Speed      float64 `json:"speed"`
	Format     string  `json:"format"`
	DurationMs int64   `json:"durationMs"`
	SizeBytes  int64   `json:"sizeBytes"`
	SampleRate int     `json:"sampleRate"`
	Provider   *string `json:"provider"`
	// URL 是音频下载地址（相对路径），例 /api/v1/audio/5127。
	URL       string `json:"url"`
	CreatedAt string `json:"createdAt"`
}

// AudioTaskDTO 是合成任务对外视图（openapi AudioTaskDTO）。
type AudioTaskDTO struct {
	ID         int64   `json:"id"`
	ArticleID  int64   `json:"articleId"`
	Voice      string  `json:"voice"`
	Speed      float64 `json:"speed"`
	Status     string  `json:"status"`
	AudioID    *int64  `json:"audioId"`
	Provider   *string `json:"provider"`
	RetryCount int     `json:"retryCount"`
	ErrorCode  *string `json:"errorCode"`
	ErrorMsg   *string `json:"errorMsg"`
	TextChars  int     `json:"textChars"`
	CreatedAt  string  `json:"createdAt"`
	UpdatedAt  string  `json:"updatedAt"`
}

// createAudioTaskRequest 是 POST /api/v1/audio/tasks 的请求体。
type createAudioTaskRequest struct {
	ArticleID int64   `json:"articleId"`
	Voice     string  `json:"voice"`
	Speed     float64 `json:"speed"`
}

// handleCreateAudioTask 处理 POST /api/v1/audio/tasks：提交合成任务。
//
// 状态码语义（openapi）：
//   - 200：命中缓存，音频已就绪，客户端可直接播放。
//   - 202：已入队（pending / processing），客户端需轮询 GET /audio/tasks/{taskId}。
func (s *Server) handleCreateAudioTask(w http.ResponseWriter, r *http.Request) {
	if s.deps.Audio == nil {
		writeError(r.Context(), w, errAudioUnavailable)
		return
	}
	var req createAudioTaskRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if req.ArticleID <= 0 {
		writeError(r.Context(), w, apierr.Validation("参数校验失败", []apierr.Details{
			{Field: "articleId", Message: "articleId 必须是正整数"},
		}))
		return
	}
	task, cached, err := s.deps.Audio.EnsureTask(r.Context(), req.ArticleID, req.Voice, req.Speed)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	status := http.StatusAccepted
	if cached {
		status = http.StatusOK
	}
	writeJSON(r.Context(), w, status, newAudioTaskDTO(task))
}

// handleGetAudioTask 处理 GET /api/v1/audio/tasks/{taskId}：查询合成状态。
func (s *Server) handleGetAudioTask(w http.ResponseWriter, r *http.Request) {
	if s.deps.Audio == nil {
		writeError(r.Context(), w, errAudioUnavailable)
		return
	}
	taskID, err := pathInt64(r, "taskId")
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	task, err := s.deps.Audio.GetTask(r.Context(), taskID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	// 状态未终结（pending/processing）时禁止缓存，ready/failed 可短暂缓存。
	cacheControl := "no-store"
	if task != nil && (task.Status == model.TaskStatusReady || task.Status == model.TaskStatusFailed) {
		cacheControl = "private, max-age=5"
	}
	writeJSONWithETag(r.Context(), w, r, http.StatusOK, newAudioTaskDTO(task), cacheControl)
}

// handleRetryAudioTask 处理 POST /api/v1/audio/tasks/{taskId}/retry：重试失败任务。
//
// 重置 retry_count 并重新入队，返回 202。
func (s *Server) handleRetryAudioTask(w http.ResponseWriter, r *http.Request) {
	if s.deps.Audio == nil {
		writeError(r.Context(), w, errAudioUnavailable)
		return
	}
	taskID, err := pathInt64(r, "taskId")
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	task, err := s.deps.Audio.RetryTask(r.Context(), taskID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusAccepted, newAudioTaskDTO(task))
}

// handleGetAudioFile 处理 GET /api/v1/audio/{audioId}：音频文件（支持 Range 断点续传）。
//
// 响应头（openapi §getAudioFile）：
//   - Accept-Ranges: bytes
//   - ETag
//   - Cache-Control: public, max-age=31536000, immutable
//   - 206 时附Content-Range
//
// 条件请求命中 If-None-Match → 304。
func (s *Server) handleGetAudioFile(w http.ResponseWriter, r *http.Request) {
	if s.deps.Audio == nil {
		writeError(r.Context(), w, errAudioUnavailable)
		return
	}
	audioID, err := pathInt64(r, "audioId")
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	audio, absPath, err := s.deps.Audio.OpenAudio(r.Context(), audioID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}

	// 以 ETag 承载"不可变"语义：同一 audioId 的文件内容永不变化，
	// 因此 ETag 直接由 ID 派生，无需读取文件计算哈希（省一次 IO）。
	etag := computeETag([]byte(fmt.Sprintf("audio-%d-%d", audio.ID, audio.SizeBytes)))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", audioFileCacheControl)
	if matchETag(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	file, err := os.Open(absPath)
	if err != nil {
		writeError(r.Context(), w, apierr.NotFound("音频不存在"))
		return
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		writeError(r.Context(), w, apierr.Internal(err))
		return
	}

	// Content-Type 按实际格式推断；未知格式回落为二进制流。
	contentType := audioContentType(audio.Format)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Last-Modified", time.UnixMilli(audio.CreatedAt).UTC().Format(http.TimeFormat))

	// http.ServeContent 原生支持 Range（206）、If-Range 与长度协商，
	// 无需自行解析 Range 头——这是标准库给出的正确实现。
	http.ServeContent(w, r, filepath.Base(absPath), info.ModTime(), file)
}

// ---------------- 内部助手 ----------------

// audioContentType 将音频格式映射为 MIME 类型。
func audioContentType(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "ogg":
		return "audio/ogg"
	case "aac", "m4a":
		return "audio/aac"
	case "flac":
		return "audio/flac"
	case "silk":
		return "audio/silk"
	case "opus":
		return "audio/opus"
	default:
		return "application/octet-stream"
	}
}

// newAudioDTO 由实体构造对外视图。
func newAudioDTO(a *model.Audio) *AudioDTO {
	if a == nil {
		return nil
	}
	dto := &AudioDTO{
		ID:         a.ID,
		ArticleID:  a.ArticleID,
		Voice:      a.Voice,
		Speed:      a.Speed,
		Format:     a.Format,
		DurationMs: a.DurationMs,
		SizeBytes:  a.SizeBytes,
		SampleRate: a.SampleRate,
		URL:        fmt.Sprintf("/api/v1/audio/%d", a.ID),
		CreatedAt:  util.FormatTime(a.CreatedAt),
	}
	if provider := strings.TrimSpace(a.Provider); provider != "" {
		dto.Provider = &provider
	}
	return dto
}

// newAudioTaskDTO 由实体构造对外视图。
func newAudioTaskDTO(t *model.AudioTask) *AudioTaskDTO {
	if t == nil {
		return nil
	}
	dto := &AudioTaskDTO{
		ID:         t.ID,
		ArticleID:  t.ArticleID,
		Voice:      t.Voice,
		Speed:      t.Speed,
		Status:     string(t.Status),
		RetryCount: t.RetryCount,
		TextChars:  t.TextChars,
		CreatedAt:  util.FormatTime(t.CreatedAt),
		UpdatedAt:  util.FormatTime(t.UpdatedAt),
	}
	// AudioID 为 0 表示尚未就绪，输出 null。
	if t.AudioID > 0 {
		id := t.AudioID
		dto.AudioID = &id
	}
	if v := strings.TrimSpace(t.Provider); v != "" {
		dto.Provider = &v
	}
	if v := strings.TrimSpace(t.ErrorCode); v != "" {
		dto.ErrorCode = &v
	}
	if v := strings.TrimSpace(t.ErrorMsg); v != "" {
		dto.ErrorMsg = &v
	}
	return dto
}

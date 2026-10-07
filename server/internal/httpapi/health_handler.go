package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	"github.com/eznews/eznews/internal/apierr"
)

// 本文件实现运维探针：/healthz（存活）、/readyz（就绪）、/metrics（指标）。
// 三个端点**均不鉴权**——它们面向反代/编排系统，而非终端用户。

// readyzTimeout 是就绪探针的数据库探测超时。
// 反代探活通常有秒级超时，本地 SQLite 不应让探针挂住。
const readyzTimeout = 2 * time.Second

// HealthDTO 是 GET /healthz 的响应。
//
// openapi 将 /healthz 定义为"存活探针（不查 DB）"，因此这里只报告进程自身状态
// 与各依赖的**已知快照**，实际探测放在 /readyz。
type HealthDTO struct {
	// Status 恒为 "ok"（进程存活即通过，不因 DB 抖动而重启容器）。
	Status    string `json:"status"`
	UptimeSec int64  `json:"uptimeSec"`
	Version   string `json:"version"`
	// Dependencies 报告各依赖的已知状态快照（不做实时探测）。
	Dependencies map[string]string `json:"dependencies"`
}

// ReadyDTO 是 GET /readyz 的响应。
type ReadyDTO struct {
	// Status 为 "ok" 或 "unavailable"。
	Status string `json:"status"`
	// DB 是数据库探测结果："ok" 或错误摘要。
	DB string `json:"db"`
	// UptimeSec 与 Version 便于反代日志关联。
	UptimeSec int64  `json:"uptimeSec"`
	Version   string `json:"version"`
}

// handleHealthz 处理 GET /healthz：存活探针。
//
// **不查数据库**（openapi 明确"不查 DB"）：容器存活与依赖可用性是两件事，
// 数据库抖动应体现在 /readyz（流量摘除）而不是 /healthz（触发重启）。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	var uptimeSec int64
	deps := map[string]string{
		"ttsQueue": "ok",
		"db":       "unchecked", // 实际探测见 /readyz
	}
	if s.deps.Metrics != nil {
		uptimeSec = int64(time.Since(s.deps.Metrics.StartAt()).Seconds())
	}
	if s.deps.Audio != nil {
		deps["ttsQueue"] = queueStatus(s.deps.Audio.Queue())
	}
	writeJSON(r.Context(), w, http.StatusOK, HealthDTO{
		Status:       "ok",
		UptimeSec:    uptimeSec,
		Version:      Version,
		Dependencies: deps,
	})
}

// handleReadyz 处理 GET /readyz：就绪探针。
//
// 执行 SELECT 1 确认数据库连接可用；失败返回 503，供反代摘除流量。
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	dto := ReadyDTO{
		Status:    "ok",
		DB:        "ok",
		Version:   Version,
		UptimeSec: s.uptimeSec(),
	}
	if s.deps.DB == nil {
		dto.Status = "unavailable"
		dto.DB = "not_configured"
		writeJSON(r.Context(), w, http.StatusServiceUnavailable, dto)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), readyzTimeout)
	defer cancel()
	if err := s.deps.DB.Ping(ctx); err != nil {
		dto.Status = "unavailable"
		dto.DB = sanitizeProbeError(err)
		s.logger.WarnContext(r.Context(), "就绪探针失败",
			slog.String("err", err.Error()),
			slog.String("requestId", apierr.RequestIDFromContext(r.Context())))
		writeJSON(r.Context(), w, http.StatusServiceUnavailable, dto)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, dto)
}

// handleMetrics 处理 GET /metrics：简易JSON 指标（P1，无 Prometheus 依赖）。
//
// 字段与 openapi /metrics 一致；另附 uptimeSec 与 version 便于排障。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.deps.Metrics == nil {
		writeJSON(r.Context(), w, http.StatusOK, map[string]any{
			"status":  "unavailable",
			"version": Version,
		})
		return
	}
	snapshot := s.deps.Metrics.Snapshot()
	// 文章总数是 DB派生值，惰性刷新后写入（避免 ingest 每条都 UPDATE 计数）。
	if s.deps.Articles != nil && s.deps.DB != nil {
		if count, err := s.countArticles(r.Context()); err == nil {
			s.deps.Metrics.SetArticles(count)
			snapshot["articles"] = count
		}
	}
	snapshot["version"] = Version
	snapshot["goroutines"] = runtime.NumGoroutine()
	// 指标不应被缓存，否则采集侧会读到陈旧值。
	writeJSONWithETag(r.Context(), w, r, http.StatusOK, snapshot, "no-store")
}

// ---------------- 内部助手 ----------------

// queueStatus 报告 TTS 队列水位（满载时提示，便于定位 429 来源）。
func queueStatus(queue <-chan int64) string {
	depth := len(queue)
	switch {
	case depth == 0:
		return "idle"
	case depth < cap(queue)/2:
		return "ok"
	default:
		return "busy"
	}
}

// uptimeSec 返回进程运行时长（秒）。
func (s *Server) uptimeSec() int64 {
	if s.deps.Metrics == nil {
		return 0
	}
	return int64(time.Since(s.deps.Metrics.StartAt()).Seconds())
}

// countArticles 统计文章总数（供 /metrics）。
func (s *Server) countArticles(ctx context.Context) (int64, error) {
	if s.deps.DB == nil {
		return 0, nil
	}
	row := s.deps.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM article")
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// sanitizeProbeError 将探针错误压缩为简短摘要（不外泄底层细节给公网）。
func sanitizeProbeError(err error) string {
	if err == nil {
		return "ok"
	}
	msg := err.Error()
	const maxLen = 120
	if len(msg) > maxLen {
		msg = msg[:maxLen]
	}
	return "error: " + msg
}

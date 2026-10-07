package httpapi

import (
	"net/http"
	"strings"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/model"
)

// ingestVerboseNone / ingestVerboseAll 对应 openapi 的 ?verbose=none|all。
const (
	ingestVerboseNone = "none"
	ingestVerboseAll  = "all"
)

// handleIngestArticle 处理 POST /api/v1/ingest/articles：提交单篇文章。
//
// 请求体就是一个 ArticleIngestItem（不是数组），内部包装为长度 1 的批次
// 复用 IngestBatch，保证单条与批量的去重/回执语义**完全一致**。
//
// 鉴权：X-Api-Key（路由已挂 apiKeyAuth 中间件）。
func (s *Server) handleIngestArticle(w http.ResponseWriter, r *http.Request) {
	if s.deps.Ingest == nil {
		writeError(r.Context(), w, errIngestUnavailable)
		return
	}
	var item model.IngestItem
	if err := decodeJSON(r, &item); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	verbose, err := parseVerbose(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	receipt, err := s.deps.Ingest.IngestBatch(r.Context(), []model.IngestItem{item}, verbose)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, receipt)
}

// handleIngestBatch 处理 POST /api/v1/ingest/articles/batch：批量提交文章。
//
// 单事务批写，逐条判定回执。整批失败（鉴权/校验/限流/体量）返回非 2xx；
// 单条业务失败不影响其他条目，体现在 data.results 中。
func (s *Server) handleIngestBatch(w http.ResponseWriter, r *http.Request) {
	if s.deps.Ingest == nil {
		writeError(r.Context(), w, errIngestUnavailable)
		return
	}
	verbose, err := parseVerbose(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}

	var req model.IngestBatchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if len(req.Items) == 0 {
		writeError(r.Context(), w, apierr.Validation("items 不能为空", []apierr.Details{
			{Field: "items", Message: "至少需要 1 个条目"},
		}))
		return
	}
	// 批量上限来自配置（默认 200），超限返回 413 提示拆分批次。
	if max := s.cfg.Ingest.MaxBatchItems; max > 0 && len(req.Items) > max {
		writeError(r.Context(), w, apierr.PayloadTooLarge("items 数量超过上限 "+itoa(max)))
		return
	}

	receipt, err := s.deps.Ingest.IngestBatch(r.Context(), req.Items, verbose)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, receipt)
}

// parseVerbose 解析 ?verbose=none|all（默认 none：results 只含 failed 明细）。
func parseVerbose(r *http.Request) (bool, error) {
	raw := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("verbose")))
	switch raw {
	case "", ingestVerboseNone:
		return false, nil
	case ingestVerboseAll:
		return true, nil
	default:
		return false, apierr.Validation("查询参数非法", []apierr.Details{
			{Field: "verbose", Message: "只能是 none 或 all"},
		})
	}
}

// errIngestUnavailable 表示 ingest 服务未注入。
var errIngestUnavailable = apierr.New(http.StatusServiceUnavailable, apierr.CodeInternal,
	"ingest 服务未就绪")

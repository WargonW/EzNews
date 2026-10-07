package httpapi

import (
	"net/http"
	"strings"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/service"
	"github.com/eznews/eznews/internal/util"
)

// SourceDTO 是新闻源对外视图（openapi SourceDTO）。
type SourceDTO struct {
	ID       int64  `json:"id"`
	Key      string `json:"key"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Type     string `json:"type"`
	Category string `json:"category"`
	// IsDefault 标记系统默认源：不可删除，只能停用。
	IsDefault bool `json:"isDefault"`
	Enabled   bool `json:"enabled"`
	// SuggestInterval 是建议采集间隔（秒），仅供外部采集器参考。
	SuggestInterval int     `json:"suggestInterval"`
	IconURL         *string `json:"iconUrl"`
	Language        *string `json:"language"`
	Remark          *string `json:"remark"`
	ArticleCount    int64   `json:"articleCount"`
	CreatedAt       string  `json:"createdAt"`
	UpdatedAt       string  `json:"updatedAt"`
}

// SourcesDTO 是 GET /api/v1/sources 的响应。
type SourcesDTO struct {
	Items []SourceDTO `json:"items"`
}

// sourceInputRequest 是 POST/PUT /api/v1/sources 的请求体（openapi SourceInput）。
//
// Enabled 用 *bool 以区分"未提供"与"显式 false"：
// 默认源仅允许修改 enabled，此时其他字段缺省是合法输入。
type sourceInputRequest struct {
	Name            string  `json:"name"`
	URL             string  `json:"url"`
	Type            string  `json:"type"`
	Category        string  `json:"category"`
	SuggestInterval int     `json:"suggestInterval"`
	IconURL         *string `json:"iconUrl"`
	Language        *string `json:"language"`
	Remark          *string `json:"remark"`
	Enabled         *bool   `json:"enabled"`
}

// toInput 转换为 service 层入参。
func (r sourceInputRequest) toInput() service.SourceInput {
	return service.SourceInput{
		Name:            r.Name,
		URL:             r.URL,
		Type:            r.Type,
		Category:        r.Category,
		SuggestInterval: r.SuggestInterval,
		IconURL:         deref(r.IconURL),
		Language:        deref(r.Language),
		Remark:          deref(r.Remark),
		Enabled:         r.Enabled,
	}
}

// sourceEnabledRequest 是 PATCH /api/v1/sources/{id}/enabled 的请求体。
type sourceEnabledRequest struct {
	Enabled *bool `json:"enabled"`
}

// handleListSources 处理 GET /api/v1/sources：源列表。
//
// openapi 的 includeDisabled 默认值为 true（与文章列表相反）：
// 源管理页需要看到停用源，否则用户无法重新启用。
func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	if s.deps.Sources == nil {
		writeError(r.Context(), w, errSourcesUnavailable)
		return
	}
	includeDisabled, err := queryBool(r, "includeDisabled", true)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	list, err := s.deps.Sources.List(r.Context(), includeDisabled)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	items := make([]SourceDTO, 0, len(list))
	for _, src := range list {
		items = append(items, newSourceDTO(src))
	}
	// 源配置变化不频繁，短缓存 30s。
	writeJSONWithETag(r.Context(), w, r, http.StatusOK, SourcesDTO{Items: items},
		"public, max-age=30")
}

// handleCreateSource 处理 POST /api/v1/sources：新增自定义源。
//
// key 由服务端按名称生成稳定业务键（openapi SourceInput 不接受 key）。
func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request) {
	if s.deps.Sources == nil {
		writeError(r.Context(), w, errSourcesUnavailable)
		return
	}
	var req sourceInputRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(r.Context(), w, apierr.Validation("源参数校验失败", []apierr.Details{
			{Field: "name", Message: "name 不能为空"},
		}))
		return
	}
	src, err := s.deps.Sources.Create(r.Context(), req.toInput())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusCreated, newSourceDTO(derefSource(src)))
}

// handleUpdateSource 处理 PUT /api/v1/sources/{id}：编辑源。
//
// 默认源仅允许修改 enabled，其余字段的修改由 service 层拒绝（400 VALIDATION_FAILED）。
func (s *Server) handleUpdateSource(w http.ResponseWriter, r *http.Request) {
	if s.deps.Sources == nil {
		writeError(r.Context(), w, errSourcesUnavailable)
		return
	}
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	var req sourceInputRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	src, err := s.deps.Sources.Update(r.Context(), id, req.toInput())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, newSourceDTO(derefSource(src)))
}

// handleSetSourceEnabled 处理 PATCH /api/v1/sources/{id}/enabled：启停源。
//
// 停用后其文章默认不进入列表（ArticleService.IncludeDisabled 控制）。
func (s *Server) handleSetSourceEnabled(w http.ResponseWriter, r *http.Request) {
	if s.deps.Sources == nil {
		writeError(r.Context(), w, errSourcesUnavailable)
		return
	}
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	var req sourceEnabledRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if req.Enabled == nil {
		writeError(r.Context(), w, apierr.Validation("参数校验失败", []apierr.Details{
			{Field: "enabled", Message: "enabled 不能为空"},
		}))
		return
	}
	src, err := s.deps.Sources.SetEnabled(r.Context(), id, *req.Enabled)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, newSourceDTO(derefSource(src)))
}

// handleDeleteSource 处理 DELETE /api/v1/sources/{id}：删除自定义源。
//
// 默认源不可删除 → 409 CONFLICT（由 service 层判定）。
func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	if s.deps.Sources == nil {
		writeError(r.Context(), w, errSourcesUnavailable)
		return
	}
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if err := s.deps.Sources.Delete(r.Context(), id); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, map[string]any{
		"id":      id,
		"deleted": true,
	})
}

// newSourceDTO 由实体构造对外视图。
func newSourceDTO(src model.Source) SourceDTO {
	dto := SourceDTO{
		ID:              src.ID,
		Key:             src.Key,
		Name:            src.Name,
		URL:             src.URL,
		Type:            string(src.Type),
		Category:        src.Category,
		IsDefault:       src.IsDefault,
		Enabled:         src.Enabled,
		SuggestInterval: src.SuggestInterval,
		ArticleCount:    src.ArticleCount,
		CreatedAt:       util.FormatTime(src.CreatedAt),
		UpdatedAt:       util.FormatTime(src.UpdatedAt),
	}
	if icon := strings.TrimSpace(src.IconURL); icon != "" {
		dto.IconURL = &icon
	}
	if lang := strings.TrimSpace(src.Language); lang != "" {
		dto.Language = &lang
	}
	if remark := strings.TrimSpace(src.Remark); remark != "" {
		dto.Remark = &remark
	}
	return dto
}

// derefSource 解引用，nil 时返回零值（避免 handler panic）。
func derefSource(src *model.Source) model.Source {
	if src == nil {
		return model.Source{}
	}
	return *src
}

// errSourcesUnavailable 表示源服务未注入。
var errSourcesUnavailable = apierr.New(http.StatusServiceUnavailable, apierr.CodeInternal,
	"源服务未就绪")

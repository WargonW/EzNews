// 本文件是管理后台的 HTTP 接入层。
//
// 分层约定不变：这里只做「解析 → 调 service → 序列化」。
// 权限判定、最后管理员守卫、停用连带吊销会话等规则**全在 service 层**，
// 本文件一处都不重复。理由很简单：规则写在 handler 里，就意味着
// 多加一条路由就可能漏掉一条守卫，而漏掉的那些守卫正是不可逆的那几条。

package httpapi

import (
	"net/http"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/auth"
	"github.com/eznews/eznews/internal/service"
)

// adminActorID 取出当前操作者 ID。
//
// ★ 取不到就是 500，不是"当成游客继续"。管理端点只会.requireAdmin 包裹，
//
//	走到这里时上下文中必然有 userID；若没有，说明中间件装配出了错 ——
//	这种时候向下走会以"系统自己"的身份执行破坏性操作，比直接报错糟得多。
func adminActorID(r *http.Request) (int64, error) {
	id, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		return 0, apierr.Internal(errNoActor)
	}
	return id, nil
}

// adminUser是从 service 的管理用户 DTO 到 HTTP 的直接透传。
//
// 刻意不重新定义一个带 json tag 的结构体：两边字段一旦不同步，
// 就会出现"接口明明返回了 canDelete，前端收到的却是 undefined"这类
// 层层透传却静默丢字段的问题。service 的 DTO 就是契约本身。
type adminUser = service.AdminUserDTO

// ---- GET /api/v1/admin/overview ----

func (s *Server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	if s.deps.Admin == nil {
		writeError(r.Context(), w, errAdminDisabled)
		return
	}
	data, err := s.deps.Admin.Overview(r.Context())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, data)
}

// ---- GET /api/v1/admin/users ----

func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	if s.deps.Admin == nil {
		writeError(r.Context(), w, errAdminDisabled)
		return
	}
	actorID, err := adminActorID(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	limit, err := queryInt(r, "limit", 20, 1, 100)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	// cursor 是用户主键的字符串形式；非法值回落到第一页而不是报错 ——
	// 翻页游标来自 URL，用户可能手改或存了旧书签，报错反而是坏体验。
	//
	// ★ 这与上面 limit 的处理**刻意不同**，不是疏漏：
	//   - limit 决定"返回多少行"，静默截断会让客户端误以为拿到了请求的数量，
	//     所以越界必须 400（见 response.go 的 queryInt）。
	//   - cursor 只决定"从哪里开始"，非法就回第一页，最坏结果是多翻一页，无害。
	//   同理 role 非法必须 400：筛选条件拼错却静默返回空列表，会被误读成"系统很健康"。
	cursor, _ := queryInt64(r, "cursor", 0)
	if cursor < 0 {
		cursor = 0
	}
	data, err := s.deps.Admin.ListUsers(r.Context(), actorID,
		r.URL.Query().Get("q"), r.URL.Query().Get("role"), limit, cursor)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, data)
}

// ---- POST /api/v1/admin/users ----

// adminCreateUserRequest 是管理员建号的请求体。
type adminCreateUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Email    string `json:"email"`
	Role     string `json:"role"`
}

func (s *Server) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	if s.deps.Admin == nil {
		writeError(r.Context(), w, errAdminDisabled)
		return
	}
	actorID, err := adminActorID(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	var req adminCreateUserRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	data, err := s.deps.Admin.CreateUser(r.Context(), actorID, service.CreateUserInput{
		Username: req.Username,
		Password: req.Password,
		Email:    req.Email,
		Role:     req.Role,
	})
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusCreated, data)
}

// ---- GET /api/v1/admin/users/{id} ----

func (s *Server) handleAdminGetUser(w http.ResponseWriter, r *http.Request) {
	if s.deps.Admin == nil {
		writeError(r.Context(), w, errAdminDisabled)
		return
	}
	actorID, err := adminActorID(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	data, err := s.deps.Admin.GetUser(r.Context(), actorID, id)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, data)
}

// ---- PATCH /api/v1/admin/users/{id}/role ----

// adminRoleRequest 是改角色的请求体。
//
// Role 用 string 而非 *string：本端点唯一的作用就是它，缺字段没有"保留原值"的语义。
type adminRoleRequest struct {
	Role string `json:"role"`
}

func (s *Server) handleAdminSetUserRole(w http.ResponseWriter, r *http.Request) {
	if s.deps.Admin == nil {
		writeError(r.Context(), w, errAdminDisabled)
		return
	}
	actorID, err := adminActorID(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	var req adminRoleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	data, err := s.deps.Admin.SetRole(r.Context(), actorID, id, req.Role)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, data)
}

// ---- PATCH /api/v1/admin/users/{id}/disabled ----

// adminDisabledRequest 是停用/启用的请求体。
//
// ★ Disabled 必须是 *bool：这里存在"显式 false"的合法语义（启用账号）。
//
//	用裸 bool 会把客户端漏传 body 与显式传 {"disabled":false} 混为一谈，
//	于是"想启用却没传字段"会被当成"要停用"—— 方向反了且不可逆。
type adminDisabledRequest struct {
	Disabled *bool `json:"disabled"`
}

func (s *Server) handleAdminSetUserDisabled(w http.ResponseWriter, r *http.Request) {
	if s.deps.Admin == nil {
		writeError(r.Context(), w, errAdminDisabled)
		return
	}
	actorID, err := adminActorID(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	var req adminDisabledRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if req.Disabled == nil {
		writeError(r.Context(), w, apierr.Validation("请求体校验失败",
			[]apierr.Details{{Field: "disabled", Message: "不能为空"}}))
		return
	}
	data, err := s.deps.Admin.SetDisabled(r.Context(), actorID, id, *req.Disabled)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, data)
}

// ---- DELETE /api/v1/admin/users/{id} ----

func (s *Server) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	if s.deps.Admin == nil {
		writeError(r.Context(), w, errAdminDisabled)
		return
	}
	actorID, err := adminActorID(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if err := s.deps.Admin.DeleteUser(r.Context(), actorID, id); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeNoContent(w)
}

// ---- POST /api/v1/admin/users/{id}/sessions/revoke-all ----

// adminRevokedDTO 是吊销全部会话的响应。
type adminRevokedDTO struct {
	Revoked int64 `json:"revoked"`
}

func (s *Server) handleAdminRevokeAllSessions(w http.ResponseWriter, r *http.Request) {
	if s.deps.Admin == nil {
		writeError(r.Context(), w, errAdminDisabled)
		return
	}
	actorID, err := adminActorID(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	n, err := s.deps.Admin.RevokeAllSessions(r.Context(), actorID, id)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, adminRevokedDTO{Revoked: n})
}

// ---- GET /api/v1/admin/stats/content ----

func (s *Server) handleAdminContentStats(w http.ResponseWriter, r *http.Request) {
	if s.deps.Admin == nil {
		writeError(r.Context(), w, errAdminDisabled)
		return
	}
	data, err := s.deps.Admin.ContentStats(r.Context())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, data)
}

// ---- GET /api/v1/admin/audio/tasks ----

func (s *Server) handleAdminListAudioTasks(w http.ResponseWriter, r *http.Request) {
	if s.deps.Admin == nil {
		writeError(r.Context(), w, errAdminDisabled)
		return
	}
	limit, err := queryInt(r, "limit", 20, 1, 200)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	cursor, _ := queryInt64(r, "cursor", 0)
	if cursor < 0 {
		cursor = 0
	}
	data, err := s.deps.Admin.AudioTasks(r.Context(), r.URL.Query().Get("status"), int64(limit), cursor)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, data)
}

// ---- POST /api/v1/admin/audio/tasks/{taskId}/retry ----

func (s *Server) handleAdminRetryAudioTask(w http.ResponseWriter, r *http.Request) {
	if s.deps.Admin == nil {
		writeError(r.Context(), w, errAdminDisabled)
		return
	}
	actorID, err := adminActorID(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	taskID, err := pathInt64(r, "taskId")
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	data, err := s.deps.Admin.RetryAudioTask(r.Context(), actorID, taskID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, data)
}

// ---- GET /api/v1/admin/system ----

func (s *Server) handleAdminSystem(w http.ResponseWriter, r *http.Request) {
	if s.deps.Admin == nil {
		writeError(r.Context(), w, errAdminDisabled)
		return
	}
	data, err := s.deps.Admin.SystemInfo(r.Context())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, data)
}

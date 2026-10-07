package httpapi

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/auth"
	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/service"
	"github.com/eznews/eznews/internal/util"
)

// 本文件实现 /api/v1/me/** 全部端点，全部由 RequireAuth 包裹。
//
// ★ 强制约束：user_id **只能**来自 access token（auth.UserIDFromContext），
// 绝不接受请求体/查询参数中的 user_id —— 否则任何人都能读写他人数据。

// handleGetMe 处理 GET /api/v1/me：返回当前账号基本信息。
func (s *Server) handleGetMe(w http.ResponseWriter, r *http.Request) {
	userID, err := userIDFromContext(r.Context())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if s.deps.Auth == nil {
		writeError(r.Context(), w, errAuthUnavailable)
		return
	}
	user, err := s.deps.Auth.GetUser(r.Context(), userID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	// 账号信息带 ETag：客户端可用 If-None-Match 避免重复传输。
	// 但属用户私有数据，缓存指令必须是 private。
	writeJSONWithETag(r.Context(), w, r, http.StatusOK, newUserDTO(user), accountCacheControl)
}

// handleDeleteMe 处理 DELETE /api/v1/me：注销账号（DEC-8 四条硬约束）。
//
//  1. 必须携带当前密码二次确认（handler 只做非空校验，比对由 service 完成）；
//  2. 级联删除范围由 service 保证，绝不触碰 article/source/audio；
//  3. 成功返回 204；
//  4. 不可逆。
func (s *Server) handleDeleteMe(w http.ResponseWriter, r *http.Request) {
	userID, err := userIDFromContext(r.Context())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	var req DeleteAccountRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if req.Password == "" {
		writeError(r.Context(), w, apierr.Validation("注销参数校验失败", []apierr.Details{
			{Field: "password", Message: "password 不能为空（二次确认）"},
		}))
		return
	}
	if s.deps.Auth == nil {
		writeError(r.Context(), w, errAuthUnavailable)
		return
	}
	if err := s.deps.Auth.DeleteAccount(r.Context(), userID, req.Password); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	s.logger.InfoContext(r.Context(), "账号注销成功",
		slog.Int64("userId", userID),
		slog.String("requestId", apierr.RequestIDFromContext(r.Context())))
	s.clearRefreshCookie(w)
	writeNoContent(w)
}

// handleChangePassword 处理 PATCH /api/v1/me/password。
//
// ★ 契约漂移（见交付报告）：openapi 声明「响应同时下发新的一对 token」，
// 但 service.AuthService.ChangePassword 只返回 error——它在内部
// BumpTokenVersion + RevokeAllSessions，**刻意**不签发新令牌
// （安全上更严格：改密后所有设备都必须重新登录，客户端拿旧密码换新令牌
// 等于把「改密」退化为「静默续期」）。
// handler 因此返回 200 + 空 data，语义为「已改密，请重新登录」。
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	userID, err := userIDFromContext(r.Context())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	var req ChangePasswordRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if err := req.validate(); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if req.OldPassword == req.NewPassword {
		writeError(r.Context(), w, apierr.Validation("改密参数校验失败", []apierr.Details{
			{Field: "newPassword", Message: "新密码不能与旧密码相同"},
		}))
		return
	}
	if s.deps.Auth == nil {
		writeError(r.Context(), w, errAuthUnavailable)
		return
	}
	// currentSessionID 来自 JWT 的 sid（DEC-14）。改密后 service 会为当前设备补发新令牌，
	// 其他设备全部被踢下线；sid 缺失时 service 不签发，前端按 reloginRequired 提示重登。
	sid, _ := auth.SessionIDFromContext(r.Context())
	res, err := s.deps.Auth.ChangePassword(r.Context(), userID, sid, req.OldPassword, req.NewPassword)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	s.logger.InfoContext(r.Context(), "密码修改成功，其他设备会话已全部撤销",
		slog.Int64("userId", userID),
		slog.Int64("currentSessionId", sid),
		slog.String("requestId", apierr.RequestIDFromContext(r.Context())))

	// 未拿到新令牌（sid 缺失或建后继会话失败）时，明确告知客户端需要重新登录。
	if res == nil || res.AccessToken == "" {
		s.clearRefreshCookie(w)
		writeJSONWithETag(r.Context(), w, r, http.StatusOK,
			map[string]any{"reloginRequired": true}, accountCacheControl)
		return
	}
	// 换发新 refresh token 时必须同步轮换 Cookie，否则浏览器里那条旧的还能继续用。
	// 复用 auth_handler 的 newRefreshCookie，保证与登录/刷新时的属性完全一致
	// （Path/HttpOnly/SameSite/Secure 任一处不一致，浏览器就会残留两条同名 Cookie，
	//  之后 logout 只能清掉其中一条，另一条继续有效 —— 登出又变成"形同虚设"）。
	if s.cfg.Auth.RefreshCookie && res.RefreshToken != "" {
		http.SetCookie(w, s.newRefreshCookie(res.RefreshToken))
	} else {
		s.clearRefreshCookie(w)
	}
	// 用强类型 DTO 而非 map：map 无法表达 omitempty，
	// refreshToken 在 Cookie 通道下会是"" 而非 null，客户端会误判为"需要自己存 refresh token"。
	writeJSONWithETag(r.Context(), w, r, http.StatusOK, TokenResponse{
		AccessToken:  res.AccessToken,
		RefreshToken: strPtrOrNil(res.RefreshToken),
		ExpiresIn:    res.ExpiresIn,
		SessionID:    res.SessionID,
		NeedsLogin:   false,
	}, accountCacheControl)
}

// handleMerge 处理 POST /api/v1/me/merge：游客数据幂等合并。
//
// 登录后由客户端**自动**发起，无弹窗。幂等键 = (user_id, client_id, nonce)，
// 由 service 层在 merge_log 上保证；handler 只透传。
//
// 失败不影响登录态：客户端静默降级本地态并退避重试。
func (s *Server) handleMerge(w http.ResponseWriter, r *http.Request) {
	userID, err := userIDFromContext(r.Context())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	var req MergeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if err := validateMergeRequest(&req); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if s.deps.Merge == nil {
		writeError(r.Context(), w, errMergeUnavailable)
		return
	}
	res, err := s.deps.Merge.Merge(r.Context(), userID, req)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	s.logger.InfoContext(r.Context(), "游客数据合并完成",
		slog.Int64("userId", userID),
		slog.Bool("replayed", res != nil && res.Replayed),
		slog.String("requestId", apierr.RequestIDFromContext(r.Context())))
	writeJSON(r.Context(), w, http.StatusOK, newMergeResult(res))
}

// handleListFavorites 处理 GET /api/v1/me/favorites：收藏增量列表。
//
// ★ DEC-11：墓碑项必须随流返回，客户端据此本地删除。
// ★ withArticles（默认 true）：一次 IN 查询为每条带上title/summary/sourceName/publishedAt，
// 前端无需逐条 GET /articles/{id}。墓碑项同样带摘要（文章还在库时），
// 以便渲染"文章已删除"而非空白行。
func (s *Server) handleListFavorites(w http.ResponseWriter, r *http.Request) {
	userID, err := userIDFromContext(r.Context())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	since, limit, withArticles, err := parseStateQuery(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if s.deps.State == nil {
		writeError(r.Context(), w, errStateUnavailable)
		return
	}
	page, err := s.deps.State.ListFavoritesDelta(r.Context(), userID, since, limit)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if page == nil {
		writeError(r.Context(), w, errNilStatePage)
		return
	}
	ids := make([]int64, 0, len(page.Items))
	for _, it := range page.Items {
		ids = append(ids, it.ArticleID)
	}
	briefs := s.loadStateBriefs(r, ids, withArticles)
	writeJSON(r.Context(), w, http.StatusOK,
		newFavoritePage(page.Items, page.NextCursor, page.HasMore, util.NowMs(), briefs))
}

// handleSetFavorite 处理 POST /api/v1/me/favorites：收藏 / 取消收藏。
//
// 取消写墓碑（deleted=true），**不做物理删除** —— 否则跨端无法同步"对方已取消"。
func (s *Server) handleSetFavorite(w http.ResponseWriter, r *http.Request) {
	userID, err := userIDFromContext(r.Context())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	req, err := decodeStateRequest(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if s.deps.State == nil {
		writeError(r.Context(), w, errStateUnavailable)
		return
	}
	if err := s.requireArticleExists(r, req.ArticleID); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	// ★ 参数取反：传输层用 deleted（墓碑）表达「取消」，
	// service 用 favorite（正向语义）表达同一件事。
	view, err := s.deps.State.SetFavorite(r.Context(), userID, req.ArticleID, !req.Deleted)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, newStateItemFromFavorite(view))
}

// handleListReads 处理 GET /api/v1/me/reads：已读增量列表（含墓碑）。
//
// withArticles 语义与 /me/favorites 完全一致（复用同一个 StateItem 结构）。
func (s *Server) handleListReads(w http.ResponseWriter, r *http.Request) {
	userID, err := userIDFromContext(r.Context())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	since, limit, withArticles, err := parseStateQuery(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if s.deps.State == nil {
		writeError(r.Context(), w, errStateUnavailable)
		return
	}
	page, err := s.deps.State.ListReadsDelta(r.Context(), userID, since, limit)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if page == nil {
		writeError(r.Context(), w, errNilStatePage)
		return
	}
	ids := make([]int64, 0, len(page.Items))
	for _, it := range page.Items {
		ids = append(ids, it.ArticleID)
	}
	briefs := s.loadStateBriefs(r, ids, withArticles)
	writeJSON(r.Context(), w, http.StatusOK,
		newReadPage(page.Items, page.NextCursor, page.HasMore, util.NowMs(), briefs))
}

// handleSetRead 处理 POST /api/v1/me/reads：标记已读 / 取消已读。
func (s *Server) handleSetRead(w http.ResponseWriter, r *http.Request) {
	userID, err := userIDFromContext(r.Context())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	req, err := decodeStateRequest(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if s.deps.State == nil {
		writeError(r.Context(), w, errStateUnavailable)
		return
	}
	if err := s.requireArticleExists(r, req.ArticleID); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	// ★ 参数取反，同 handleSetFavorite。
	view, err := s.deps.State.SetRead(r.Context(), userID, req.ArticleID, !req.Deleted)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, newStateItemFromRead(view))
}

// handleBatchSetState 处理 POST /api/v1/me/state/batch：批量标记已读 / 收藏。
//
// 典型场景是阅读列表里的"全部标记已读"—— 客户端在本地攒了最多 500 条 id，
// 一次提交即可，不必串行发500 次 POST /me/reads（那既是500 次往返，
// 也让用户看到"标一半就停"的中间态）。
//
// ★ 原子性由 service 的单事务实现保证：中途任一条失败则整批回滚。
// 本 handler 因此**不会**返回逐条的部分失败清单，客户端也无需处理
// "部分成功"：要么 200 且两个计数都落库，要么整批没生效、可安全重试。
//
// ★ 先校验文章存在性再进 service（而不是靠 FK 约束在事务里炸）：
// FK 违例会被 service 包成 500，而"传了不存在的文章 id"是客户端错误、
// 应当是 404。提前校验还让整个批次不必开启事务就失败，省掉一次写锁。
func (s *Server) handleBatchSetState(w http.ResponseWriter, r *http.Request) {
	userID, err := userIDFromContext(r.Context())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	var req BatchStateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if err := req.validate(); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if s.deps.State == nil {
		writeError(r.Context(), w, errStateUnavailable)
		return
	}
	if err := s.requireArticlesExist(r, req.ArticleIDs); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	// 传输层用"正向语义"的 read/favorite（service 也是），
	// 无需像单条接口那样取反——取反只在 deleted 墓碑语义下才需要。
	res, err := s.deps.State.BatchSet(r.Context(), userID, service.BatchSetStateRequest{
		ArticleIDs: req.ArticleIDs,
		Read:       *req.Read,
		Favorite:   req.Favorite,
	})
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	// 先映射再打日志：newBatchStateResult 对 nil 做了兜底，
	// 直接解引用 res 写日志等于在一行日志上引入一个 panic 点。
	out := newBatchStateResult(res)
	s.logger.InfoContext(r.Context(), "批量标记用户状态完成",
		slog.Int64("userId", userID),
		slog.Int("requested", len(req.ArticleIDs)),
		slog.Int("readsChanged", out.ReadsChanged),
		slog.Int("favoritesChanged", out.FavoritesChanged),
		slog.Bool("touchedFavorite", req.Favorite != nil),
		slog.String("requestId", apierr.RequestIDFromContext(r.Context())))
	writeJSON(r.Context(), w, http.StatusOK, out)
}

// handleGetPreferences 处理 GET /api/v1/me/preferences：拉取偏好 KV 全量。
func (s *Server) handleGetPreferences(w http.ResponseWriter, r *http.Request) {
	userID, err := userIDFromContext(r.Context())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if s.deps.State == nil {
		writeError(r.Context(), w, errStateUnavailable)
		return
	}
	views, err := s.deps.State.GetPrefs(r.Context(), userID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSONWithETag(r.Context(), w, r, http.StatusOK, newPreferencesDTO(views), accountCacheControl)
}

// handlePutPreferences 处理 PUT /api/v1/me/preferences：整包覆盖偏好 KV。
//
// 整包覆盖 + 服务端为准，天然幂等（openapi 明确语义）。
func (s *Server) handlePutPreferences(w http.ResponseWriter, r *http.Request) {
	userID, err := userIDFromContext(r.Context())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	var req PutPreferencesRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if err := req.validate(); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if s.deps.State == nil {
		writeError(r.Context(), w, errStateUnavailable)
		return
	}
	// ReplacePrefs 内部会做键名/条目上限校验并整包事务写入（DELETE + INSERT 同事务）。
	views, err := s.deps.State.ReplacePrefs(r.Context(), userID, req.Preferences)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, newPreferencesDTO(views))
}

// handleListSessions 处理 GET /api/v1/me/sessions：活跃会话列表。
//
// current 标记由 service 依据 JWT 的 sid 计算，handler 只需传入当前会话 ID。
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	userID, err := userIDFromContext(r.Context())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if s.deps.Auth == nil {
		writeError(r.Context(), w, errAuthUnavailable)
		return
	}
	currentID, _ := auth.SessionIDFromContext(r.Context())
	views, err := s.deps.Auth.ListSessions(r.Context(), userID, currentID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, newSessionDTOs(views))
}

// handleRevokeSession 处理 DELETE /api/v1/me/sessions/{id}：踢下线指定会话。
func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	userID, err := userIDFromContext(r.Context())
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	sessionID, err := pathInt64(r, "id")
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if s.deps.Auth == nil {
		writeError(r.Context(), w, errAuthUnavailable)
		return
	}
	// RevokeSessionByID 内部校验 session.user_id == userID，无法踢他人会话。
	if err := s.deps.Auth.RevokeSessionByID(r.Context(), userID, sessionID); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	s.logger.InfoContext(r.Context(), "会话已撤销",
		slog.Int64("userId", userID),
		slog.Int64("sessionId", sessionID))
	writeJSON(r.Context(), w, http.StatusOK, map[string]any{
		"id":        sessionID,
		"revoked":   true,
		"revokedAt": util.NowMs(),
	})
}

// ---------------- 内部助手 ----------------

// parseStateQuery 解析收藏/已读列表的 since、limit 与 withArticles 参数。
func parseStateQuery(r *http.Request) (model.Cursor, int, bool, error) {
	since, err := model.DecodeCursor(queryString(r, "since", ""))
	if err != nil {
		return model.ZeroCursor, 0, false, apierr.Validation("游标非法", []apierr.Details{
			{Field: "since", Message: "since 格式非法"},
		})
	}
	limit, err := queryInt(r, "limit", defaultStateLimit, 1, maxStateLimit)
	if err != nil {
		return model.ZeroCursor, 0, false, err
	}
	return since, limit, parseWithArticles(r), nil
}

// parseWithArticles 解析 withArticles 查询参数（默认 true = 附带文章摘要）。
//
// ★ 刻意**不**用 queryBool：那个函数对非法值返回 400，而这里要求静默回落默认。
// 客户端拼错参数名/传了 "maybe" 时，让它仍能拿到数据比报 400 更有用 ——
// 摘要是附加信息，缺了不影响墓碑同步与增量游标语义。
func parseWithArticles(r *http.Request) bool {
	raw := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("withArticles")))
	switch raw {
	case "", "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return true // 非法值 → 默认 true，不报错
	}
}

// loadStateBriefs 批量取回一页状态项的文章摘要（withArticles=false 时返回 nil）。
//
// 传入的 ids 允许重复，repo 内部会去重；查不到的 article 行不会出现在返回 map 中。
// 摘要查询失败不阻断主流程：降级为只返回 3 个基础字段，
// 增量同步与墓碑语义不受影响（记 warn 便于排查）。
func (s *Server) loadStateBriefs(r *http.Request, ids []int64, withArticles bool) map[int64]model.ArticleBrief {
	if !withArticles || s.deps.Articles == nil || len(ids) == 0 {
		return nil
	}
	briefs, err := s.deps.Articles.FindBriefs(r.Context(), ids)
	if err != nil {
		s.logger.WarnContext(r.Context(), "批量查询状态项文章摘要失败，降级为不含摘要",
			slog.String("path", r.URL.Path),
			slog.Int("count", len(ids)),
			slog.String("err", err.Error()))
		return nil
	}
	return briefs
}

// decodeStateRequest 解析收藏/已读写入请求。
//
// 注意：user_id **不**从请求体读取（见文件头说明），
// 因此这里没有 UserID 字段，调用方一律用 token 里的 userID。
func decodeStateRequest(r *http.Request) (SetStateRequest, error) {
	var req SetStateRequest
	if err := decodeJSON(r, &req); err != nil {
		return req, err
	}
	if req.ArticleID <= 0 {
		return req, apierr.Validation("参数校验失败", []apierr.Details{
			{Field: "articleId", Message: "articleId 必须是正整数"},
		})
	}
	return req, nil
}

// requireArticleExists 校验文章存在（openapi 对收藏接口声明了 404）。
func (s *Server) requireArticleExists(r *http.Request, articleID int64) error {
	if s.deps.Articles == nil {
		return nil
	}
	_, err := s.deps.Articles.Get(r.Context(), articleID)
	return err
}

// requireArticlesExist 批量校验文章存在，任一不存在即整体 404。
//
// ★ 批量而非逐条：500 条逐条 Get 就是 500 次往返。这里借FindBriefs 的
// `id IN (...)` 一次问出来（它内部按 500 分批，本方法最多传 500 条 → 恰好一批），
// 查询次数与条目数无关。
//
// 缺 Articles 依赖时跳过校验（与 requireArticleExists 的降级约定一致）。
func (s *Server) requireArticlesExist(r *http.Request, articleIDs []int64) error {
	if s.deps.Articles == nil || len(articleIDs) == 0 {
		return nil
	}
	briefs, err := s.deps.Articles.FindBriefs(r.Context(), articleIDs)
	if err != nil {
		return err
	}
	for _, id := range articleIDs {
		if _, ok := briefs[id]; !ok {
			return apierr.NotFound("文章不存在: " + strconv.FormatInt(id, 10))
		}
	}
	return nil
}

// newStateItemFromFavorite 把 service 的收藏视图映射为 openapi StateItem。
func newStateItemFromFavorite(v *service.FavoriteView) StateItem {
	if v == nil {
		return StateItem{}
	}
	return StateItem{
		ArticleID: v.ArticleID,
		Deleted:   v.Deleted,
		UpdatedAt: v.UpdatedAt,
	}
}

// newStateItemFromRead 把 service 的已读视图映射为 openapi StateItem。
func newStateItemFromRead(v *service.ReadView) StateItem {
	if v == nil {
		return StateItem{}
	}
	return StateItem{
		ArticleID: v.ArticleID,
		Deleted:   v.Deleted,
		UpdatedAt: v.UpdatedAt,
	}
}

// accountCacheControl 是账号私有数据的缓存指令。
//
// private：绝不允许共享缓存（反代/CDN）留存用户资料；
// no-cache：允许存副本，但每次使用前必须回源验证（配合 ETag 生效）。
const accountCacheControl = "private, no-cache"

// 装配缺失类错误（service 未注入）。出现即代表 cmd/eznews 装配有误。
var (
	errMergeUnavailable = apierr.New(http.StatusServiceUnavailable, apierr.CodeInternal,
		"合并服务未就绪")
	errStateUnavailable = apierr.New(http.StatusServiceUnavailable, apierr.CodeInternal,
		"用户状态服务未就绪")
	// errNilStatePage 是 service 返回空页的编程错误。
	errNilStatePage = apierr.New(http.StatusInternalServerError, apierr.CodeInternal,
		"用户状态增量结果为空")
)

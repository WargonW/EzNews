package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/auth"
)

// refreshCookieName 是 refreshToken 的 Cookie 名（Web 同源部署，DEC-12）。
const refreshCookieName = "eznews_refresh"

// handleRegister 处理 POST /api/v1/auth/register。
//
// 注册即登录：service 层建号的同时建立会话并签发双令牌，
// 客户端无需再调一次 /auth/login（省一次 Argon2id 校验 + 少让用户输一遍密码）。
// 仅当建会话失败时降级为 needsLogin=true，客户端据此提示手动登录。
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if err := req.validate(); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if s.deps.Auth == nil {
		writeError(r.Context(), w, errAuthUnavailable)
		return
	}
	// 注册路径同样消耗 Argon2id（建 hash），沿用登录的双维度限流。
	if !s.allowAuthAttempt(w, r, "register") {
		return
	}
	if !s.keyAuthAttempt(w, r, "register", req.Username) {
		return
	}

	res, err := s.deps.Auth.Register(r.Context(),
		req.Username, req.Password, deref(req.Email),
		deref(req.ClientID), deref(req.DeviceName))
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	s.logger.InfoContext(r.Context(), "用户注册成功",
		slog.String("username", res.Username),
		slog.Int64("userId", res.UserID),
		slog.Bool("needsLogin", res.NeedsLogin),
		slog.String("requestId", apierr.RequestIDFromContext(r.Context())))

	resp := &TokenResponse{
		AccessToken: res.AccessToken,
		ExpiresIn:   res.ExpiresIn,
		SessionID:   res.SessionID,
		NeedsLogin:  res.NeedsLogin,
		User: &UserDTO{
			ID: res.UserID, Username: res.Username, Role: res.Role, CreatedAt: res.CreatedAt,
		},
	}
	// 注册即登录（service.Register 会建会话并签发令牌），
	// 所以这里能把 refreshToken 正常写进 Cookie / 响应体。
	s.writeTokenResponse(w, r, http.StatusCreated, resp, res.RefreshToken)
}

// handleLogin 处理 POST /api/v1/auth/login。
//
// 失败统一返回 401 INVALID_CREDENTIALS（不区分"用户不存在"与"密码错误"）。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if err := req.validate(); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if s.deps.Auth == nil {
		writeError(r.Context(), w, errAuthUnavailable)
		return
	}
	// per-username 与 per-IP 双维度限流（PRD §7.2）。
	//
	// 注意：service.AuthService 内部**也**有一层限流（按 auth.loginLimitPerUser /
	// loginLimitPerIP 配置串解析）。两层不冲突：handler 层按请求计数做粗粒度闸门，
	// service 层做精确的失败计数与账号锁定。此处命中时主动清零粗粒度桶，
	// 避免登录成功后仍被自己拦在门外。
	if !s.allowAuthAttempt(w, r, "login") {
		return
	}
	if !s.keyAuthAttempt(w, r, "login", req.Username) {
		return
	}

	res, err := s.deps.Auth.Login(r.Context(),
		req.Username, req.Password, deref(req.ClientID), deref(req.DeviceName), clientIP(r))
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	s.resetAuthAttempts(r, "login", req.Username)
	s.logger.InfoContext(r.Context(), "用户登录成功",
		slog.String("username", res.Username),
		slog.Int64("userId", res.UserID),
		slog.Int64("sessionId", res.SessionID),
		slog.String("requestId", apierr.RequestIDFromContext(r.Context())))

	s.writeTokenResponse(w, r, http.StatusOK, &TokenResponse{
		AccessToken: res.AccessToken,
		ExpiresIn:   res.ExpiresIn,
		SessionID:   res.SessionID,
		User:        s.lookupUserDTO(r, res.UserID),
	}, res.RefreshToken)
}

// handleRefresh 处理 POST /api/v1/auth/refresh。
//
// refreshToken 优先取请求体；缺省时从 httpOnly Cookie 读取（Web 同源部署）。
// 轮换与 reuse 两级判定（DEC-7）全部在 service 层完成。
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req RefreshRequest
	// 请求体可选：空体（同源 Cookie 模式）是合法输入，不能报 400。
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(r.Context(), w, err)
			return
		}
	}
	token := deref(req.RefreshToken)
	if token == "" {
		token = refreshTokenFromCookie(r)
	}
	if token == "" {
		writeError(r.Context(), w, apierr.Unauthorized("缺少 refreshToken"))
		return
	}
	if s.deps.Auth == nil {
		writeError(r.Context(), w, errAuthUnavailable)
		return
	}
	if !s.allowAuthAttempt(w, r, "refresh") {
		return
	}

	res, err := s.deps.Auth.Refresh(r.Context(), token, deref(req.ClientID), deref(req.DeviceName))
	if err != nil {
		// reuse 命中时 service 会撤销全部会话，客户端必须重新登录。
		writeError(r.Context(), w, err)
		return
	}
	s.writeTokenResponse(w, r, http.StatusOK, &TokenResponse{
		AccessToken: res.AccessToken,
		ExpiresIn:   res.ExpiresIn,
		SessionID:   res.SessionID,
		// refresh 响应必须带 user：客户端此时内存里已无用户资料（access token 刚过期），
		// 若这里为 nil，它刷新成功后仍显示未登录，必须再调一次 /me。
		User: &UserDTO{
			ID: res.UserID, Username: res.Username, Role: res.Role,
		},
	}, res.RefreshToken)
}

// handleLogout 处理 POST /api/v1/auth/logout。
//
// 幂等且**不泄露会话存在性**：即使会话行已不存在也返回 204。
// 优先按 access token 的 sid 定位（DEC-14）；sid 缺失时用请求体/ Cookie 里的
// refreshToken 反查（openapi 声明的回退路径）。两条都拿不到时仅清客户端态，仍返回 204。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// 请求体可选，解析失败不阻断登出（登出必须尽最大努力成功）。
	var body struct {
		RefreshToken *string `json:"refreshToken"`
	}
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &body); err != nil {
			s.logger.WarnContext(r.Context(), "登出请求体解析失败，按仅清客户端态处理",
				slog.String("err", err.Error()))
		}
	}
	refreshToken := deref(body.RefreshToken)
	if refreshToken == "" {
		refreshToken = refreshTokenFromCookie(r)
	}

	if s.deps.Auth == nil {
		// 账号服务不可用时仍清客户端态：登出的语义是「让客户端忘记」，必须成功。
		s.clearRefreshCookie(w)
		writeNoContent(w)
		return
	}

	// userID / sessionID 均来自 access token（OptionalAuth 已验签 + 校验 token_version）。
	userID, _ := auth.UserIDFromContext(r.Context())
	sessionID, _ := auth.SessionIDFromContext(r.Context())

	if userID == 0 && refreshToken == "" {
		// 无有效 token 且无 refreshToken：幂等返回 204，仅清客户端态。
		s.logger.WarnContext(r.Context(), "登出请求未携带任何凭据，仅清客户端态")
		s.clearRefreshCookie(w)
		writeNoContent(w)
		return
	}

	if err := s.deps.Auth.Logout(r.Context(), userID, sessionID, refreshToken); err != nil {
		// 凭据无效不算失败：登出的语义是「让客户端忘记」，必须成功。
		if isAuthFailure(err) {
			s.logger.InfoContext(r.Context(), "登出时会话已失效，按幂等成功处理",
				slog.String("code", string(apierr.AsAppError(err).Code)))
			s.clearRefreshCookie(w)
			writeNoContent(w)
			return
		}
		writeError(r.Context(), w, err)
		return
	}
	s.clearRefreshCookie(w)
	writeNoContent(w)
}

// writeTokenResponse 按部署模式裁剪凭据并写出响应。
//
// DEC-12：
//   - auth.refreshCookie=true → 写 httpOnly + SameSite=Lax Cookie，响应体 refreshToken 置 null。
//   - auth.refreshTokenInBody=true → 响应体返回 refreshToken（Android/跨域需要）。
//
// 两个开关默认都为 true（config.Validate 禁止同时关闭）。
// refreshToken 形参允许为空（service 未返回该值时只写 Cookie 裁剪逻辑、不设置）。
func (s *Server) writeTokenResponse(w http.ResponseWriter, r *http.Request,
	status int, resp *TokenResponse, refreshToken string) {

	if resp == nil {
		writeError(r.Context(), w, errNilTokenResponse)
		return
	}
	if s.cfg.Auth.RefreshCookie && refreshToken != "" {
		http.SetCookie(w, s.newRefreshCookie(refreshToken))
	}
	if s.cfg.Auth.RefreshTokenInBody && refreshToken != "" {
		token := refreshToken
		resp.RefreshToken = &token
	}
	writeJSON(r.Context(), w, status, resp)
}

// newRefreshCookie 构造 refreshToken 的 httpOnly Cookie。
//
// SameSite=Lax 而非 Strict：Lax 允许顶层 GET 导航携带，
// 满足「跨子域反代 + 前端路由跳转」场景；Strict 会导致刷新后首次导航丢 token。
func (s *Server) newRefreshCookie(token string) *http.Cookie {
	return &http.Cookie{
		Name:     refreshCookieName,
		Value:    token,
		Path:     s.refreshCookiePath(),
		HttpOnly: true,
		Secure:   true, // 同源部署由反代终结 TLS；本地 http 调试时浏览器会忽略该属性
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.cfg.Auth.RefreshTokenTTL.Std().Seconds()),
	}
}

// clearRefreshCookie 清除 refreshToken Cookie（登出时调用）。
func (s *Server) clearRefreshCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     s.refreshCookiePath(),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1, // 立即过期
	})
}

// refreshCookiePath 返回 Cookie 作用域路径，缺省限定在 auth 端点子树。
//
// 路径收窄的意义：/api/v1/articles 这类高频读接口不需要携带 refreshToken，
// 缩小 Path 可减少每次请求的 Cookie 头体积。
func (s *Server) refreshCookiePath() string {
	if p := s.cfg.Auth.RefreshCookiePath; p != "" {
		return p
	}
	return "/api/v1/auth"
}

// refreshTokenFromCookie 从 Cookie 读取 refreshToken。
func refreshTokenFromCookie(r *http.Request) string {
	c, err := r.Cookie(refreshCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// allowAuthAttempt 执行登录类端点的进程内限流（per-IP）。
//
// 返回 false 表示已写出 429 响应，调用方应立即 return。
//
// 维度键带端点类型，避免 register 的匿名滥用把 login 的额度吃光。
func (s *Server) allowAuthAttempt(w http.ResponseWriter, r *http.Request, kind string) bool {
	if s.deps.Limiter == nil {
		return true
	}
	if !s.deps.Limiter.Allow("auth:" + kind + ":ip:" + clientIP(r)) {
		writeError(r.Context(), w, apierr.RateLimited("请求过于频繁，请稍后重试"))
		return false
	}
	return true
}

// keyAuthAttempt 以用户名再叠加一层限流（防针对单账号的撞库）。
func (s *Server) keyAuthAttempt(w http.ResponseWriter, r *http.Request, kind, username string) bool {
	if s.deps.Limiter == nil || username == "" {
		return true
	}
	if !s.deps.Limiter.Allow("auth:" + kind + ":user:" + username) {
		writeError(r.Context(), w, apierr.RateLimited("该账号登录尝试过于频繁，请稍后重试"))
		return false
	}
	return true
}

// resetAuthAttempts 在认证成功后清空该用户名/IP 的粗粒度限流桶。
//
// 只清 handler 层的桶；service 层的失败计数由它自己在登录成功时清零。
func (s *Server) resetAuthAttempts(r *http.Request, kind, username string) {
	if s.deps.Limiter == nil {
		return
	}
	s.deps.Limiter.Reset("auth:" + kind + ":ip:" + clientIP(r))
	if username != "" {
		s.deps.Limiter.Reset("auth:" + kind + ":user:" + username)
	}
}

// lookupUserDTO 尽力补齐 UserDTO；查询失败时返回 nil 而非报错。
//
// 用于 login 这类「令牌已签发成功」的路径：用户资料只是附带信息，
// 查不到不应让整个登录失败（客户端仍可凭accessToken 调用 /me 自行获取）。
func (s *Server) lookupUserDTO(r *http.Request, userID int64) *UserDTO {
	if s.deps.Auth == nil || userID <= 0 {
		return nil
	}
	user, err := s.deps.Auth.GetUser(r.Context(), userID)
	if err != nil {
		s.logger.WarnContext(r.Context(), "登录成功后读取用户资料失败，响应中 user 置空",
			slog.Int64("userId", userID), slog.String("err", err.Error()))
		return nil
	}
	return newUserDTO(user)
}

// isAuthFailure 判断错误是否为鉴权类（401），用于登出幂等处理。
func isAuthFailure(err error) bool {
	return apierr.AsAppError(err).Status == http.StatusUnauthorized
}

// errAuthUnavailable 表示 service 层尚未注入账号能力（装配缺失）。
var errAuthUnavailable = apierr.New(http.StatusServiceUnavailable, apierr.CodeInternal,
	"账号服务未就绪")

// errNilTokenResponse 是 service 返回空响应体的编程错误。
var errNilTokenResponse = apierr.New(http.StatusInternalServerError, apierr.CodeInternal,
	"service 返回空的令牌响应")

// Package httpapi 是 EZNews 的 HTTP 接入层：路由表、中间件装配与全部 handler。
//
// 分层约定（ARCHITECTURE.md §7.2）：本包只做「协议翻译」——
// 解析 HTTP 请求、调用 service 层、序列化响应；不写 SQL、不含业务规则。
//
// 鉴权分两类，**绝不可混用**（ARCHITECTURE.md §9.1）：
//
//   - RequireAuth  ——仅用于 /api/v1/me/**。无有效 JWT → 401 UNAUTHORIZED。
//   - OptionalAuth ——用于 /api/v1/articles、/sources、/audio 等业务读接口。
//     游客可访问；带合法 JWT 时在响应里追加 isFavorited / isRead；
//     带非法或过期 token 时**不拒绝请求**，仅降级为游客态。
//     这条是 Web 端游客体验的硬要求：token 过期绝不能让文章列表直接报错。
//
// 采集器写入（/api/v1/ingest/*）走完全独立的 X-Api-Key 鉴权，与用户 JWT 无关。
package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/auth"
	"github.com/eznews/eznews/internal/config"
	"github.com/eznews/eznews/internal/observe"
	"github.com/eznews/eznews/internal/ratelimit"
	"github.com/eznews/eznews/internal/repo"
	"github.com/eznews/eznews/internal/service"
	"github.com/eznews/eznews/internal/store"
	"github.com/eznews/eznews/internal/util"
	"github.com/eznews/eznews/internal/webui"

	"github.com/eznews/eznews/internal/httpapi/middleware"
)

// Version 是服务版本号，由main 通过 ldflags 或默认值注入 /healthz 与 /metrics。
const Version = "1.0.0"

// Deps 是 Server 所需的全部依赖，由 cmd/eznews 装配后注入。
type Deps struct {
	DB      *store.DB
	Config  *config.Config
	Metrics *observe.Metrics
	Logger  *slog.Logger
	Limiter *ratelimit.Limiter

	// UserRepo 由 cmd/eznews 注入以保持装配对称；当前 handler 层的
	// token_version 校验已委托给 service.AuthService.VerifyTokenVersion，
	// 故本包不直接持有它。保留字段是为了不改动既有的装配代码。
	UserRepo *repo.UserRepo

	Articles *service.ArticleService
	Sources  *service.SourceService
	Audio    *service.AudioService
	Ingest   *service.IngestService

	// 账号三件套。均为 service 层已落地的具体类型，
	// handler 只做协议翻译，不重复实现业务规则。
	Auth  *service.AuthService
	Merge *service.MergeService
	State *service.UserStateService

	// Admin 是管理后台的 service。为 nil 时全部 /admin/** 端点返回 500，
	// 而不是当作"没有这个接口"返回 404 —— 后者会让调用方以为自己路径写错了。
	Admin *service.AdminService

	// JWT 用于本地验签（零 DB 查询）。为 nil 时 OptionalAuth 退化为纯游客态。
	JWT *auth.JWTManager
}

// Server 持有 handler 所需的全部依赖，并负责装配路由与中间件链。
type Server struct {
	deps   Deps
	cfg    *config.Config
	mux    *http.ServeMux
	logger *slog.Logger

	// spa 是内嵌 Web 端的静态处理器；nil 表示本二进制没有内嵌前端产物。
	// 见 internal/webui.HasUI 与 serveSPA。
	spa http.Handler
}

// New 创建 Server 并完成路由注册与中间件装配。
func New(deps Deps) (*Server, error) {
	if deps.Config == nil {
		return nil, apierr.Internal(errNilConfig)
	}
	s := &Server{
		deps:   deps,
		cfg:    deps.Config,
		mux:    http.NewServeMux(),
		logger: deps.Logger,
	}
	if s.logger == nil {
		// 未注入时回落到 slog 默认 logger，保证日志永不为 nil（避免各处判空）。
		s.logger = slog.Default()
	}
	// 仅在真正内嵌了 Web 端产物时才挂载静态处理器。
	// 占位版本的 index.html 不含 <script>，挂上去用户只会看到空白页。
	if webui.HasUI() {
		if dist, err := webui.Dist(); err == nil {
			s.spa = newSPAHandler(dist)
		} else {
			s.logger.Warn("内嵌 Web 端产物读取失败，仅提供 API", slog.String("err", err.Error()))
		}
	} else {
		s.logger.Warn("本二进制未内嵌 Web 端产物，仅提供 API",
			slog.String("hint", "发布版请用 scripts/build.sh all 构建"))
	}
	s.routes()
	return s, nil
}

// newSPAHandler 构造单页应用的静态处理器。
//
// 与裸 http.FileServer 的区别是**前端路由回落**：
// 用户直接访问 /article/123、/admin、或刷新任意客户端路由时，
// 磁盘（这里是内嵌 FS）上并不存在对应文件，FileServer 会返回 404；
// 而 SPA 期望拿到 index.html，再由前端路由接管。所以找不到文件时回落到 index.html。
//
// 缓存策略的选择：
//   - 文件名带内容哈希的资源（Vite 产物 assets/*.js）→ 强缓存 immutable，一年；
//     内容变了文件名就会变，永不会出现"缓存了旧内容"。
//   - index.html → 必须 no-cache。它是所有资源的入口，
//     一旦被缓存，发布的版本更新就再也拉不到了，而页面上的 JS 链接还是旧的。
func newSPAHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 去掉前导 "/" 才能在内嵌 FS 里按相对路径查找。
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		f, err := fsys.Open(name)
		if err == nil {
			_ = f.Close()
			if strings.Contains(name, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		// 找不到：**只有 GET 才回落到 index.html**。
		// 对 POST/PUT 等返回半吊子 HTML 会掩盖请求方法本身的错误。
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			fileServer.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
	})
}

// ServeHTTP 实现 http.Handler：外层套上全局中间件链。
//
// 中间件顺序（ARCHITECTURE.md §7.2）：RequestID → Recover → BodyLimit → Logging → Handler。
// 限流与鉴权按路由分组在 routes() 内装配，因为它们的强度依赖具体端点（ingest / 读 / 登录）。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := middleware.Chain(s.mux,
		middleware.RequestID,
		middleware.Recover,
		middleware.BodyLimit(s.cfg.Ingest.MaxBodyBytes),
		s.logging,
	)
	h.ServeHTTP(w, r)
}

// errNilConfig 是在装配阶段编程错误时返回的错误。
var errNilConfig = apierr.New(http.StatusInternalServerError, apierr.CodeInternal, "config 不能为空")

// ---------------- 路由表 ----------------

// routes 注册全部路由，并按端点装配对应的鉴权中间件。
//
// 鉴权中间件在注册时就包裹 handler（而非全局统一套一层），
// 因为不同端点的鉴权强度本质不同：ingest 用 API Key、/me/** 强制 JWT、
// 业务读接口游客可访问。集中在此处声明，避免逐个 handler 漏挂。
func (s *Server) routes() {
	// ---- 存活/就绪/指标（不鉴权）----
	s.route(authNone, "GET /healthz", s.handleHealthz)
	s.route(authNone, "GET /readyz", s.handleReadyz)
	s.route(authNone, "GET /metrics", s.handleMetrics)

	// ---- 采集器写入：X-Api-Key 鉴权，与账号体系完全独立 ----
	s.route(authAPIKey, "POST /api/v1/ingest/articles", s.handleIngestArticle)
	s.route(authAPIKey, "POST /api/v1/ingest/articles/batch", s.handleIngestBatch)

	// ---- 账号（游客可调用）----
	s.route(authNone, "POST /api/v1/auth/register", s.handleRegister)
	s.route(authNone, "POST /api/v1/auth/login", s.handleLogin)
	s.route(authNone, "POST /api/v1/auth/refresh", s.handleRefresh)
	// 登出用 OptionalAuth：带 sid 时按会话销毁；两者都缺失时仍返回 204（openapi 约定）。
	s.route(authOptional, "POST /api/v1/auth/logout", s.handleLogout)

	// ---- 业务读接口：OptionalAuth（游客可访问）----
	s.route(authOptional, "GET /api/v1/articles", s.handleListArticles)
	s.route(authOptional, "GET /api/v1/articles/{id}", s.handleGetArticle)
	s.route(authOptional, "GET /api/v1/articles/{id}/audio", s.handleGetArticleAudio)
	s.route(authOptional, "GET /api/v1/categories", s.handleCategories)
	s.route(authOptional, "GET /api/v1/sources", s.handleListSources)
	s.route(authOptional, "GET /api/v1/audio/tasks/{taskId}", s.handleGetAudioTask)
	s.route(authOptional, "GET /api/v1/audio/{audioId}", s.handleGetAudioFile)

	// ---- 业务写接口：OptionalAuth（保持与 openapi 的"可选"标注一致）----
	s.route(authOptional, "POST /api/v1/sources", s.handleCreateSource)
	s.route(authOptional, "PUT /api/v1/sources/{id}", s.handleUpdateSource)
	s.route(authOptional, "PATCH /api/v1/sources/{id}/enabled", s.handleSetSourceEnabled)
	s.route(authOptional, "DELETE /api/v1/sources/{id}", s.handleDeleteSource)
	s.route(authOptional, "POST /api/v1/audio/tasks", s.handleCreateAudioTask)
	s.route(authOptional, "POST /api/v1/audio/tasks/{taskId}/retry", s.handleRetryAudioTask)

	// ---- 账号数据：RequireAuth（必须有有效 JWT）----
	s.route(authRequired, "GET /api/v1/me", s.handleGetMe)
	s.route(authRequired, "DELETE /api/v1/me", s.handleDeleteMe)
	s.route(authRequired, "PATCH /api/v1/me/password", s.handleChangePassword)
	s.route(authRequired, "POST /api/v1/me/merge", s.handleMerge)
	s.route(authRequired, "GET /api/v1/me/favorites", s.handleListFavorites)
	s.route(authRequired, "POST /api/v1/me/favorites", s.handleSetFavorite)
	s.route(authRequired, "GET /api/v1/me/reads", s.handleListReads)
	s.route(authRequired, "POST /api/v1/me/reads", s.handleSetRead)
	// 批量标记走独立路径 /me/state/batch 而非挂到 /me/reads 下：
	// 它同时可改收藏（favorite 字段），语义上跨越 reads 与 favorites 两个资源，
	// 塞进其中任一个都会让契约读起来像只管一半。
	s.route(authRequired, "POST /api/v1/me/state/batch", s.handleBatchSetState)
	s.route(authRequired, "GET /api/v1/me/preferences", s.handleGetPreferences)
	s.route(authRequired, "PUT /api/v1/me/preferences", s.handlePutPreferences)
	s.route(authRequired, "GET /api/v1/me/sessions", s.handleListSessions)
	s.route(authRequired, "DELETE /api/v1/me/sessions/{id}", s.handleRevokeSession)

	// ---- 管理后台：RequireAuth + 管理员角色校验 ----
	//
	// authAdmin 的实现见 requireAdmin。这里刻意摆在路由表靠后位置：
	// 与管理后台无关的路由先注册，读代码的人先看常规业务再看管理员功能。
	s.route(authAdmin, "GET /api/v1/admin/overview", s.handleAdminOverview)
	s.route(authAdmin, "GET /api/v1/admin/users", s.handleAdminListUsers)
	s.route(authAdmin, "POST /api/v1/admin/users", s.handleAdminCreateUser)
	s.route(authAdmin, "GET /api/v1/admin/users/{id}", s.handleAdminGetUser)
	s.route(authAdmin, "PATCH /api/v1/admin/users/{id}/role", s.handleAdminSetUserRole)
	s.route(authAdmin, "PATCH /api/v1/admin/users/{id}/disabled", s.handleAdminSetUserDisabled)
	s.route(authAdmin, "DELETE /api/v1/admin/users/{id}", s.handleAdminDeleteUser)
	s.route(authAdmin, "POST /api/v1/admin/users/{id}/sessions/revoke-all", s.handleAdminRevokeAllSessions)
	s.route(authAdmin, "GET /api/v1/admin/stats/content", s.handleAdminContentStats)
	s.route(authAdmin, "GET /api/v1/admin/audio/tasks", s.handleAdminListAudioTasks)
	s.route(authAdmin, "POST /api/v1/admin/audio/tasks/{taskId}/retry", s.handleAdminRetryAudioTask)
	s.route(authAdmin, "GET /api/v1/admin/system", s.handleAdminSystem)

	// ---- 兜底：Web 端 SPA，或 API 的 JSON 404 ----
	//
	// ★ 这两条走同一个 "/" 兜底，必须区分开：
	//   Go 1.22 的 ServeMux 按**最长匹配**路由，像 `/api/v1/does-not-exist`
	//   这种未知 API 路径匹配不到任何具体 pattern，同样会落到 "/"。
	//   如果直接把它交给 SPA，客户端会在期待 JSON Envelope 的地方收到一份 HTML，
	//   统一的错误处理链路就此断掉（且症状是"JSON 解析失败"，完全指错方向）。
	//   所以凡是 /api/ 前缀的一律维持 JSON 404，其余才交给前端路由。
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
			writeError(r.Context(), w, apierr.NotFound("接口不存在: "+r.URL.Path))
			return
		}
		s.serveSPA(w, r)
	})
}

// serveSPA 提供 Web 端（含 /admin 管理后台）的静态资源与前端路由回落。
//
// 单文件交付的关键一环：页面、脚本、样式全部来自 //go:embed 的内嵌文件系统
// （见 internal/webui），运行时不再依赖任何磁盘上的静态目录。
func (s *Server) serveSPA(w http.ResponseWriter, r *http.Request) {
	if s.spa == nil {
		// 没内嵌前端（占位版本）：给出明确说明，而不是白屏或 404。
		//
		// 这种情况只出现在「直接 go build 而没有先拷前端产物」时 ——
		// 比如纯后端开发者 clone 下来顺手编了一个二进制。
		// 让人一眼看出原因，比让他对着空白页猜好。
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(noUIMessage))
		return
	}
	s.spa.ServeHTTP(w, r)
}

// noUIMessage 是二进制未内嵌前端时返回的说明。
const noUIMessage = "EZNews 服务端运行中，但本二进制未内嵌 Web 端产物。\n\n" +
	"发布版本请用单文件构建产出（内含全部页面）：\n" +
	"    bash scripts/build.sh all\n\n" +
	"当前 API 不受影响，仍可通过 /api/v1/** 正常访问。\n"

// route 按鉴权类型包裹中间件后注册路由。
//
// authType 与 wrapAuth 必须一一对应；新增鉴权类型时两处同时改。
func (s *Server) route(authType, pattern string, h http.HandlerFunc) {
	s.mux.HandleFunc(pattern, s.wrapAuth(authType, h))
}

// wrapAuth 返回按鉴权类型装饰后的 handler。
func (s *Server) wrapAuth(authType string, h http.HandlerFunc) http.HandlerFunc {
	switch authType {
	case authRequired:
		return s.requireAuth(h)
	case authAdmin:
		return s.requireAdmin(h)
	case authOptional:
		return s.optionalAuth(s.optionalAPIKey(h))
	case authAPIKey:
		return s.apiKeyAuth(h)
	case authNone:
		return h
	default:
		// 未知鉴权类型按「不鉴权」处理会放大风险，故fail-closed拒绝该端点。
		return func(w http.ResponseWriter, r *http.Request) {
			writeError(r.Context(), w, apierr.Internal(errUnknownAuth))
		}
	}
}

// errAdminDisabled 表示本实例未装配管理后台服务。
var errAdminDisabled = apierr.New(http.StatusInternalServerError, apierr.CodeInternal,
	"管理后台服务未启用")

// errNoActor 表示管理端点的上下文里取不到操作者 ID（中间件装配错误）。
var errNoActor = apierr.New(http.StatusInternalServerError, apierr.CodeInternal,
	"无法确定操作者身份")

// errUnknownAuth 表示路由表声明了未实现的鉴权类型（编程错误，必须 fail-closed）。
var errUnknownAuth = apierr.New(http.StatusInternalServerError, apierr.CodeInternal,
	"路由鉴权类型未实现")

// 鉴权类型常量（仅用于路由表自文档化）。
const (
	authNone     = "none"     // 不鉴权
	authOptional = "optional" // OptionalAuth：游客可访问
	authRequired = "required" // RequireAuth：必须有有效 JWT
	authAPIKey   = "apikey"   // X-Api-Key：采集器
	authAdmin    = "admin"    // RequireAuth + 管理员角色 + 未停用
)

// Routes 返回最终路由表（供报告与自检使用）。
func (s *Server) Routes() []RouteInfo {
	return []RouteInfo{
		{Method: "GET", Path: "/healthz", Auth: authNone, Desc: "存活探针（含依赖状态，不鉴权）"},
		{Method: "GET", Path: "/readyz", Auth: authNone, Desc: "就绪探针（SELECT 1 + WAL 可写）"},
		{Method: "GET", Path: "/metrics", Auth: authNone, Desc: "进程内 JSON 指标"},

		{Method: "POST", Path: "/api/v1/ingest/articles", Auth: authAPIKey, Desc: "提交单篇文章"},
		{Method: "POST", Path: "/api/v1/ingest/articles/batch", Auth: authAPIKey, Desc: "批量提交文章"},

		{Method: "POST", Path: "/api/v1/auth/register", Auth: authNone, Desc: "注册并建立会话"},
		{Method: "POST", Path: "/api/v1/auth/login", Auth: authNone, Desc: "登录"},
		{Method: "POST", Path: "/api/v1/auth/refresh", Auth: authNone, Desc: "轮换 refreshToken"},
		{Method: "POST", Path: "/api/v1/auth/logout", Auth: authOptional, Desc: "登出（幂等 204）"},

		{Method: "GET", Path: "/api/v1/articles", Auth: authOptional, Desc: "文章列表（首屏/翻页/增量）"},
		{Method: "GET", Path: "/api/v1/articles/{id}", Auth: authOptional, Desc: "文章详情"},
		{Method: "GET", Path: "/api/v1/articles/{id}/audio", Auth: authOptional, Desc: "缓存直查音频"},
		{Method: "GET", Path: "/api/v1/categories", Auth: authOptional, Desc: "分类枚举与计数"},
		{Method: "GET", Path: "/api/v1/sources", Auth: authOptional, Desc: "源列表"},
		{Method: "POST", Path: "/api/v1/sources", Auth: authOptional, Desc: "新增自定义源"},
		{Method: "PUT", Path: "/api/v1/sources/{id}", Auth: authOptional, Desc: "编辑源"},
		{Method: "PATCH", Path: "/api/v1/sources/{id}/enabled", Auth: authOptional, Desc: "启停源"},
		{Method: "DELETE", Path: "/api/v1/sources/{id}", Auth: authOptional, Desc: "删除自定义源"},

		{Method: "POST", Path: "/api/v1/audio/tasks", Auth: authOptional, Desc: "提交合成任务"},
		{Method: "GET", Path: "/api/v1/audio/tasks/{taskId}", Auth: authOptional, Desc: "查询合成状态"},
		{Method: "POST", Path: "/api/v1/audio/tasks/{taskId}/retry", Auth: authOptional, Desc: "重试失败任务"},
		{Method: "GET", Path: "/api/v1/audio/{audioId}", Auth: authOptional, Desc: "音频文件（Range）"},

		{Method: "GET", Path: "/api/v1/me", Auth: authRequired, Desc: "当前账号信息"},
		{Method: "DELETE", Path: "/api/v1/me", Auth: authRequired, Desc: "注销账号（需密码，204）"},
		{Method: "PATCH", Path: "/api/v1/me/password", Auth: authRequired, Desc: "修改密码"},
		{Method: "POST", Path: "/api/v1/me/merge", Auth: authRequired, Desc: "游客数据幂等合并"},
		{Method: "GET", Path: "/api/v1/me/favorites", Auth: authRequired, Desc: "收藏增量列表（含墓碑）"},
		{Method: "POST", Path: "/api/v1/me/favorites", Auth: authRequired, Desc: "收藏/取消收藏"},
		{Method: "GET", Path: "/api/v1/me/reads", Auth: authRequired, Desc: "已读增量列表（含墓碑）"},
		{Method: "POST", Path: "/api/v1/me/reads", Auth: authRequired, Desc: "标记已读/取消已读"},
		{Method: "POST", Path: "/api/v1/me/state/batch", Auth: authRequired, Desc: "批量标记已读/收藏（整批原子，上限 500）"},
		{Method: "GET", Path: "/api/v1/me/preferences", Auth: authRequired, Desc: "拉取偏好 KV"},
		{Method: "PUT", Path: "/api/v1/me/preferences", Auth: authRequired, Desc: "整包写入偏好 KV"},
		{Method: "GET", Path: "/api/v1/me/sessions", Auth: authRequired, Desc: "活跃会话列表"},
		{Method: "DELETE", Path: "/api/v1/me/sessions/{id}", Auth: authRequired, Desc: "踢下线指定会话"},

		{Method: "GET", Path: "/api/v1/admin/overview", Auth: authAdmin, Desc: "后台概览"},
		{Method: "GET", Path: "/api/v1/admin/users", Auth: authAdmin, Desc: "用户列表（分页/搜索）"},
		{Method: "POST", Path: "/api/v1/admin/users", Auth: authAdmin, Desc: "管理员代建账号"},
		{Method: "GET", Path: "/api/v1/admin/users/{id}", Auth: authAdmin, Desc: "用户详情"},
		{Method: "PATCH", Path: "/api/v1/admin/users/{id}/role", Auth: authAdmin, Desc: "修改用户角色"},
		{Method: "PATCH", Path: "/api/v1/admin/users/{id}/disabled", Auth: authAdmin, Desc: "停用/启用账户"},
		{Method: "DELETE", Path: "/api/v1/admin/users/{id}", Auth: authAdmin, Desc: "删除账户（204）"},
		{Method: "POST", Path: "/api/v1/admin/users/{id}/sessions/revoke-all", Auth: authAdmin, Desc: "吊销全部会话"},
		{Method: "GET", Path: "/api/v1/admin/stats/content", Auth: authAdmin, Desc: "内容统计"},
		{Method: "GET", Path: "/api/v1/admin/audio/tasks", Auth: authAdmin, Desc: "语音任务列表"},
		{Method: "POST", Path: "/api/v1/admin/audio/tasks/{taskId}/retry", Auth: authAdmin, Desc: "重试语音任务"},
		{Method: "GET", Path: "/api/v1/admin/system", Auth: authAdmin, Desc: "系统与数据库状态"},
	}
}

// RouteInfo 是对外暴露的路由描述。
type RouteInfo struct {
	Method string
	Path   string
	Auth   string
	Desc   string
}

// ---------------- 鉴权中间件 ----------------

// requireAuth 是 /api/v1/me/** 的强制鉴权中间件。
//
// 无 Authorization 头或 token 非法 → 401。
// 过期单独返回 TOKEN_EXPIRED（客户端据此静默 refresh 后重放），
// 签名无效/声明缺失返回 UNAUTHORIZED（客户端应降级游客态）。
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			writeError(r.Context(), w, apierr.Unauthorized("缺少访问令牌"))
			return
		}
		claims, err := s.parseToken(token)
		if err != nil {
			writeError(r.Context(), w, err)
			return
		}
		userID, err := claims.UserID()
		if err != nil {
			writeError(r.Context(), w, apierr.Unauthorized("访问令牌无效"))
			return
		}
		ctx := auth.WithUserID(r.Context(), userID)
		if claims.HasSessionID() {
			ctx = auth.WithSessionID(ctx, claims.SessionID)
		}
		// token_version 校验（改密/全局登出后旧 token 立即失效）。
		if err := s.verifyTokenVersion(ctx, userID, claims.TokenVersion); err != nil {
			writeError(ctx, w, err)
			return
		}
		// 会话级校验：单独踢下线某个会话后，它的 access token 立即失效。
		if err := s.verifySessionActive(ctx, userID, claims); err != nil {
			writeError(ctx, w, err)
			return
		}
		next(w, r.WithContext(ctx))
	}
}

// requireAdmin 是管理后台端点的鉴权中间件。
//
// 它在 requireAuth 的完整校验之上再加一道角色校验，顺序刻意如此：
// 先确认"这是谁"（JWT 签名、过期、token_version、会话吊销），
// 再确认"他够不够格"（role、停用状态）。反过来做会浪费一次 DB 查询在
// 一个连身份都不合法的请求上。
//
// ★ 403 与 401 的区分是有意的：
//   - 401 = 身份有问题，客户端应去 refresh；
//   - 403 = 身份有效但没权限，客户端必须**立即跳走**，重试一万次也没用。
//     混为一谈的结果是：被降权的管理员页面陷入静默重试循环，没有任何提示。
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if s.deps.Admin == nil {
			writeError(ctx, w, errAdminDisabled)
			return
		}
		userID, ok := auth.UserIDFromContext(ctx)
		if !ok {
			writeError(ctx, w, apierr.Internal(errNoActor))
			return
		}
		// 每次请求现查库而非信任 token 里的 claim：
		// 降权/停用可以由此做到**立即生效**，不必等 access token 过期。
		// 详见 AdminService.GetAccountState 的说明。
		st, err := s.deps.Admin.GetAccountState(ctx, userID)
		if err != nil {
			writeError(ctx, w, err)
			return
		}
		if !st.Exists {
			writeError(ctx, w, apierr.Unauthorized("账号不存在"))
			return
		}
		if st.Disabled {
			writeError(ctx, w, apierr.Forbidden("账号已被停用"))
			return
		}
		if !st.IsAdmin {
			writeError(ctx, w, apierr.Forbidden("需要管理员权限"))
			return
		}
		next(w, r)
	})
}

// optionalAuth 是业务读接口的可选鉴权中间件（游客可访问）。
//
// 语义（硬要求）：
//   - 没带 Authorization 头 → 直接放行，游客态。
//   - 带了合法 JWT → 注入 user_id，handler 可据此附加 isFavorited / isRead。
//   - 带了非法/过期 token → **不拒绝**，按游客态放行，仅记 debug 日志。
//     客户端 access token 在 2 小时边界过期时，文章列表必须仍能正常浏览。
func (s *Server) optionalAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			next(w, r)
			return
		}
		claims, err := s.parseToken(token)
		if err != nil {
			s.logger.DebugContext(r.Context(), "可选鉴权失败，降级为游客态",
				slog.String("path", r.URL.Path),
				slog.String("reason", err.Error()))
			next(w, r)
			return
		}
		userID, err := claims.UserID()
		if err != nil {
			next(w, r)
			return
		}
		if err := s.verifyTokenVersion(r.Context(), userID, claims.TokenVersion); err != nil {
			s.logger.DebugContext(r.Context(), "token_version 不匹配，降级为游客态",
				slog.Int64("userId", userID))
			next(w, r)
			return
		}
		// 会话已被单独吊销：同样降级为游客态而非拒绝。
		// 公开接口（/articles 等）必须保证被踢用户仍能以游客身份浏览，
		// 只有 /me/** 这类强制鉴权接口才把失败升级为 401。
		if err := s.verifySessionActive(r.Context(), userID, claims); err != nil {
			s.logger.DebugContext(r.Context(), "会话已吊销，降级为游客态",
				slog.Int64("userId", userID), slog.Int64("sessionId", claims.SessionID))
			next(w, r)
			return
		}
		ctx := auth.WithUserID(r.Context(), userID)
		if claims.HasSessionID() {
			ctx = auth.WithSessionID(ctx, claims.SessionID)
		}
		next(w, r.WithContext(ctx))
	}
}

// apiKeyAuth 是采集器写入接口的 X-Api-Key 鉴权中间件。
//
// 与账号体系完全独立：不读 Authorization 头，不查用户表。
// 常数时间比较所有配置 key（遍历全部 key 而非短路，避免时序侧信道）。
func (s *Server) apiKeyAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimSpace(r.Header.Get("X-Api-Key"))
		if provided == "" {
			writeError(r.Context(), w, apierr.InvalidAPIKey())
			return
		}
		var matched string
		for _, k := range s.cfg.Auth.APIKeys {
			// 常数时间比较；为避免长度信息泄漏，先比较哈希。
			if subtle.ConstantTimeCompare([]byte(hashSecret(provided)), []byte(hashSecret(k.Key))) == 1 {
				matched = k.ID
			}
		}
		if matched == "" {
			writeError(r.Context(), w, apierr.InvalidAPIKey())
			return
		}
		s.logger.DebugContext(r.Context(), "api key 通过", slog.String("keyId", matched))
		next(w, r.WithContext(auth.WithKeyID(r.Context(), matched)))
	}
}

// optionalAPIKey 是读接口的可选 key 校验：
// 仅当 auth.requireKeyForRead=true 时才要求携带合法 key，否则直接放行。
func (s *Server) optionalAPIKey(next http.HandlerFunc) http.HandlerFunc {
	if !s.cfg.Auth.RequireKeyForRead {
		return next
	}
	return s.apiKeyAuth(next)
}

// bearerToken 从 Authorization 头提取 Bearer token。
func bearerToken(r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	if raw == "" {
		return "", false
	}
	const prefix = "bearer "
	if len(raw) <= len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(raw[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// parseToken 本地验签（零 DB 查询）。未配置 JWT 时一律视为无效。
func (s *Server) parseToken(token string) (*auth.Claims, error) {
	if s.deps.JWT == nil {
		return nil, apierr.Unauthorized("访问令牌无效")
	}
	claims, err := s.deps.JWT.Parse(token)
	if err != nil {
		if errors.Is(err, auth.ErrTokenExpired) {
			return nil, apierr.TokenExpired()
		}
		return nil, apierr.Unauthorized("访问令牌无效")
	}
	return claims, nil
}

// verifyTokenVersion 校验 token_version；改密或全局登出后旧 token 立即失效。
//
// 委托给 service.AuthService.VerifyTokenVersion：账号已注销时它返回 401，
// 避免已删除账号的 token 继续访问 /me/**。
func (s *Server) verifyTokenVersion(ctx context.Context, userID int64, tv int32) error {
	if s.deps.Auth == nil {
		// 未注入账号服务时跳过校验（单测/降级场景）。
		return nil
	}
	return s.deps.Auth.VerifyTokenVersion(ctx, userID, tv)
}

// verifySessionActive 校验 sid 对应会话未被单独吊销（踢下线后立即 401）。
//
// 与 verifyTokenVersion 的先后顺序是**刻意的**：
// token_version 是用户级开关（改密 / 泄露处置 / 退出全部设备都会 tv+1），
// 语义更粗但判断更权威，必须先判；本方法只补上"单个会话被踢"这一细粒度场景。
//
// 未注入 Auth 时跳过（与 verifyTokenVersion 的降级约定一致）。
func (s *Server) verifySessionActive(ctx context.Context, userID int64, claims *auth.Claims) error {
	if s.deps.Auth == nil || claims == nil || !claims.HasSessionID() {
		return nil
	}
	return s.deps.Auth.VerifySessionActive(ctx, userID, claims.SessionID)
}

// hashSecret 用于常数时间比较，避免直接比较明文长度带来的时序泄漏。
func hashSecret(s string) string {
	return util.SHA256Hex(s)
}

// ---------------- 访问日志 ----------------

// statusWriter 记录响应状态码与字节数，供访问日志使用。
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

// WriteHeader 记录状态码。
func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write 累加响应字节数。
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Flush 实现 http.Flusher，保证 SSE/大文件场景可用。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logging 是结构化访问日志中间件：记录方法、路径、状态码与耗时。
//
// 放在 Recover 之内，确保 panic 被转成 500 后仍能记录成一条完整访问日志。
func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		level := slog.LevelInfo
		if sw.status >= http.StatusInternalServerError {
			level = slog.LevelError
		} else if sw.status >= http.StatusBadRequest {
			level = slog.LevelWarn
		}
		s.logger.Log(r.Context(), level, "http",
			slog.String("requestId", apierr.RequestIDFromContext(r.Context())),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", sw.status),
			slog.Int64("bytes", sw.bytes),
			slog.Int64("durationMs", time.Since(start).Milliseconds()),
			slog.String("ip", clientIP(r)),
		)
	})
}

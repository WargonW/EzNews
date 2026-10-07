package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/auth"
	"github.com/eznews/eznews/internal/config"
	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/ratelimit"
	"github.com/eznews/eznews/internal/repo"
	"github.com/eznews/eznews/internal/store"
	"github.com/eznews/eznews/internal/util"
)

// AuthService 实现账号体系的全部业务逻辑：注册、登录、刷新（含 DEC-7 reuse 两级判定）、
// 登出（按 sid 定位）、注销账号、改密、会话列表与 token_version 校验。
//
// 设计要点（这些都是踩过的坑，改动前请先读 ARCHITECTURE.md §9）：
//   - Refresh Token 是 32B 随机不透明串，库内只存 sha256；Access Token 是自包含 JWT，本地验签零 DB 查询。
//   - 轮换必须原地 UPDATE（见 repo.UserRepo.RotateSession 的行身份不变量说明）。
//   - token_version 是"无 Redis 黑名单"的失效手段：+1 即作废该用户全部 Access Token。
type AuthService struct {
	db        *store.DB
	users     *repo.UserRepo
	jwt       *auth.JWTManager
	hasher    *auth.Argon2Hasher
	sem       *auth.Semaphore
	cfg       config.AuthConf
	userLimit *ratelimit.Limiter
	ipLimit   *ratelimit.Limiter
	// failCount 记录用户名维度的连续失败次数与锁定截止时间（配合 lockoutDuration 做账号锁定）。
	failCount sync.Map // map[string]*failState
	// revokedSessions 缓存"已被单独吊销"的会话（sessionID -> 吊销时刻）。
	//
	// 存在的理由：token_version 是**用户级**开关，单独吊销某个会话时递增它会连带
	// 踢掉该用户的所有设备（语义错误），而不递增则被吊销会话的 access token
	// 在 AccessTokenTTL（默认 2h）内依然可用。批量吊销路径（改密 / 泄露处置 /
	// 退出全部设备）本来就配套 BumpTokenVersion，由 tv 兜底，因此只有单会话吊销
	// 需要这个缓存来把失效时间压到"立即"。
	//
	// 详见 session_revocation_cache.go 顶部的取值依据与"只缓存已吊销态"的取舍说明。
	revokedSessions *revokedSessionCache
}

// NewAuthService 创建 AuthService。
func NewAuthService(
	db *store.DB,
	users *repo.UserRepo,
	jwtMgr *auth.JWTManager,
	cfg config.AuthConf,
) *AuthService {
	s := &AuthService{
		db:     db,
		users:  users,
		jwt:    jwtMgr,
		hasher: auth.NewArgon2Hasher(cfg.Argon2.Memory, cfg.Argon2.Time, cfg.Argon2.Threads),
		sem:    auth.NewSemaphore(cfg.Argon2MaxConcurrency, 32),
		cfg:    cfg,
		// 容量与 TTL 是代码内固定常量而非配置项：它们是内存/性能的内部实现细节，
		// 不是运维需要调节的策略，暴露成配置只会扩大配置面却无实际收益。
		revokedSessions: newRevokedSessionCache(),
	}
	// 登录限流按配置串解析（形如 "5/m"、"20/m"）。解析失败退化为保守默认值，
	// 绝不因配置写错而放行无限制——那等于把登录接口变成无保护的密码喷洒靶子。
	pu, bu, err := config.ParseRate(cfg.LoginLimitPerUser)
	if err != nil {
		slog.Warn("loginLimitPerUser 配置非法，退化为默认 5/m",
			slog.String("value", cfg.LoginLimitPerUser), slog.String("err", err.Error()))
		pu, bu = 5.0/60, 5
	}
	pi, bi, err := config.ParseRate(cfg.LoginLimitPerIP)
	if err != nil {
		slog.Warn("loginLimitPerIP 配置非法，退化为默认 20/m",
			slog.String("value", cfg.LoginLimitPerIP), slog.String("err", err.Error()))
		pi, bi = 20.0/60, 20
	}
	s.userLimit = ratelimit.New(pu, bu, 4096, 15*time.Minute)
	s.ipLimit = ratelimit.New(pi, bi, 4096, 15*time.Minute)
	return s
}

// RegisterResult 是注册响应数据。
//
// ★ 注册必须直接返回可用令牌（openapi 的 201 + TokenResponse），不能让客户端再调一次 /auth/login：
//
//	多一次 Argon2id 校验（19 MiB 内存 + 数百毫秒 CPU），在"节省资源"的目标下是纯浪费；
//	而且第二次登录会让用户输入两遍密码，交互上也是退步。
//
// NeedsLogin 保留为显式字段而不是靠"有没有 accessToken"推断，便于客户端在
// refreshTokenInBody=false 且 refreshCookie 被禁用时明确知道该提示用户去登录。
type RegisterResult struct {
	UserID     int64  `json:"id"`
	Username   string `json:"username"`
	Role       string `json:"role"`
	CreatedAt  string `json:"createdAt"`
	NeedsLogin bool   `json:"needsLogin"`
	// AccessToken 恒返回（注册即建立会话），与 openapi 的 required 一致。
	AccessToken string `json:"accessToken"`
	TokenType   string `json:"tokenType"`
	ExpiresIn   int64  `json:"expiresIn"`
	SessionID   int64  `json:"sessionId"`
	// RefreshToken 仅在 auth.refreshTokenInBody=true 时返回（跨域/原生端需要）。
	RefreshToken string `json:"refreshToken,omitempty"`
}

// Register 创建账号。
// 注册开关关闭时返回 403 FORBIDDEN（单人自托管场景下这是防机器人注册的第一道闸）。
func (s *AuthService) Register(ctx context.Context, username, password, email, clientID, deviceName string) (*RegisterResult, error) {
	if !s.cfg.RegistrationEnabled {
		return nil, apierr.Forbidden("注册已关闭，请联系管理员创建账号")
	}
	username = strings.TrimSpace(username)
	if e := validateUsername(username); e != nil {
		return nil, e
	}
	if e := validatePassword(password); e != nil {
		return nil, e
	}
	email = strings.TrimSpace(email)
	if email != "" && !strings.Contains(email, "@") {
		return nil, apierr.Validation("邮箱格式不正确", []apierr.Details{{Field: "email", Message: "必须是合法邮箱地址"}})
	}
	if clientID = strings.TrimSpace(clientID); clientID == "" {
		clientID = newClientID()
	}

	// 用户名唯一性检查放在哈希之前：避免攻击者用不同用户名把 CPU 打满。
	// 注意这会泄露"用户名是否存在"，但注册接口本就允许枚举（登录接口才不允许）。
	if existing, err := s.users.FindByUsername(ctx, username); err == nil && existing != nil {
		return nil, apierr.Conflict("该用户名已被占用")
	} else if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return nil, apierr.Internal(err)
	}

	hash, err := s.hashWithLimit(ctx, password)
	if err != nil {
		return nil, err
	}

	now := util.NowMs()
	u := &model.User{
		Username:     username,
		PasswordHash: hash,
		Email:        email,
		Role:         "user",
		TokenVersion: 1,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	defer func() { _ = tx.Rollback() }()

	userID, err := s.users.Create(ctx, tx, u)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	// ★ 必须在 Commit 之前写。注册即登录，所以 last_login_at 就在这一刻落地。
	//
	// 之前这行写在 Commit() 之后、拿的还是同一个 tx：*sql.Tx 一旦 Commit，
	// 后续任何 ExecContext 都返回 sql.ErrTxDone，而错误被 `_ =` 吞掉，
	// 结果是注册用户的 last_login_at 永远是 NULL，且没有任何日志线索。
	// 失败不阻断注册（账号已落库），但必须留下日志，否则又是静默失败。
	if err := s.users.UpdateLastLogin(ctx, tx, userID, now); err != nil {
		slog.Warn("注册成功但写入 last_login_at 失败",
			slog.Int64("userId", userID), slog.String("err", err.Error()))
	}
	if err := tx.Commit(); err != nil {
		return nil, apierr.Internal(err)
	}

	// 注册即登录：建会话 + 签发令牌。失败不视为注册失败——
	// 账号已落库，此时返回错误会让用户重试注册并撞上"用户名已占用"，
	// 是比"注册成功但未登录"更糟的结果。所以只降级为 needsLogin。
	u.ID = userID
	sessionID, accessToken, expiresIn, refreshToken, serr := s.issueSession(ctx, u, clientID, deviceName, now)
	if serr != nil {
		slog.Error("注册成功但建立会话失败，已降级为需手动登录",
			slog.Int64("userId", userID), slog.String("err", serr.Error()))
		return &RegisterResult{
			UserID:     userID,
			Username:   username,
			Role:       "user",
			CreatedAt:  util.FormatTime(now),
			NeedsLogin: true,
		}, nil
	}

	res := &RegisterResult{
		UserID:      userID,
		Username:    username,
		Role:        "user",
		CreatedAt:   util.FormatTime(now),
		NeedsLogin:  false,
		AccessToken: accessToken,
		TokenType:   "Bearer",
		ExpiresIn:   expiresIn,
		SessionID:   sessionID,
	}
	if s.cfg.RefreshTokenInBody {
		res.RefreshToken = refreshToken
	}
	return res, nil
}

// LoginResult 是登录/刷新响应数据。
type LoginResult struct {
	UserID      int64  `json:"id"`
	Username    string `json:"username"`
	Role        string `json:"role"`
	AccessToken string `json:"accessToken"`
	TokenType   string `json:"tokenType"`
	ExpiresIn   int64  `json:"expiresIn"` // 秒
	SessionID   int64  `json:"sessionId"`
	// RefreshToken 仅在 auth.refreshTokenInBody=true 时返回（跨域部署需要）。
	RefreshToken string `json:"refreshToken,omitempty"`
}

// CreateUserByAdmin 由管理员代建账号。
//
// ★ 存在的理由很具体：配置 auth.registrationEnabled=false 之后，
//
//	如果后台没有建号入口，这个系统就**再也进不去新人**了 ——
//	关闭注册是常见的安全加固动作，缺了这条路等于把门锁死。
//
// 与 Register 的关键差异：
//   - 不受 registrationEnabled 限制（管理员本身就是显式授权的来源）；
//   - 可以指定 role；
//   - **不建会话、不签发令牌**。管理员建号是给别人建的，
//     顺手把新账号的 access token 返回给管理员是一次凭据泄漏。
//     返回的用户需要自己走正常登录流程。
func (s *AuthService) CreateUserByAdmin(ctx context.Context, actorID int64,
	username, password, email, role string) (*model.User, error) {
	username = strings.TrimSpace(username)
	if e := validateUsername(username); e != nil {
		return nil, e
	}
	if e := validatePassword(password); e != nil {
		return nil, e
	}
	email = strings.TrimSpace(email)
	if email != "" && !strings.Contains(email, "@") {
		return nil, apierr.Validation("邮箱格式不正确", []apierr.Details{{Field: "email", Message: "必须是合法邮箱地址"}})
	}
	if role == "" {
		role = model.RoleUser
	}
	if role != model.RoleUser && role != model.RoleAdmin {
		return nil, apierr.Validation("role 取值非法", []apierr.Details{{Field: "role", Message: "只能是 user 或 admin"}})
	}
	if existing, err := s.users.FindByUsername(ctx, username); err == nil && existing != nil {
		return nil, apierr.Conflict("该用户名已被占用")
	} else if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return nil, apierr.Internal(err)
	}

	hash, err := s.hashWithLimit(ctx, password)
	if err != nil {
		return nil, err
	}
	now := util.NowMs()
	u := &model.User{
		Username:     username,
		PasswordHash: hash,
		Email:        email,
		Role:         role,
		TokenVersion: 1,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	defer func() { _ = tx.Rollback() }()
	userID, err := s.users.Create(ctx, tx, u)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, apierr.Internal(err)
	}
	u.ID = userID
	slog.Info("管理员创建了账号", slog.Int64("actorId", actorID),
		slog.Int64("userId", userID), slog.String("role", role))
	return u, nil
}

// Login 校验用户名密码并签发双 Token。
//
// 失败路径全部返回同一个错误（INVALID_CREDENTIALS），不区分"用户不存在"与"密码错误"，
// 否则可被用于枚举有效用户名。
func (s *AuthService) Login(ctx context.Context, username, password, clientID, deviceName, ip string) (*LoginResult, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return nil, apierr.InvalidCredentials()
	}
	if !s.ipLimit.Allow(ip) {
		return nil, apierr.RateLimited("登录请求过于频繁，请稍后再试")
	}
	if !s.userLimit.Allow(username) {
		return nil, apierr.RateLimited("该账号登录尝试过于频繁，请稍后再试")
	}
	// 账号锁定：连续失败达到阈值后，在 lockoutDuration 内直接拒绝（哪怕密码正确）。
	if s.isLockedOut(username) {
		return nil, apierr.RateLimited("账号已临时锁定，请稍后再试")
	}

	user, err := s.users.FindByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			// 用户不存在时也执行一次哈希，抹平时序差异（枚举防护）。
			_, _ = s.hasher.Hash(password)
			return nil, apierr.InvalidCredentials()
		}
		return nil, apierr.Internal(err)
	}

	ok, err := s.verifyWithLimit(ctx, password, user.PasswordHash)
	if err != nil {
		return nil, err
	}
	if !ok {
		s.recordFailure(username)
		return nil, apierr.InvalidCredentials()
	}
	s.clearFailure(username)

	// 停用的账号不许登录。
	//
	// ★ 检查必须放在**密码校验之后**：若放在之前，攻击者可以用"是否返回
	//   账号已停用"来枚举用户名 —— 这条旁路会让前面的时序抹平防护白做。
	//   顺序对了之后，停用信息与"账号是否存在"一样，只有知道密码的人看得到。
	if user.Disabled() {
		return nil, apierr.Forbidden("账号已被停用，请联系管理员")
	}

	if clientID = strings.TrimSpace(clientID); clientID == "" {
		clientID = newClientID()
	}
	now := util.NowMs()
	sessionID, accessToken, expiresIn, refreshToken, err := s.issueSession(ctx, user, clientID, deviceName, now)
	if err != nil {
		return nil, err
	}

	// 更新最后登录时间（失败不影响登录本身）。
	if tx, terr := s.db.BeginWrite(ctx); terr == nil {
		_ = s.users.UpdateLastLogin(ctx, tx, user.ID, now)
		_ = tx.Commit()
	}
	// 登录成功解除该用户维度的限流惩罚。
	s.userLimit.Reset(username)

	res := &LoginResult{
		UserID:      user.ID,
		Username:    user.Username,
		Role:        user.Role,
		AccessToken: accessToken,
		TokenType:   "Bearer",
		ExpiresIn:   expiresIn,
		SessionID:   sessionID,
	}
	if s.cfg.RefreshTokenInBody {
		res.RefreshToken = refreshToken
	}
	return res, nil
}

// issueSession 创建一个新会话行并签发配套的 Access Token。
// 返回 (sessionID, accessToken, accessExpiresInSeconds, refreshToken, error)。
//
// 会话行与 Access Token 的 sid 必须一致，所以两件事必须在同一个事务边界内确定：
// 先 INSERT 拿到自增 id，再用该 id 签 JWT；签发失败则整笔回滚，不留孤儿会话。
func (s *AuthService) issueSession(ctx context.Context, u *model.User, clientID, deviceName string, now int64) (int64, string, int64, string, error) {
	refreshToken, err := auth.NewRefreshToken()
	if err != nil {
		return 0, "", 0, "", apierr.Internal(err)
	}

	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return 0, "", 0, "", apierr.Internal(err)
	}
	defer func() { _ = tx.Rollback() }()

	id, err := s.users.InsertSession(ctx, tx, &model.Session{
		UserID:           u.ID,
		ClientID:         clientID,
		RefreshTokenHash: auth.HashToken(refreshToken),
		DeviceName:       deviceName,
		LastSeenAt:       now,
		ExpiresAt:        now + s.cfg.RefreshTokenTTL.Std().Milliseconds(),
		CreatedAt:        now,
	})
	if err != nil {
		return 0, "", 0, "", apierr.Internal(err)
	}

	accessToken, exp, err := s.jwt.Sign(u.ID, id, u.TokenVersion)
	if err != nil {
		return 0, "", 0, "", apierr.Internal(err)
	}
	if err := tx.Commit(); err != nil {
		return 0, "", 0, "", apierr.Internal(err)
	}
	return id, accessToken, int64(exp.Sub(time.Now()).Seconds()), refreshToken, nil
}

// RefreshResult 是刷新响应。
//
// ★ 必须携带用户信息（openapi 的 TokenResponse.user 为required）：
//
//	客户端在 access token 过期后靠 refresh 恢复登录态，此时它的内存里已经没有用户资料了。
//	若响应里没有 user，客户端刷新成功后仍显示未登录/空白，必须再调一次 /me。
type RefreshResult struct {
	UserID       int64  `json:"id"`
	Username     string `json:"username"`
	Role         string `json:"role"`
	AccessToken  string `json:"accessToken"`
	TokenType    string `json:"tokenType"`
	ExpiresIn    int64  `json:"expiresIn"`
	SessionID    int64  `json:"sessionId"`
	RefreshToken string `json:"refreshToken,omitempty"`
}

// Refresh 轮换 Refresh Token 并签发新的 Access Token。
//
// ★ DEC-7 reuse 两级判定（本函数是全项目最容易改错的地方，改之前务必读完）：
//
//	情形 A：库里 refresh_token_hash 命中，且会话有效
//	         → 正常轮换（原地 UPDATE，旧 hash 移入 prev_*）
//	情形 B：库里 refresh_token_hash 命中，但会话已过期/已吊销
//	         → 视为泄露：撤销该用户全部会话 + token_version+1
//	情形 C：当前 hash 未命中，但 prev_refresh_token_hash 命中，
//	         且 now - prev_rotated_at <= graceSec 且 client_id 相同
//	         → 宽限重放（弱网重试 / 客户端并发刷新）：幂等地再轮换一次，
//	           **不**撤销会话、不递增 token_version
//	情形 D：其余（超窗重放或异设备重放）
//	         → 判定泄露：撤销全部会话 + token_version+1
//
// 情形 C 是 Android 弱网下的刚需：客户端超时重发旧 token 若被当成泄露，
// 会把用户所有设备一起踢下线。
func (s *AuthService) Refresh(ctx context.Context, refreshToken, clientID, deviceName string) (*RefreshResult, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, apierr.Unauthorized("缺少 refresh token")
	}
	hash := auth.HashToken(refreshToken)
	now := util.NowMs()
	clientID = strings.TrimSpace(clientID)

	// 先查当前代
	sess, err := s.users.FindSessionByHash(ctx, hash)
	switch {
	case err == nil && sess != nil:
		return s.handleCurrentSession(ctx, sess, clientID, deviceName, now)
	case err != nil && !errors.Is(err, repo.ErrNotFound):
		return nil, apierr.Internal(err)
	}

	// 当前代未命中 → 查上一代（宽限窗口）
	prev, perr := s.users.FindSessionByPrevHash(ctx, hash)
	if perr != nil && !errors.Is(perr, repo.ErrNotFound) {
		return nil, apierr.Internal(perr)
	}
	if prev != nil && s.isGraceReplay(prev, clientID, now) {
		// 情形 C：同设备 + 宽限窗口内 → 幂等再轮换一次。
		return s.rotateSession(ctx, prev, clientID, deviceName, now)
	}

	// 情形 B / D：token 对应的会话已被作废，或超出宽限窗口 / 异设备重放 → 判定泄露。
	// ★ 与 handleCurrentSession 同理：先分辨是不是"被管理员停用"。
	//   被停用的账号在停用那一刻全部会话都被吊销，而客户端手里往往还是**上一代**
	//   refresh token，于是直接落到这个分支。报 401 会让客户端去重试一个必然失败的
	//   流程；报 403 才能让它立刻跳出登录态。
	ownerID := int64(0)
	if prev != nil {
		ownerID = prev.UserID
	} else {
		ownerID = s.lookupRevokedOwner(ctx, hash)
	}
	if ownerID > 0 {
		if _, disabled, derr := s.users.GetAccountGuard(ctx, ownerID); derr == nil && disabled {
			return nil, apierr.Forbidden("账号已被停用，请联系管理员")
		}
	}
	if prev != nil {
		s.revokeAllOnLeak(ctx, prev.UserID, "refresh_reuse_out_of_window")
	} else if ownerID > 0 {
		s.revokeAllOnLeak(ctx, ownerID, "refresh_reuse_revoked")
	}
	return nil, apierr.Unauthorized("登录已失效，请重新登录")
}

// isGraceReplay 判定是否落在 DEC-7 宽限窗口内。
func (s *AuthService) isGraceReplay(sess *model.Session, clientID string, now int64) bool {
	if sess == nil || sess.PrevRotatedAt <= 0 {
		return false
	}
	graceMs := int64(s.cfg.RefreshReuseGraceSec) * 1000
	if now-sess.PrevRotatedAt > graceMs {
		return false
	}
	// client_id 相同才认定为弱网重试；异设备重放一律视为泄露。
	return clientID != "" && clientID == sess.ClientID
}

// handleCurrentSession 处理 refresh_token_hash 命中当前代的场景。
func (s *AuthService) handleCurrentSession(ctx context.Context, sess *model.Session, clientID, deviceName string, now int64) (*RefreshResult, error) {
	// 会话已被吊销：令牌在泄露后被作废，任何重放都必须按泄露处理。
	if sess.RevokedAt > 0 {
		// ★ 但先分辨"被吊销"的原因。管理员停用账号时会在同一个事务里吊销全部会话，
		//   因此被停用的用户往往**走不到** rotateSession 的停用检查 —— 会话在更早的
		//   这里就已经是 revoked 了。
		//   若不在这里补判，客户端拿到的是 401（"登录已失效，请重新登录"），而按契约
		//   401 意味着"去 refresh 然后重放" —— 于是客户端会拿着这个永远不可能成功的
		//   token 反复重试，直到 refresh 也拿到 401 才放弃。403 才是"立即跳出登录态"。
		//   也不要做 revokeAllOnLeak：那是给"令牌泄露"用的，被停用不是泄露。
		//
		// ★ 与 rotateSession 里的同类检查构成**冗余防御**（已用变异测试确认：
		//   单独移除这一处不会让任何用例转红，两处同时移除才会）。这里刻意保留冗余——
		//   停用是安全属性，多一道防线是优点；但要清楚知道它测不到单独一层，
		//   不要误以为"这条用例覆盖了本行"。
		if _, disabled, derr := s.users.GetAccountGuard(ctx, sess.UserID); derr == nil && disabled {
			return nil, apierr.Forbidden("账号已被停用，请联系管理员")
		}
		s.revokeAllOnLeak(ctx, sess.UserID, "refresh_reuse_revoked")
		return nil, apierr.Unauthorized("登录已失效，请重新登录")
	}
	if sess.ExpiresAt <= now {
		s.revokeAllOnLeak(ctx, sess.UserID, "refresh_expired_replay")
		return nil, apierr.Unauthorized("登录已过期，请重新登录")
	}
	// client_id 变了但用了有效 token：可能是 token 被复制到另一台设备。
	// 保守处理——同 IP 视为正常（同一浏览器换了 clientId），跨 client_id 直接拒绝，
	// 但不牵连全量会话（避免误伤正常用户）。
	if clientID != "" && sess.ClientID != "" && clientID != sess.ClientID {
		slog.Warn("refresh client_id mismatch",
			slog.Int64("sessionId", sess.ID), slog.Int64("userId", sess.UserID),
			slog.String("stored", sess.ClientID), slog.String("presented", clientID))
		return nil, apierr.Unauthorized("登录已失效，请重新登录")
	}
	if clientID == "" {
		clientID = sess.ClientID
	}
	return s.rotateSession(ctx, sess, clientID, deviceName, now)
}

// rotateSession 执行一次原地轮换并签发新 Access Token。
//
// ★ 这里也是"停用"必须拦住的一道口子：refresh token 有效期按天计，
//
//	若只拦密码登录和 access token 校验，被停用的用户只要不断 refresh
//	就能无限续期。三条入口（登录 / access 校验 / refresh）缺一条，
//	停用这个动作就漏了。
func (s *AuthService) rotateSession(ctx context.Context, sess *model.Session, clientID, deviceName string, now int64) (*RefreshResult, error) {
	if _, disabled, err := s.users.GetAccountGuard(ctx, sess.UserID); err != nil {
		if !errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.Internal(err)
		}
	} else if disabled {
		return nil, apierr.Forbidden("账号已被停用，请联系管理员")
	}
	newRefresh, err := auth.NewRefreshToken()
	if err != nil {
		return nil, apierr.Internal(err)
	}
	next := &model.Session{
		UserID:           sess.UserID,
		ClientID:         clientID,
		RefreshTokenHash: auth.HashToken(newRefresh),
		DeviceName:       deviceName,
		ExpiresAt:        now + s.cfg.RefreshTokenTTL.Std().Milliseconds(),
	}

	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	defer func() { _ = tx.Rollback() }()

	n, err := s.users.RotateSession(ctx, tx, sess.ID, next, now)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	if n == 0 {
		// 并发刷新时另一请求已先一步轮换并把 revoked_at/prev 改掉了。
		// 此时本次不应作废会话（另一请求是合法的），直接让客户端重试即可。
		return nil, apierr.Unauthorized("登录已刷新，请重试")
	}
	// 取最新 token_version：并发场景下可能已被改密/泄露处置递增过。
	user, err := s.users.FindByID(ctx, sess.UserID)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, apierr.Internal(err)
	}

	accessToken, exp, err := s.jwt.Sign(user.ID, sess.ID, user.TokenVersion)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	return &RefreshResult{
		UserID:       user.ID,
		Username:     user.Username,
		Role:         user.Role,
		AccessToken:  accessToken,
		TokenType:    "Bearer",
		ExpiresIn:    int64(exp.Sub(time.Now()).Seconds()),
		SessionID:    sess.ID,
		RefreshToken: newRefresh,
	}, nil
}

// lookupRevokedOwner 尝试从已吊销会话中反查 token 归属的用户，用于泄露判定。
func (s *AuthService) lookupRevokedOwner(ctx context.Context, hash string) int64 {
	sess, err := s.users.FindSessionByHash(ctx, hash)
	if err != nil || sess == nil {
		return 0
	}
	return sess.UserID
}

// revokeAllOnLeak 泄露处置：撤销该用户全部会话并递增 token_version。
//
// 注意这里不做"锁账号"（避免被恶意触发后无法登录），只作废全部凭证。
func (s *AuthService) revokeAllOnLeak(ctx context.Context, userID int64, reason string) {
	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		slog.Error("泄露处置失败：无法开启写事务", slog.Int64("userId", userID), slog.String("reason", reason))
		return
	}
	defer func() { _ = tx.Rollback() }()
	now := util.NowMs()
	n, err := s.users.RevokeAllSessions(ctx, tx, userID, now)
	if err != nil {
		slog.Error("泄露处置失败：撤销会话出错",
			slog.Int64("userId", userID), slog.String("reason", reason), slog.String("err", err.Error()))
		return
	}
	if _, err := s.users.BumpTokenVersion(ctx, tx, userID, now); err != nil {
		slog.Error("泄露处置失败：递增 token_version 出错",
			slog.Int64("userId", userID), slog.String("reason", reason), slog.String("err", err.Error()))
		return
	}
	if err := tx.Commit(); err != nil {
		slog.Error("泄露处置失败：提交出错", slog.Int64("userId", userID), slog.String("err", err.Error()))
		return
	}
	slog.Warn("检测到 refresh token 重放，已撤销全部会话",
		slog.Int64("userId", userID), slog.String("reason", reason), slog.Int64("revoked", n))
}

// Logout 按 sid 作废当前会话。
//
// DEC-14：sid 来自 JWT（Claims.SessionID），与传输通道解耦。
// 传 sid=0 或找不到会话时返回 nil 而非 401——登出必须幂等，
// 重复登出、token 已失效后再登出，都不该报错。
//
// refreshToken 是回退通道（openapi 允许请求体携带）：当 sid 缺失时用它反查会话。
// 校验 userID 归属，防止拿别人泄露的 refreshToken 去踢掉他人会话。
func (s *AuthService) Logout(ctx context.Context, userID, sessionID int64, refreshToken string) error {
	if sessionID <= 0 && strings.TrimSpace(refreshToken) != "" {
		sess, err := s.users.FindSessionByHash(ctx, auth.HashToken(refreshToken))
		switch {
		case err == nil && sess != nil:
			// 只接受归属当前用户的会话；不匹配时按"无 sid"路径处理。
			if userID <= 0 || sess.UserID == userID {
				sessionID = sess.ID
				userID = sess.UserID
			}
		case err != nil && !errors.Is(err, repo.ErrNotFound):
			return apierr.Internal(err)
		}
	}
	if sessionID <= 0 {
		// 无 sid 且无法用 refreshToken 反查（极老的无sid token）：
		// 退化为按 user 撤销全部会话。宁可多登出，也不要登不出。
		if userID <= 0 {
			// 连 userID 都没有：无法定位任何会话，只清客户端态（handler 回 204）。
			return nil
		}
		s.revokeAllOnLeak(ctx, userID, "logout_without_sid")
		return nil
	}
	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return apierr.Internal(err)
	}
	defer func() { _ = tx.Rollback() }()
	now := util.NowMs()
	if err := s.users.RevokeSession(ctx, tx, sessionID, userID, now); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			// 已经登出过：幂等成功。
			// 仍要写缓存：否则一个"库里已吊销但缓存无记录"的会话，
			// 其残留 access token 会因缓存未命中而每次回源，直到某次回源才被判失效。
			// 写缓存让幂等重登出同样具备"立即失效"的效果。
			s.noteSessionRevoked(sessionID)
			return nil
		}
		return apierr.Internal(err)
	}
	if err := tx.Commit(); err != nil {
		return apierr.Internal(err)
	}
	// 提交成功后写缓存：登出后该会话的 access token 立即 401。
	s.noteSessionRevoked(sessionID)
	return nil
}

// LogoutAll 登出全部设备（"退出所有设备"按钮）。
func (s *AuthService) LogoutAll(ctx context.Context, userID int64) (int64, error) {
	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return 0, apierr.Internal(err)
	}
	defer func() { _ = tx.Rollback() }()
	n, err := s.users.RevokeAllSessions(ctx, tx, userID, util.NowMs())
	if err != nil {
		return 0, apierr.Internal(err)
	}
	// 让已签发的 Access Token 立即失效。
	if _, err := s.users.BumpTokenVersion(ctx, tx, userID, util.NowMs()); err != nil {
		return 0, apierr.Internal(err)
	}
	if err := tx.Commit(); err != nil {
		return 0, apierr.Internal(err)
	}
	return n, nil
}

// ChangePassword 修改密码。
//
// ★ 安全语义（不要为了"体验好"而削弱它）：
//
//	改密后必须递增 token_version（使全部已签发 Access Token 立即失效）
//	并撤销全部会话（使全部 Refresh Token 失效）。
//	理由：改密的典型动机正是"怀疑密码泄露"，此时任何继续存活的凭证都必须失效。
//
// ★ 体验处理：撤销全部会话后，为**当前设备**重新签发一对令牌。
//
//	这样做既满足"其他设备全部被踢下线"的安全目标，
//	又不会让正在改密的这台设备被一起踢出（否则用户改完密码立刻看到登录页，交互很差）。
//	注意新令牌的 tv 是递增后的值，所以旧 token 依然无效。
//	currentSessionID <= 0（拿不到 sid 的老 token）时不签发，前端按 reloginRequired 处理。
func (s *AuthService) ChangePassword(ctx context.Context, userID, currentSessionID int64, oldPassword, newPassword string) (*LoginResult, error) {
	if e := validatePassword(newPassword); e != nil {
		return nil, e
	}
	if oldPassword == newPassword {
		return nil, apierr.Validation("新密码不能与旧密码相同",
			[]apierr.Details{{Field: "newPassword", Message: "必须与旧密码不同"}})
	}
	res := &LoginResult{
		TokenType: "Bearer",
	}
	user, err := s.users.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("账号不存在")
		}
		return nil, apierr.Internal(err)
	}
	res.UserID = user.ID
	res.Username = user.Username
	res.Role = user.Role
	ok, err := s.verifyWithLimit(ctx, oldPassword, user.PasswordHash)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, apierr.InvalidCredentials()
	}
	newHash, err := s.hashWithLimit(ctx, newPassword)
	if err != nil {
		return nil, err
	}

	// 必须在撤销之前取旧会话的 client_id / device_name：
	// RevokeAllSessions 之后该行已作废，届时就查不到了。
	// 沿用 client_id 是硬要求 —— 同设备重放旧 refresh token 时，
	// DEC-7 靠 client_id 相同才判为"弱网重试"而不是"异设备泄露"，
	// 换一个新的 client_id 会让这台设备自己把自己踢下线。
	var keepClientID, keepDeviceName string
	if cur, cerr := s.users.FindSessionByID(ctx, currentSessionID, userID); cerr == nil && cur != nil {
		keepClientID, keepDeviceName = cur.ClientID, cur.DeviceName
	}

	tx, terr := s.db.BeginWrite(ctx)
	if terr != nil {
		return nil, apierr.Internal(terr)
	}
	defer func() { _ = tx.Rollback() }()
	now := util.NowMs()
	if err := s.users.UpdatePassword(ctx, tx, userID, newHash, now); err != nil {
		return nil, apierr.Internal(err)
	}
	// 改密后必须让所有已签发的 Access Token 失效，否则旧 token 在 2 小时内仍可读写用户数据。
	newTV, err := s.users.BumpTokenVersion(ctx, tx, userID, now)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	if _, err := s.users.RevokeAllSessions(ctx, tx, userID, now); err != nil {
		return nil, apierr.Internal(err)
	}
	// 当前设备的后继会话：id 会变（新行），所以客户端的 sessionId 需要更新。
	// 这不影响安全性：旧 sid 所在行已被 RevokeAllSessions 作废。
	var newSessionID int64
	if keepClientID != "" {
		refreshToken, rerr := auth.NewRefreshToken()
		if rerr == nil {
			newSessionID, err = s.users.InsertSession(ctx, tx, &model.Session{
				UserID:           user.ID,
				ClientID:         keepClientID,
				RefreshTokenHash: auth.HashToken(refreshToken),
				DeviceName:       keepDeviceName,
				LastSeenAt:       now,
				ExpiresAt:        now + s.cfg.RefreshTokenTTL.Std().Milliseconds(),
				CreatedAt:        now,
			})
			if err != nil {
				// 建后继会话失败不影响改密本身（用户重新登录即可），降级为需重新登录。
				newSessionID = 0
			} else {
				accessToken, exp, serr := s.jwt.Sign(user.ID, newSessionID, newTV)
				if serr != nil {
					newSessionID = 0
				} else {
					res.AccessToken = accessToken
					res.ExpiresIn = int64(exp.Sub(time.Now()).Seconds())
					res.SessionID = newSessionID
					if s.cfg.RefreshTokenInBody {
						res.RefreshToken = refreshToken
					}
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, apierr.Internal(err)
	}
	s.clearFailure(user.Username)
	return res, nil
}

// DeleteAccount 注销账号（不可逆）。
func (s *AuthService) DeleteAccount(ctx context.Context, userID int64, password string) error {
	user, err := s.users.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return apierr.NotFound("账号不存在")
		}
		return apierr.Internal(err)
	}
	// 必须校验密码：防止 token 泄露即导致账号被销毁。
	ok, err := s.verifyWithLimit(ctx, password, user.PasswordHash)
	if err != nil {
		return err
	}
	if !ok {
		return apierr.InvalidCredentials()
	}
	tx, terr := s.db.BeginWrite(ctx)
	if terr != nil {
		return apierr.Internal(terr)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.users.DeleteUser(ctx, tx, userID); err != nil {
		return apierr.Internal(err)
	}
	if err := tx.Commit(); err != nil {
		return apierr.Internal(err)
	}
	slog.Info("账号已注销", slog.Int64("userId", userID))
	return nil
}

// SessionView 是会话列表项（不暴露任何 token 哈希）。
type SessionView struct {
	ID         int64  `json:"id"`
	DeviceName string `json:"deviceName"`
	ClientID   string `json:"clientId"`
	LastSeenAt string `json:"lastSeenAt"`
	CreatedAt  string `json:"createdAt"`
	ExpiresAt  string `json:"expiresAt"`
	Current    bool   `json:"current"`
}

// ListSessions 返回用户的活跃会话，并标记当前会话。
func (s *AuthService) ListSessions(ctx context.Context, userID, currentSessionID int64) ([]SessionView, error) {
	list, err := s.users.ListSessions(ctx, userID, util.NowMs())
	if err != nil {
		return nil, apierr.Internal(err)
	}
	out := make([]SessionView, 0, len(list))
	for _, s := range list {
		out = append(out, SessionView{
			ID:         s.ID,
			DeviceName: s.DeviceName,
			ClientID:   s.ClientID,
			LastSeenAt: util.FormatTime(s.LastSeenAt),
			CreatedAt:  util.FormatTime(s.CreatedAt),
			ExpiresAt:  util.FormatTime(s.ExpiresAt),
			Current:    s.ID == currentSessionID,
		})
	}
	return out, nil
}

// RevokeSessionByID 作废指定会话（仅能作废自己的）。
func (s *AuthService) RevokeSessionByID(ctx context.Context, userID, sessionID int64) error {
	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return apierr.Internal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.users.RevokeSession(ctx, tx, sessionID, userID, util.NowMs()); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return apierr.NotFound("会话不存在或已失效")
		}
		return apierr.Internal(err)
	}
	if err := tx.Commit(); err != nil {
		return apierr.Internal(err)
	}
	// 提交成功后才写缓存：让该会话的 access token 从此刻起立即 401。
	// 单会话吊销刻意不递增 token_version——那会连带踢掉该用户所有设备，语义错误。
	s.noteSessionRevoked(sessionID)
	return nil
}

// GetUser 读取用户资料。
func (s *AuthService) GetUser(ctx context.Context, userID int64) (*model.User, error) {
	u, err := s.users.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("账号不存在")
		}
		return nil, apierr.Internal(err)
	}
	return u, nil
}

// VerifyTokenVersion 校验 JWT 中的 tv 是否仍与库内一致。
//
// 每请求一次 DB 查询是"无 Redis"的代价；调用方应配合短 TTL 缓存（见 httpapi 中间件）。
func (s *AuthService) VerifyTokenVersion(ctx context.Context, userID int64, claimsTV int32) error {
	tv, disabled, err := s.users.GetAccountGuard(ctx, userID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return apierr.Unauthorized("账号不存在")
		}
		return apierr.Internal(err)
	}
	// ★ 停用必须在**这里**拦，而不是只在manage后台的写操作上拦。
	//   否则被停用的用户凭手中还没过期的 access token，最长还能用满 2 小时 ——
	//   停用按钮就成了"下次登录才生效"的空头支票，而停用的典型场景
	//   （封禁捣乱账号）恰恰要求立刻生效。
	//   返回 403 而非 401：401 会触发客户端 refresh 重试，refresh 也会同样被拒，
	//   于是前端陷入静默重试循环；403 明确表达"这个身份不被允许"，客户端应直接登出。
	if disabled {
		return apierr.Forbidden("账号已被停用")
	}
	if tv != claimsTV {
		// tv 不一致 = 期间发生过改密 / 全局登出 / 泄露处置。
		return apierr.Unauthorized("登录已失效，请重新登录")
	}
	return nil
}

// VerifySessionActive 校验 sid 对应会话未被单独吊销。
//
// ★ 它与 VerifyTokenVersion 是互补而非重复的两道闸门：
//   - VerifyTokenVersion 管**用户级**失效（改密 / 泄露处置 / 退出全部设备 → tv+1）；
//   - 本方法管**会话级**失效（只踢掉某一个设备 → 该 sid 立即作废）。
//
// 早期实现只有前者，导致"删除某个会话后它的 access token 还能用 2 小时"。
//
// 热路径开销：缓存未命中时多一次主键索引点查（见 repo.UserRepo.IsSessionActive）。
// 命中缓存（已吊销）则零 DB 查询。
//
// sessionID <= 0 时直接放行：那是旧版本签发、不带 sid 的 token，
// 它们的失效判定完全交给 token_version，此处不做额外拦截以保持既有行为。
func (s *AuthService) VerifySessionActive(ctx context.Context, userID, sessionID int64) error {
	if s == nil || s.users == nil || sessionID <= 0 {
		return nil
	}
	if revoked, hit := s.revokedSessions.Lookup(sessionID); hit && revoked {
		return apierr.Unauthorized("登录已失效，请重新登录")
	}
	active, err := s.users.IsSessionActive(ctx, sessionID, userID)
	if err != nil {
		return apierr.Internal(err)
	}
	if !active {
		// 回源确认已吊销/已失效 → 写缓存，让后续请求零 DB 查询直接 401。
		s.revokedSessions.Put(sessionID, time.Now())
		return apierr.Unauthorized("登录已失效，请重新登录")
	}
	return nil
}

// noteSessionRevoked 在吊销事务**提交成功后**把会话写入吊销缓存。
//
// ⚠️ 必须在 Commit 之后调用：事务若回滚而缓存已写入，会留下一个"实际未吊销"的
// 假吊销条目，用户会被无端踢下线（且要等 30 分钟 TTL 或 DB 复核才可能恢复）。
func (s *AuthService) noteSessionRevoked(sessionID int64) {
	if s == nil || sessionID <= 0 {
		return
	}
	s.revokedSessions.Put(sessionID, time.Now())
}

// CleanupExpiredSessions 清理过期会话（供 Janitor 周期调用）。
func (s *AuthService) CleanupExpiredSessions(ctx context.Context, revokedRetentionMs int64) (int64, error) {
	tx, err := s.db.BeginWrite(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	n, err := s.users.DeleteExpiredSessions(ctx, tx, util.NowMs(), revokedRetentionMs)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// hashWithLimit 在并发受限的情况下计算 Argon2id 哈希。
// 信号量满且排队超时 → 503 SERVICE_BUSY（快速失败优于无限期占用连接）。
func (s *AuthService) hashWithLimit(ctx context.Context, password string) (string, error) {
	if err := s.acquireSlot(ctx); err != nil {
		return "", err
	}
	defer s.sem.Release()
	hash, err := s.hasher.Hash(password)
	if err != nil {
		return "", apierr.Internal(err)
	}
	return hash, nil
}

// verifyWithLimit 在并发受限的情况下校验密码。
func (s *AuthService) verifyWithLimit(ctx context.Context, password, phc string) (bool, error) {
	if err := s.acquireSlot(ctx); err != nil {
		return false, err
	}
	defer s.sem.Release()
	ok, err := s.hasher.Verify(password, phc)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidPHC) {
			// 库里 PHC 损坏：这是运维问题，不能当成"密码错误"糊弄过去。
			slog.Error("密码哈希 PHC 格式非法", slog.String("err", err.Error()))
			return false, apierr.Internal(err)
		}
		return false, apierr.Internal(err)
	}
	return ok, nil
}

// acquireSlot 获取 Argon2id 并发槽，带排队超时。
func (s *AuthService) acquireSlot(ctx context.Context) error {
	if s.cfg.Argon2QueueTimeoutMs <= 0 {
		return s.sem.Acquire(ctx)
	}
	timeout := time.Duration(s.cfg.Argon2QueueTimeoutMs) * time.Millisecond
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	sub, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- s.sem.Acquire(sub) }()
	select {
	case err := <-errCh:
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return apierr.New(http.StatusServiceUnavailable, "SERVICE_BUSY",
					"系统繁忙，请稍后重试").WithHeader("Retry-After", "2")
			}
			return apierr.Internal(err)
		}
		return nil
	case <-timer.C:
		// 排队超时。注意：这里不释放任何槽位（还没拿到）。
		return apierr.New(http.StatusServiceUnavailable, "SERVICE_BUSY",
			"系统繁忙，请稍后重试").WithHeader("Retry-After", "2")
	}
}

// maxLoginFails 是触发账号锁定的连续失败次数。
const maxLoginFails = 10

// failState 是单账号的失败状态。
//
// 必须以指针存进 sync.Map：存值拷贝会让不同请求各自看到独立计数，锁定逻辑直接失效。
type failState struct {
	mu        sync.Mutex
	count     int
	lockedTil int64 // 锁定截止时间（epoch 毫秒），0 = 未锁定
}

// locked 判断账号是否仍在锁定期。
func (f *failState) locked(now int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedTil > now
}

// fail 记录一次失败，必要时进入锁定期。
func (f *failState) fail(now, lockoutMs int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
	if f.count >= maxLoginFails {
		f.lockedTil = now + lockoutMs
		f.count = 0 // 进入锁定期后计数清零，锁定结束后重新起算
	}
}

// reset 在登录成功后清空状态。
func (f *failState) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count = 0
	f.lockedTil = 0
}

// idle 判断是否可被清理（未锁定且计数为 0）。
func (f *failState) idle() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count == 0 && f.lockedTil == 0
}

// isLockedOut 判断账号是否处于锁定期。
func (s *AuthService) isLockedOut(username string) bool {
	v, ok := s.failCount.Load(username)
	if !ok {
		return false
	}
	return v.(*failState).locked(util.NowMs())
}

// recordFailure 记录一次失败；达到阈值则锁定 lockoutDuration。
func (s *AuthService) recordFailure(username string) {
	v, ok := s.failCount.Load(username)
	if !ok {
		v, _ = s.failCount.LoadOrStore(username, &failState{})
	}
	v.(*failState).fail(util.NowMs(), s.cfg.LockoutDuration.Std().Milliseconds())
}

// clearFailure 登录成功后清零失败计数与锁定。
func (s *AuthService) clearFailure(username string) {
	if v, ok := s.failCount.Load(username); ok {
		v.(*failState).reset()
	}
}

// ExpiredFails 清理已解锁且无失败计数的条目（由 Janitor 周期调用，防止内存无界增长）。
//
// sync.Map 没有 TTL，只能扫全表删空闲项；条目数上限由"曾登录失败的用户名数"决定，
// 且每 janitorInterval 才扫一次，开销可忽略。
func (s *AuthService) ExpiredFails() {
	s.failCount.Range(func(k, v any) bool {
		if v.(*failState).idle() {
			s.failCount.Delete(k)
		}
		return true
	})
}

// newClientID 生成一个随机的 client_id。
func newClientID() string {
	t, err := auth.NewRefreshToken()
	if err != nil {
		// 随机源不可用属于极端情况，退化为时间串（仍能保证"不同设备不同值"这一弱目标）。
		return fmt.Sprintf("c%d", time.Now().UnixNano())
	}
	return "c" + t[:16]
}

// --- 参数校验 ---

// validateUsername 校验用户名：3-32 字符，字母/数字/下划线/短横线。
func validateUsername(u string) error {
	if len(u) < 3 || len(u) > 32 {
		return apierr.Validation("用户名长度须为 3-32 个字符",
			[]apierr.Details{{Field: "username", Message: "长度须为 3-32"}})
	}
	for _, r := range u {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return apierr.Validation("用户名只能包含字母、数字、下划线、短横线与点",
				[]apierr.Details{{Field: "username", Message: "含非法字符"}})
		}
	}
	return nil
}

// validatePassword 校验密码强度：8-72 字符，至少包含两类字符。
// 上限 72 是为了兼容未来可能切换到 bcrypt 时的限制。
func validatePassword(p string) error {
	if len(p) < 8 {
		return apierr.Validation("密码长度至少 8 位",
			[]apierr.Details{{Field: "password", Message: "至少 8 位"}})
	}
	if len(p) > 72 {
		return apierr.Validation("密码长度不能超过 72 位",
			[]apierr.Details{{Field: "password", Message: "最长 72 位"}})
	}
	var hasLower, hasUpper, hasDigit, hasSymbol bool
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= '0' && r <= '9':
			hasDigit = true
		default:
			hasSymbol = true
		}
	}
	classes := 0
	for _, b := range []bool{hasLower, hasUpper, hasDigit, hasSymbol} {
		if b {
			classes++
		}
	}
	if classes < 2 {
		return apierr.Validation("密码需至少包含两类字符（大小写字母 / 数字 / 符号）",
			[]apierr.Details{{Field: "password", Message: "强度不足"}})
	}
	return nil
}

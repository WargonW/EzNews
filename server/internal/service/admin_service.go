package service

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/eznews/eznews/internal/apierr"
	"github.com/eznews/eznews/internal/model"
	"github.com/eznews/eznews/internal/repo"
	"github.com/eznews/eznews/internal/store"
)

// AdminService 是管理后台的业务层。
//
// ★ 本文件的绝大部分复杂度集中在一件事上：**不让管理员把系统锁死**。
//
// EZNews 没有 SMTP、没有邮箱验证、没有找回密码入口 —— 一旦管理员被误操作
// 降级或停用，唯一的恢复手段是拿着服务器权限直接改 SQLite。
// 这不是"加个确认弹窗"就能解决的产品问题，而是必须在服务端强制兜住的**不可逆**操作。
// 所以下面每一处守卫都刻意写在 service 而不是 handler：
// handler 可以被绕过（多加一个路由就漏了），service 是唯一入口。
type AdminService struct {
	db     *store.DB
	users  *repo.UserRepo
	arts   *repo.ArticleRepo
	audio  *repo.AudioRepo
	audios *AudioService
	auth   *AuthService

	startedAt int64
	version   string
	lgr       *slog.Logger
}

// NewAdminService 创建 AdminService。
func NewAdminService(db *store.DB, users *repo.UserRepo, arts *repo.ArticleRepo,
	audio *repo.AudioRepo, audios *AudioService, auth *AuthService, version string) *AdminService {
	return &AdminService{
		db:        db,
		users:     users,
		arts:      arts,
		audio:     audio,
		audios:    audios,
		auth:      auth,
		startedAt: time.Now().Unix(),
		version:   version,
		lgr:       slog.Default(),
	}
}

// ---------------- 权限判定 ----------------

// AccountState 是一次性取回的权限相关事实。
type AccountState struct {
	IsAdmin  bool
	Disabled bool
	Exists   bool
}

// GetAccountState 判定某用户的管理权限。
//
// ★ 刻意每次请求都查库，而不是把 role 塞进 JWT：
//
//	管理员 A 把管理员 B 降级后，B 手里的 token 还剩最多 2 小时有效期。
//	如果权限来自 token 里的 claim，B 在这 2 小时内依然是管理员 ——
//	这恰恰是"被降级的人最可能搞破坏"的窗口期。
//	管理端点调用量极低（每分钟个位数），一次主键点查的代价完全可以接受。
func (s *AdminService) GetAccountState(ctx context.Context, userID int64) (AccountState, error) {
	role, disabled, err := s.users.AdminGetAccountState(ctx, userID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return AccountState{}, nil
		}
		return AccountState{}, apierr.Internal(err)
	}
	return AccountState{
		IsAdmin:  role == model.RoleAdmin,
		Disabled: disabled,
		Exists:   true,
	}, nil
}

// IsAdmin 判断是否为**可用**管理员（存在、是 admin、且未被停用）。
//
// 停用的管理员不算管理员：否则"停用"这个操作对管理员自己毫无意义
// （被停用的人还能打开后台把自已恢复） —— 那停用按钮就成了摆设。
func (s *AdminService) IsAdmin(ctx context.Context, userID int64) (bool, error) {
	st, err := s.GetAccountState(ctx, userID)
	if err != nil {
		return false, err
	}
	return st.IsAdmin && !st.Disabled, nil
}

// ---------------- 概览 ----------------

// AdminOverviewDTO 是后台首页的一屏数据。
type AdminOverviewDTO struct {
	Users           int64            `json:"users"`
	Admins          int64            `json:"admins"`
	DisabledUsers   int64            `json:"disabledUsers"`
	ActiveSessions  int64            `json:"activeSessions"`
	Articles        int64            `json:"articles"`
	Sources         int64            `json:"sources"`
	Audios          int64            `json:"audios"`
	FailedTasks     int64            `json:"failedAudioTasks"`
	PendingTasks    int64            `json:"pendingAudioTasks"`
	ArticlesLast24h int64            `json:"articlesLast24h"`
	TaskStatus      map[string]int64 `json:"audioTaskStatus"`
	SchemaVersion   int              `json:"schemaVersion"`
	UptimeSeconds   int64            `json:"uptimeSeconds"`
	ServerVersion   string           `json:"serverVersion"`
	GoVersion       string           `json:"goVersion"`
	GeneratedAt     int64            `json:"generatedAt"`
}

// Overview 返回概览数据。
//
// 内容 / 语音页各自有成体系的明细接口，这里刻意只放"够判断要不要点进去"的量：
// 概览页每次进入都加载，堆成一张全量报表会让首屏越来越慢。
func (s *AdminService) Overview(ctx context.Context) (*AdminOverviewDTO, error) {
	now := time.Now().Unix()
	counts, err := s.users.AdminCountsOnce(ctx, now)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	audioStats, err := s.audio.AdminAudioStats(ctx)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	content, err := s.arts.AdminContentStats(ctx, now)
	if err != nil {
		return nil, apierr.Internal(err)
	}

	out := &AdminOverviewDTO{
		Users:           counts.Users,
		Admins:          counts.Admins,
		DisabledUsers:   counts.DisabledUsers,
		ActiveSessions:  counts.ActiveSessions,
		Articles:        counts.Articles,
		Sources:         counts.Sources,
		Audios:          counts.Audios,
		FailedTasks:     audioStats.FailedTasks,
		PendingTasks:    audioStats.TasksByStatus[string(model.TaskStatusPending)],
		ArticlesLast24h: content.Last24h,
		TaskStatus:      audioStats.TasksByStatus,
		UptimeSeconds:   now - s.startedAt,
		ServerVersion:   s.version,
		GoVersion:       runtime.Version(),
		GeneratedAt:     now,
	}
	if out.TaskStatus == nil {
		out.TaskStatus = map[string]int64{}
	}
	if sys, err := repo.LoadSystemStats(ctx, s.db); err == nil {
		out.SchemaVersion = sys.SchemaVersion
	}
	return out, nil
}

// ---------------- 用户管理 ----------------

// AdminUserDTO 是管理接口返回的用户视图。
//
// ★ 不含任何凭据字段（password_hash）。见 repo.AdminUser 的说明。
type AdminUserDTO struct {
	ID            int64  `json:"id"`
	Username      string `json:"username"`
	Email         string `json:"email"`
	Role          string `json:"role"`
	Disabled      bool   `json:"disabled"`
	CreatedAt     int64  `json:"createdAt"`
	LastLoginAt   int64  `json:"lastLoginAt"`
	SessionCount  int    `json:"sessionCount"`
	FavoriteCount int    `json:"favoriteCount"`
	ReadCount     int    `json:"readCount"`

	// ---- 能力位 ----
	//
	// ★ 这些布尔值由**服务端**算出，而不是让前端拿 admins 计数自己去推。
	//   "最后一个管理员""不能改自己"这类规则一旦在两端各写一遍，必然漂移：
	//   前端算错只是按钮点不动，后端算错则是系统锁死。规则只有一个所有者。
	//   前端据此置灰按钮；后端仍然独立校验，绝不依赖这些位。
	Self          bool `json:"self"`
	CanChangeRole bool `json:"canChangeRole"`
	CanDisable    bool `json:"canDisable"`
	CanDelete     bool `json:"canDelete"`
	CanRevokeAll  bool `json:"canRevokeAll"`
}

// userCaps 是一组操作许可，供列表与详情共用同一套判定。
type userCaps struct {
	self, changeRole, disable, del, revokeAll bool
}

// computeCaps 计算当前管理员对某个目标用户可以做什么。
//
// admins 是**可用**管理员总数（未停用），它是最后管理员守卫的唯一输入。
func computeCaps(targetID int64, role string, disabled bool, actorID, admins int64) userCaps {
	self := targetID == actorID
	// 最后一个可用管理员 = 不能让他消失。
	//
	// ★ 判据是"目标本人是管理员且存量只剩他一个"，而不是单纯"admins<=1"：
	//   若目标只是普通用户，停用他根本不动管理员存量，不该被拦 ——
	//   写成后者会让管理员在只剩自己时连普通用户都管不了。
	lastAdmin := role == model.RoleAdmin && !disabled && admins <= 1
	return userCaps{
		self:       self,
		changeRole: !self && !lastAdmin,
		disable:    !self && !lastAdmin,
		del:        !self && !lastAdmin,
		revokeAll:  !self,
	}
}

// AdminUserWithCountsDTO 是列表响应（含详情才有的计数）。
type AdminUserListDTO struct {
	Items      []AdminUserDTO `json:"items"`
	NextCursor string         `json:"nextCursor"`
	HasMore    bool           `json:"hasMore"`
	Total      int64          `json:"total"`
	// Admins 是**未停用**的管理员总数。前端若要展示"还剩几个管理员"，
	// 用这个而不是自己去数列表里的角色 —— 列表可能正在筛选或被翻页截断。
	Admins int64 `json:"admins"`
}

func adminUserToDTO(u repo.AdminUser, c userCaps) AdminUserDTO {
	return AdminUserDTO{
		ID:            u.ID,
		Username:      u.Username,
		Email:         u.Email,
		Role:          u.Role,
		Disabled:      u.Disabled,
		CreatedAt:     u.CreatedAt,
		LastLoginAt:   u.LastLoginAt,
		SessionCount:  u.SessionCount,
		FavoriteCount: u.FavoriteCount,
		ReadCount:     u.ReadCount,
		Self:          c.self,
		CanChangeRole: c.changeRole,
		CanDisable:    c.disable,
		CanDelete:     c.del,
		CanRevokeAll:  c.revokeAll,
	}
}

// maxAdminPageSize 是管理后台用户列表的页大小上限。
//
// 管理接口避开了常规限流器也无法阻止全表扫描：一次 limit=100000 会把整个
// 用户表读进内存再序列化。上限设在此处而不是依赖前端传参自觉。
const maxAdminPageSize = 100

// ListUsers 分页列出用户。
func (s *AdminService) ListUsers(ctx context.Context, actorID int64, q string, role string,
	limit int, cursor int64) (*AdminUserListDTO, error) {
	role = strings.TrimSpace(role)
	if role != "" && role != model.RoleUser && role != model.RoleAdmin {
		return nil, apierr.Validation("role 取值非法", map[string]string{
			"role": "只能是 user 或 admin",
		})
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > maxAdminPageSize {
		limit = maxAdminPageSize
	}
	f := repo.AdminUserFilter{Query: strings.TrimSpace(q), Role: role, Limit: limit + 1, Cursor: cursor}

	list, err := s.users.AdminListUsers(ctx, f)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	total, err := s.users.AdminCountUsers(ctx, repo.AdminUserFilter{Query: f.Query, Role: f.Role})
	if err != nil {
		return nil, apierr.Internal(err)
	}
	// 可用管理员数只取一次：它对本页所有行都一样，放进循环就是 N 次无谓查询。
	admins, err := s.users.AdminCountAdmins(ctx)
	if err != nil {
		return nil, apierr.Internal(err)
	}

	// 多取一条来判断是否还有下一页，比再发一次 COUNT(*) 便宜。
	hasMore := len(list) > limit
	if hasMore {
		list = list[:limit]
	}
	out := &AdminUserListDTO{
		Items:   make([]AdminUserDTO, 0, len(list)),
		HasMore: hasMore,
		Total:   total,
		Admins:  admins,
	}
	for _, u := range list {
		out.Items = append(out.Items,
			adminUserToDTO(u, computeCaps(u.ID, u.Role, u.Disabled, actorID, admins)))
	}
	if hasMore && len(list) > 0 {
		out.NextCursor = strconv.FormatInt(list[len(list)-1].ID, 10)
	}
	return out, nil
}

// GetUser 查询单个用户详情。
func (s *AdminService) GetUser(ctx context.Context, actorID, id int64) (*AdminUserDTO, error) {
	u, err := s.users.AdminGetUser(ctx, id, time.Now().Unix())
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("用户不存在")
		}
		return nil, apierr.Internal(err)
	}
	admins, err := s.users.AdminCountAdmins(ctx)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	dto := adminUserToDTO(*u, computeCaps(u.ID, u.Role, u.Disabled, actorID, admins))
	return &dto, nil
}

// SetRole 修改角色。
//
// 守卫（顺序重要，每条都对应一个真实事故场景）：
//  1. 目标必须存在            —— 否则 UPDATE 影响 0 行还返回成功。
//  2. 不许改自己              —— 手滑把自己降权 = 把自己锁在门外。
//  3. 降级前必须有第二个可用管理员 —— 见本文件开头的说明，这是不可逆操作。
func (s *AdminService) SetRole(ctx context.Context, actorID, targetID int64, role string) (*AdminUserDTO, error) {
	if role != model.RoleUser && role != model.RoleAdmin {
		return nil, apierr.Validation("role 取值非法", map[string]string{
			"role": "只能是 user 或 admin",
		})
	}
	if actorID == targetID {
		return nil, apierr.Conflict("不能修改自己的角色")
	}

	tx, derr := s.db.BeginWrite(ctx)
	if derr != nil {
		return nil, apierr.DBBusy()
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	target, err := s.users.AdminGetUser(ctx, targetID, time.Now().Unix())
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("用户不存在")
		}
		return nil, apierr.Internal(err)
	}
	if target.Role == role {
		// 幂等：重复提交同样的角色不该报错（前端二次确认弹窗可能重发）。
		dto := adminUserToDTO(*target, s.caps(ctx, *target, actorID))
		committed = true
		if cerr := tx.Commit(); cerr != nil {
			return nil, apierr.DBBusy()
		}
		return &dto, nil
	}
	// 降级前的最后一道闸门：必须还有别的可用管理员。
	// （目标是不是管理员由守卫自己判断，别在外面再加一层条件）
	if err := s.guardNotLastAdmin(ctx, targetID); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	if err := s.users.AdminSetRole(ctx, tx, targetID, role, now); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("用户不存在")
		}
		return nil, apierr.Internal(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, apierr.DBBusy()
	}
	committed = true

	s.lgr.Info("管理员修改了用户角色",
		"actorId", actorID, "targetId", targetID, "from", target.Role, "to", role)

	updated, err := s.users.AdminGetUser(ctx, targetID, now)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	dto := adminUserToDTO(*updated, s.caps(ctx, *updated, actorID))
	return &dto, nil
}

// caps 算一次目标用户的能力位。
//
// 出错时**保守返回全 false**：能力位只用于前端置灰按钮，
// 给不了确定性答案时宁可让按钮变灰（用户还能刷新），也别给出"可以操作"的错判。
func (s *AdminService) caps(ctx context.Context, u repo.AdminUser, actorID int64) userCaps {
	admins, err := s.users.AdminCountAdmins(ctx)
	if err != nil {
		s.lgr.Warn("统计管理员数量失败，能力位按不可用处理", slog.String("err", err.Error()))
		return userCaps{}
	}
	return computeCaps(u.ID, u.Role, u.Disabled, actorID, admins)
}

// SetDisabled 停用 / 启用账户。
//
// 停用必须**持久化**：只吊销会话或 bump token_version 都没用 ——
// 用户拿正确密码再登一次就进来了。所以这里写 user.disabled_at 列，
// 由登录与鉴权两条路径共同拦截（见 AuthService 与本中间件）。
func (s *AdminService) SetDisabled(ctx context.Context, actorID, targetID int64, disabled bool) (*AdminUserDTO, error) {
	if actorID == targetID {
		return nil, apierr.Conflict("不能停用自己的账户")
	}

	tx, derr := s.db.BeginWrite(ctx)
	if derr != nil {
		return nil, apierr.DBBusy()
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	target, err := s.users.AdminGetUser(ctx, targetID, time.Now().Unix())
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("用户不存在")
		}
		return nil, apierr.Internal(err)
	}
	if target.Disabled == disabled {
		dto := adminUserToDTO(*target, s.caps(ctx, *target, actorID))
		committed = true
		if cerr := tx.Commit(); cerr != nil {
			return nil, apierr.DBBusy()
		}
		return &dto, nil
	}
	if disabled {
		if err := s.guardNotLastAdmin(ctx, targetID); err != nil {
			return nil, err
		}
	}
	now := time.Now().Unix()
	if err := s.users.AdminSetDisabled(ctx, tx, targetID, disabled, now); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("用户不存在")
		}
		return nil, apierr.Internal(err)
	}
	// 停用必须连带吊销会话，否则"已停用"的用户在当前 2 小时 token 有效期内
	// 仍然畅通无阻 —— 停用就成了一张空头支票。
	// 启用则刻意不动会话：那只会把正常用户踢下线，毫无收益。
	if disabled {
		if _, err := s.users.AdminRevokeAllSessions(ctx, tx, targetID, now); err != nil {
			return nil, apierr.Internal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, apierr.DBBusy()
	}
	committed = true

	s.lgr.Warn("管理员修改了账户停用状态",
		"actorId", actorID, "targetId", targetID, "disabled", disabled)

	updated, err := s.users.AdminGetUser(ctx, targetID, now)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	dto := adminUserToDTO(*updated, s.caps(ctx, *updated, actorID))
	return &dto, nil
}

// DeleteUser 删除账号及其个人数据。
func (s *AdminService) DeleteUser(ctx context.Context, actorID, targetID int64) error {
	if actorID == targetID {
		return apierr.Conflict("不能删除自己的账户")
	}

	tx, derr := s.db.BeginWrite(ctx)
	if derr != nil {
		return apierr.DBBusy()
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// 存在性检查 + 最后管理员守卫。守卫内部会自己判断目标是否管理员。
	if _, _, err := s.users.AdminGetAccountState(ctx, targetID); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return apierr.NotFound("用户不存在")
		}
		return apierr.Internal(err)
	}
	if err := s.guardNotLastAdmin(ctx, targetID); err != nil {
		return err
	}
	if err := s.users.AdminDeleteUser(ctx, tx, targetID); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return apierr.NotFound("用户不存在")
		}
		return apierr.Internal(err)
	}
	if err := tx.Commit(); err != nil {
		return apierr.DBBusy()
	}
	committed = true

	s.lgr.Warn("管理员删除了账户", "actorId", actorID, "targetId", targetID)
	return nil
}

// RevokeAllSessions 吊销目标用户的全部会话（强制下线所有设备）。
//
// 刻意允许管理员踢自己以外的所有人；踢自己走单人用的 /me/sessions/{id}。
// 不 bump token_version：会话级吊销已经让 sid 作废，
// 连带 bump 会顺带废掉其它无关设备的 refresh token，属于不必要的连带伤害。
func (s *AdminService) RevokeAllSessions(ctx context.Context, actorID, targetID int64) (int64, error) {
	if actorID == targetID {
		return 0, apierr.Conflict("不能吊销自己的全部会话，请使用单个会话下线接口")
	}
	if _, _, err := s.users.AdminGetAccountState(ctx, targetID); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return 0, apierr.NotFound("用户不存在")
		}
		return 0, apierr.Internal(err)
	}
	tx, derr := s.db.BeginWrite(ctx)
	if derr != nil {
		return 0, apierr.DBBusy()
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	n, err := s.users.AdminRevokeAllSessions(ctx, tx, targetID, time.Now().Unix())
	if err != nil {
		return 0, apierr.Internal(err)
	}
	if err := tx.Commit(); err != nil {
		return 0, apierr.DBBusy()
	}
	committed = true
	return n, nil
}

// guardNotLastAdmin 保证对 targetID 执行降权/停用/删除后，系统仍有可用管理员。
//
// ★ 这是整个管理后台唯一一处"错了就不可逆"的判据。
//
//	写错的表现是：操作成功、退出登录后再也进不去后台，且没有任何报错。
//
// ★★ 它必须**自己判断目标是不是管理员**，不能把这件事丢给调用方。
//
//	第一版就是这样写的（调用方负责 `if target.Role == admin`），
//	结果三个调用点里有两处漏了外层判断 —— 实际表现是：
//	**系统里只剩一个管理员时，连一个普通用户都停用不了**，
//	管理员看着"最后一个管理员"的报错一头雾水，因为被操作的人根本不是管理员。
//	那条 Sergey 路径我自己在端到端验证时才撞出来。
//	规则只有一个所有者：把判断收进来，调用方就不可能忘。
func (s *AdminService) guardNotLastAdmin(ctx context.Context, targetID int64) error {
	role, disabled, err := s.users.AdminGetAccountState(ctx, targetID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return apierr.NotFound("用户不存在")
		}
		return apierr.Internal(err)
	}
	// 目标不是未停用的管理员 → 动他根本不影响管理员存量，放行。
	if role != model.RoleAdmin || disabled {
		return nil
	}
	total, err := s.users.AdminCountAdmins(ctx)
	if err != nil {
		return apierr.Internal(err)
	}
	if total <= 1 {
		return apierr.Conflict("这是系统中最后一个可用管理员，不能降级、停用或删除")
	}
	return nil
}

// CreateUserInput 是管理员建号的入参。
type CreateUserInput struct {
	Username string
	Password string
	Email    string
	Role     string
}

// CreateUser 由管理员代建账号。
//
// 刻意委托给 AuthService.CreateUserByAdmin 而不是自己insert：
// Argon2id 参数、用户名/密码复杂度规则、唯一性检查都在那边，
// 在这里重抄一遍就等于给"密码策略不一致"开了个后门 ——
// 后台建的号绕过了复杂度要求，这在渗透测试里是要被打的点。
func (s *AdminService) CreateUser(ctx context.Context, actorID int64, in CreateUserInput) (*AdminUserDTO, error) {
	if s.auth == nil {
		return nil, apierr.Internal(errors.New("账号服务未启用"))
	}
	u, err := s.auth.CreateUserByAdmin(ctx, actorID, in.Username, in.Password, in.Email, in.Role)
	if err != nil {
		return nil, err
	}
	dto := AdminUserDTO{
		ID: u.ID, Username: u.Username, Email: u.Email, Role: u.Role,
		CreatedAt: u.CreatedAt, LastLoginAt: u.LastLoginAt,
	}
	// 新建的账号一律可管理（不是自己、也不是最后一个管理员 —— 刚创建 admin 时
	// 总数至少为 1，且自己已经是管理员了）。
	dto.Self = false
	dto.CanChangeRole, dto.CanDisable, dto.CanDelete, dto.CanRevokeAll = true, true, true, true
	return &dto, nil
}

// ---------------- 内容与语音 ----------------

// AdminContentDTO 是 /admin/stats/content 的响应。
type AdminContentDTO struct {
	// Total 是 TopSources 里各源文章数之和（= 全库文章数）。
	Total          int64                  `json:"total"`
	Last24h        int64                  `json:"last24h"`
	Last7d         int64                  `json:"last7d"`
	DisabledSrcs   int64                  `json:"disabledSources"`
	OrphanArticles int64                  `json:"orphanArticles"`
	ByCategory     []AdminCategoryStatDTO `json:"byCategory"`
	TopSources     []AdminSourceStatDTO   `json:"topSources"`
}

// AdminCategoryStatDTO 是单个分类的文章数。
type AdminCategoryStatDTO struct {
	Category string `json:"category"`
	Count    int64  `json:"count"`
}

// AdminSourceStatDTO 是单个源的内容贡献。
type AdminSourceStatDTO struct {
	SourceID     int64  `json:"sourceId"`
	SourceName   string `json:"sourceName"`
	Enabled      bool   `json:"enabled"`
	ArticleCount int64  `json:"articleCount"`
	LastSeenAt   int64  `json:"lastSeenAt"`
}

// ContentStats 返回内容侧统计。
func (s *AdminService) ContentStats(ctx context.Context) (*AdminContentDTO, error) {
	now := time.Now().Unix()
	st, err := s.arts.AdminContentStats(ctx, now)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	out := &AdminContentDTO{
		Last24h:        st.Last24h,
		Last7d:         st.Last7d,
		DisabledSrcs:   st.DisabledSrc,
		OrphanArticles: st.OrphanArticles,
		ByCategory:     make([]AdminCategoryStatDTO, 0, len(st.ByCategory)),
		TopSources:     make([]AdminSourceStatDTO, 0, len(st.TopSources)),
	}
	for _, s2 := range st.TopSources {
		out.Total += s2.ArticleCount
		out.TopSources = append(out.TopSources, AdminSourceStatDTO{
			SourceID: s2.SourceID, SourceName: s2.SourceName, Enabled: s2.Enabled,
			ArticleCount: s2.ArticleCount, LastSeenAt: s2.LastSeenAt,
		})
	}
	for cat, n := range st.ByCategory {
		out.ByCategory = append(out.ByCategory, AdminCategoryStatDTO{Category: cat, Count: n})
	}
	return out, nil
}

// AdminAudioTaskDTO 是管理后台看到的一条合成任务。
type AdminAudioTaskDTO struct {
	ID           int64   `json:"id"`
	AudioID      int64   `json:"audioId"`
	ArticleID    int64   `json:"articleId"`
	ArticleTitle string  `json:"articleTitle"`
	Voice        string  `json:"voice"`
	Speed        float64 `json:"speed"`
	Status       string  `json:"status"`
	Provider     string  `json:"provider"`
	ErrorCode    string  `json:"errorCode"`
	ErrorMsg     string  `json:"errorMsg"`
	RetryCount   int     `json:"retryCount"`
	TextChars    int     `json:"textChars"`
	CreatedAt    int64   `json:"createdAt"`
	UpdatedAt    int64   `json:"updatedAt"`
}

// AdminAudioTaskListDTO 是任务列表响应。
type AdminAudioTaskListDTO struct {
	Items      []AdminAudioTaskDTO `json:"items"`
	Status     map[string]int64    `json:"statusCounts"`
	Audios     int64               `json:"audioCount"`
	AudioBytes int64               `json:"audioBytes"`
	Total      int64               `json:"total"`
	HasMore    bool                `json:"hasMore"`
	NextCursor string              `json:"nextCursor"`
}

// validAudioStatus 是允许出现在 status 过滤参数里的取值。
//
// ★ 刻意做成白名单而不是直接拼进 SQL：status 直接来自 query string，
//
//	虽然驱动已经参数化（不会注入），但传个乱值会得到一个空列表 + 200，
//	前端以为是"没有任务"，实际上是"参数拼错了"。
var validAudioStatus = map[string]bool{
	string(model.TaskStatusPending):    true,
	string(model.TaskStatusProcessing): true,
	string(model.TaskStatusReady):      true,
	string(model.TaskStatusFailed):     true,
}

// AudioTasks 列出语音合成任务。
func (s *AdminService) AudioTasks(ctx context.Context, status string, limit, cursor int64) (*AdminAudioTaskListDTO, error) {
	status = strings.TrimSpace(status)
	if status != "" && !validAudioStatus[status] {
		return nil, apierr.Validation("status 取值非法", map[string]string{
			"status": "只能是 pending / processing / ready / failed",
		})
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	// 多取一条判断下一页，省一次 COUNT(*)。
	tasks, err := s.audio.AdminListAudioTasks(ctx, status, limit+1, cursor)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	total, err := s.audio.AdminCountAudioTasks(ctx, status)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	stats, err := s.audio.AdminAudioStats(ctx)
	if err != nil {
		return nil, apierr.Internal(err)
	}

	hasMore := int64(len(tasks)) > limit
	if hasMore {
		tasks = tasks[:limit]
	}
	out := &AdminAudioTaskListDTO{
		Items:      make([]AdminAudioTaskDTO, 0, len(tasks)),
		Status:     stats.TasksByStatus,
		Audios:     stats.AudioCount,
		AudioBytes: stats.AudioBytes,
		Total:      total,
		HasMore:    hasMore,
	}
	for _, t := range tasks {
		out.Items = append(out.Items, AdminAudioTaskDTO{
			ID: t.ID, AudioID: t.AudioID, ArticleID: t.ArticleID, ArticleTitle: t.ArticleTitle,
			Voice: t.Voice, Speed: t.Speed, Status: t.Status, Provider: t.Provider,
			ErrorCode: t.ErrorCode, ErrorMsg: t.ErrorMsg, RetryCount: t.RetryCount,
			TextChars: t.TextChars, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		})
	}
	if hasMore && len(tasks) > 0 {
		out.NextCursor = strconv.FormatInt(tasks[len(tasks)-1].ID, 10)
	}
	if out.Status == nil {
		out.Status = map[string]int64{}
	}
	return out, nil
}

// RetryAudioTask 重试一条失败的合成任务。
//
// 复用 AudioService.RetryTask 而不是另写一套：重试涉及"重置状态 + 重新入队"，
// 这两件事分家就会出现"状态是 pending 但永远没人处理"的僵尸任务。
func (s *AdminService) RetryAudioTask(ctx context.Context, actorID, taskID int64) (*AdminAudioTaskDTO, error) {
	if s.audios == nil {
		return nil, apierr.Internal(errors.New("语音服务未启用"))
	}
	task, err := s.audios.RetryTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	s.lgr.Info("管理员重试了语音任务", "actorId", actorID, "taskId", taskID)
	return &AdminAudioTaskDTO{
		ID: task.ID, AudioID: task.AudioID, ArticleID: task.ArticleID,
		Voice: task.Voice, Speed: task.Speed,
		Status: string(task.Status), Provider: task.Provider, ErrorCode: task.ErrorCode,
		ErrorMsg: task.ErrorMsg, RetryCount: task.RetryCount, TextChars: task.TextChars,
		CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
	}, nil
}

// ---------------- 系统 ----------------

// AdminSystemDTO 是系统信息。
type AdminSystemDTO struct {
	ServerVersion string `json:"serverVersion"`
	GoVersion     string `json:"goVersion"`
	GoOS          string `json:"goos"`
	GoArch        string `json:"goarch"`
	NumGoroutine  int    `json:"numGoroutine"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
	StartedAt     int64  `json:"startedAt"`
	SchemaVersion int    `json:"schemaVersion"`
	JournalMode   string `json:"journalMode"`
	DBBytes       int64  `json:"dbBytes"`
	PageSize      int64  `json:"pageSize"`
	PageCount     int64  `json:"pageCount"`
	FreePages     int64  `json:"freePages"`
	// FreeRatio 是空闲页占比（0–1）：给"要不要 VACUUM"一个直观依据。
	// 超过 0.3 基本意味着刚删过大量数据，值得跑一次 VACUUM 把文件缩回去。
	FreeRatio   float64          `json:"freeRatio"`
	TableCounts map[string]int64 `json:"tableCounts"`
	HeapAllocMB float64          `json:"heapAllocMb"`
}

// SystemInfo 返回运行环境与数据库的客观状态。
func (s *AdminService) SystemInfo(ctx context.Context) (*AdminSystemDTO, error) {
	sys, err := repo.LoadSystemStats(ctx, s.db)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	now := time.Now().Unix()

	out := &AdminSystemDTO{
		ServerVersion: s.version,
		GoVersion:     runtime.Version(),
		GoOS:          runtime.GOOS,
		GoArch:        runtime.GOARCH,
		NumGoroutine:  runtime.NumGoroutine(),
		UptimeSeconds: now - s.startedAt,
		StartedAt:     s.startedAt,
		SchemaVersion: sys.SchemaVersion,
		JournalMode:   sys.JournalMode,
		DBBytes:       sys.DBBytes,
		PageSize:      sys.PageSize,
		PageCount:     sys.PageCount,
		FreePages:     sys.FreePages,
		TableCounts:   sys.TableCounts,
		HeapAllocMB:   float64(ms.HeapAlloc) / (1024 * 1024),
	}
	if sys.PageCount > 0 {
		out.FreeRatio = float64(sys.FreePages) / float64(sys.PageCount)
	}
	if out.TableCounts == nil {
		out.TableCounts = map[string]int64{}
	}
	return out, nil
}

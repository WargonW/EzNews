package model

// User 是账号实体（对应表 user）。
type User struct {
	ID           int64
	Username     string
	PasswordHash string // Argon2id PHC 字符串
	Email        string
	Role         string // user | admin（admin 仅预留）
	TokenVersion int32  // +1 使全部已签发 Access Token 失效
	CreatedAt    int64
	UpdatedAt    int64
	LastLoginAt  int64 // 0 表示从未登录

	// DisabledAt 是停用时刻；0 表示未停用（对应列 disabled_at 为 NULL）。
	//
	// ★ 停用是持久状态，不同于 token_version：后者只把当前所有会话踢下线，
	//   用户拿正确密码再登一次照样能进来。停用之后必须**重新登录也被拒绝**，
	//   所以它需要自己的一列。
	DisabledAt int64
}

// Disabled 报告账户是否处于停用状态。
func (u *User) Disabled() bool { return u != nil && u.DisabledAt > 0 }

// IsAdmin 报告账户是否具备管理员角色。
func (u *User) IsAdmin() bool { return u != nil && u.Role == RoleAdmin }

// 角色取值。user 表自建表起就有 role 列（DEFAULT 'user'），
// 但此前从未有任何代码写入或读取它 —— 这两个常量就是它的第一处使用者。
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// Session 是刷新令牌会话（对应表 session）。
//
// ★ 行身份不变量：一次逻辑会话 = 一行。轮换必须原地 UPDATE（RotateSession），
//
//	否则 Prev* 两列无处落（DEC-7 宽限失效）且 JWT 里的 sid 每次刷新都变（DEC-14 登出失效）。
type Session struct {
	ID       int64
	UserID   int64
	ClientID string
	// RefreshTokenHash 是当前代 hash，hex(sha256(token))，绝不明文。
	RefreshTokenHash string
	// PrevRefreshTokenHash 是上一代 hash；PrevRotatedAt 是其被轮换的时刻。
	// DEC-7 宽限判定：重放旧 token 时按 (prev_refresh_token_hash, prev_rotated_at)
	// 判断是否落在 60s + 同 client_id 的宽限窗口内。
	PrevRefreshTokenHash string
	PrevRotatedAt        int64
	DeviceName           string
	LastSeenAt           int64
	ExpiresAt            int64
	RevokedAt            int64 // 0 表示未吊销
	CreatedAt            int64
}

// Favorite 是收藏（含墓碑，对应表 user_favorite）。
type Favorite struct {
	ID        int64
	UserID    int64
	ArticleID int64
	DeletedAt int64 // 0 表示有效；非 0 = 墓碑
	CreatedAt int64
	UpdatedAt int64
}

// Read 是已读标记（含墓碑，对应表 user_read）。
type Read struct {
	ID        int64
	UserID    int64
	ArticleID int64
	DeletedAt int64
	CreatedAt int64
	UpdatedAt int64
}

// Preference 是偏好 KV（对应表 user_preference）。
type Preference struct {
	UserID    int64
	Key       string
	Value     string
	UpdatedAt int64
}

// MergeLog 是合并幂等日志（对应表 merge_log）。
type MergeLog struct {
	ID         int64
	UserID     int64
	ClientID   string
	Nonce      string
	ResultJSON string
	CreatedAt  int64
}

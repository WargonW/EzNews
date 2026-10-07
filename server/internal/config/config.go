// Package config 定义 EZNews 服务端的配置结构、默认值与校验规则（fail-fast）。
//
// 配置优先级：内置默认值 < YAML 文件 < 环境变量（EZNEWS_*）。
// YAML 中的 ${VAR} 占位符会在加载时展开为同名环境变量的值。
package config

import (
	"fmt"
	"strings"
	"time"
)

// Duration 是支持 YAML 字符串（如 "15s" "2h" "720h"）与环境变量字符串解析的时间长度类型。
type Duration time.Duration

// UnmarshalYAML 实现 yaml.Unmarshaler，允许配置里写 "15s" 这样的可读形式。
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var raw string
	if err := unmarshal(&raw); err != nil {
		return err
	}
	return d.Set(raw)
}

// MarshalYAML 实现 yaml.Marshaler，回写为可读字符串。
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// Set 从字符串解析时长，空串表示保持零值。
func (d *Duration) Set(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("非法时长 %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// String 返回时长的可读形式。
func (d Duration) String() string { return time.Duration(d).String() }

// Std 转换为标准库 time.Duration。
func (d Duration) Std() time.Duration { return time.Duration(d) }

// APIKey 表示一个采集器密钥：ID 用于日志脱敏标识，Key 为实际密钥。
type APIKey struct {
	ID  string `yaml:"id"`
	Key string `yaml:"key"`
}

// ServerConf 是 HTTP 服务器相关配置。
type ServerConf struct {
	Addr            string   `yaml:"addr"`
	ReadTimeout     Duration `yaml:"readTimeout"`
	WriteTimeout    Duration `yaml:"writeTimeout"`
	ShutdownTimeout Duration `yaml:"shutdownTimeout"`
}

// DBConf 是 SQLite 相关配置。
type DBConf struct {
	Path                  string `yaml:"path"`
	BusyTimeoutMs         int    `yaml:"busyTimeoutMs"`
	CacheSizeKB           int    `yaml:"cacheSizeKB"`
	CheckpointIntervalMin int    `yaml:"checkpointIntervalMin"`
}

// Argon2Conf 是 Argon2id 哈希参数（写入 PHC 字符串，存量密码不受后续调参影响）。
type Argon2Conf struct {
	Memory  uint32 `yaml:"memory"`  // KiB
	Time    uint32 `yaml:"time"`    // 迭代次数
	Threads uint8  `yaml:"threads"` // 并行度
}

// AuthConf 是鉴权相关配置（采集器 API Key + 账号体系）。
type AuthConf struct {
	APIKeys             []APIKey   `yaml:"apiKeys"`
	RequireKeyForRead   bool       `yaml:"requireKeyForRead"`
	JWTSecret           string     `yaml:"jwtSecret"`
	AccessTokenTTL      Duration   `yaml:"accessTokenTTL"`
	RefreshTokenTTL     Duration   `yaml:"refreshTokenTTL"`
	RegistrationEnabled bool       `yaml:"registrationEnabled"`
	Argon2              Argon2Conf `yaml:"argon2"`
	// Argon2MaxConcurrency 限制并发 Argon2id 计算数量（默认 2），防止 19MiB × N 内存叠加。
	Argon2MaxConcurrency int `yaml:"argon2MaxConcurrency"`
	// Argon2QueueTimeoutMs 是等待 Argon2id 并发槽的最长等待毫秒数，超时返回 503 SERVICE_BUSY。
	// 缺省会导致第 N+1 个并发登录无限期阻塞在信号量上，与「快速失败」意图相反。
	Argon2QueueTimeoutMs int      `yaml:"argon2QueueTimeoutMs"`
	LoginLimitPerUser    string   `yaml:"loginLimitPerUser"`
	LoginLimitPerIP      string   `yaml:"loginLimitPerIP"`
	LockoutDuration      Duration `yaml:"lockoutDuration"`
	// RefreshReuseGraceSec 是 refresh token 重放的宽限窗口秒数（DEC-7）。
	// 窗口内且同 client_id 视为弱网重试，幂等再轮换一次；超窗或换设备则判定为泄露。
	RefreshReuseGraceSec int    `yaml:"refreshReuseGraceSec"`
	JWTSecretPath        string `yaml:"jwtSecretPath"`
	// RefreshCookie 是否在登录/刷新响应中写入 httpOnly Cookie（同源部署推荐）。
	RefreshCookie bool `yaml:"refreshCookie"`
	// RefreshTokenInBody 是否在响应体中返回 refreshToken（跨域部署需要）。
	RefreshTokenInBody bool   `yaml:"refreshTokenInBody"`
	RefreshCookiePath  string `yaml:"refreshCookiePath"`
	// RefreshReplayGuardMax 是同一 prev hash 在宽限窗口内允许的最大重放次数，超出返回 429。
	RefreshReplayGuardMax int `yaml:"refreshReplayGuardMax"`
}

// IngestConf 是 ingest 接收侧的配置。
type IngestConf struct {
	MaxBatchItems       int      `yaml:"maxBatchItems"`
	MaxBodyBytes        int64    `yaml:"maxBodyBytes"`
	StoreContent        bool     `yaml:"storeContent"`
	AutoCreateSource    bool     `yaml:"autoCreateSource"`
	DefaultTimezone     string   `yaml:"defaultTimezone"`
	TrackParamBlacklist []string `yaml:"trackParamBlacklist"`
}

// RateLimitConf 是进程内令牌桶参数。
type RateLimitConf struct {
	GlobalRps   float64 `yaml:"globalRps"`
	GlobalBurst int     `yaml:"globalBurst"`
	PerKeyRps   float64 `yaml:"perKeyRps"`
	PerKeyBurst int     `yaml:"perKeyBurst"`
}

// TTSCredentials 是云 TTS 凭据，仅存在于服务端进程内存。
type TTSCredentials struct {
	SecretID  string `yaml:"secretId"`
	SecretKey string `yaml:"secretKey"`
	AppID     string `yaml:"appId"`
}

// TTSConf 是语音合成相关配置。
type TTSConf struct {
	Provider         string         `yaml:"provider"`
	FallbackProvider string         `yaml:"fallbackProvider"`
	Credentials      TTSCredentials `yaml:"credentials"`
	DefaultVoice     string         `yaml:"defaultVoice"`
	DefaultSpeed     float64        `yaml:"defaultSpeed"`
	Format           string         `yaml:"format"`
	SampleRate       int            `yaml:"sampleRate"`
	MaxChars         int            `yaml:"maxChars"`
	Concurrency      int            `yaml:"concurrency"`
	QueueSize        int            `yaml:"queueSize"`
	MaxRetries       int            `yaml:"maxRetries"`
	Timeout          Duration       `yaml:"timeout"`
}

// AudioConf 是音频落盘与清理相关配置。
type AudioConf struct {
	Dir                    string `yaml:"dir"`
	MaxTotalBytes          int64  `yaml:"maxTotalBytes"`
	MaxAgeDays             int    `yaml:"maxAgeDays"`
	AccessFlushIntervalSec int    `yaml:"accessFlushIntervalSec"`
}

// RetentionConf 是文章归档保留策略。
type RetentionConf struct {
	Enabled bool `yaml:"enabled"`
	Days    int  `yaml:"days"`
}

// JanitorConf 是后台清理任务配置。
type JanitorConf struct {
	IntervalMin int `yaml:"intervalMin"`
}

// SearchConf 是搜索相关配置。
type SearchConf struct {
	EnableFTS bool `yaml:"enableFts"`
}

// LogConf 是日志配置。
type LogConf struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Config 是完整的服务端配置树。
type Config struct {
	Server    ServerConf    `yaml:"server"`
	DB        DBConf        `yaml:"db"`
	Auth      AuthConf      `yaml:"auth"`
	Ingest    IngestConf    `yaml:"ingest"`
	RateLimit RateLimitConf `yaml:"ratelimit"`
	TTS       TTSConf       `yaml:"tts"`
	Audio     AudioConf     `yaml:"audio"`
	Retention RetentionConf `yaml:"retention"`
	Janitor   JanitorConf   `yaml:"janitor"`
	Search    SearchConf    `yaml:"search"`
	Log       LogConf       `yaml:"log"`
}

// Defaults 返回内置默认配置（对齐 ARCHITECTURE.md §13.1）。
func Defaults() *Config {
	return &Config{
		Server: ServerConf{
			Addr:            ":8080",
			ReadTimeout:     Duration(15 * time.Second),
			WriteTimeout:    Duration(60 * time.Second),
			ShutdownTimeout: Duration(30 * time.Second),
		},
		DB: DBConf{
			Path:                  "./data/eznews.db",
			BusyTimeoutMs:         5000,
			CacheSizeKB:           16000,
			CheckpointIntervalMin: 5,
		},
		Auth: AuthConf{
			APIKeys:             nil,
			RequireKeyForRead:   false,
			AccessTokenTTL:      Duration(2 * time.Hour),
			RefreshTokenTTL:     Duration(720 * time.Hour),
			RegistrationEnabled: true,
			Argon2: Argon2Conf{
				Memory:  19456, // 19 MiB（单位 KiB）
				Time:    2,
				Threads: 1,
			},
			Argon2MaxConcurrency:  2,
			Argon2QueueTimeoutMs:  3000,
			LoginLimitPerUser:     "5/m",
			LoginLimitPerIP:       "20/m",
			LockoutDuration:       Duration(15 * time.Minute),
			RefreshReuseGraceSec:  60,
			RefreshReplayGuardMax: 10,
			JWTSecretPath:         "./data/jwt_secret",
			// 默认双 true：Android 原生端不走 Cookie 通道，只开 Cookie 会让安卓端集体失效。
			RefreshCookie:      true,
			RefreshTokenInBody: true,
			// 必须是 /api/v1/auth（宽路径），窄化到 /api/v1/auth/refresh 会导致
			// 浏览器调 logout 时不带 Cookie、服务端无法销毁 session，登出形同虚设。
			RefreshCookiePath: "/api/v1/auth",
		},
		Ingest: IngestConf{
			MaxBatchItems:       200,
			MaxBodyBytes:        4 * 1024 * 1024,
			StoreContent:        false,
			AutoCreateSource:    false,
			DefaultTimezone:     "UTC",
			TrackParamBlacklist: []string{"utm_*", "spm", "from", "ref", "share_*", "hmsr"},
		},
		RateLimit: RateLimitConf{
			GlobalRps:   100,
			GlobalBurst: 150,
			PerKeyRps:   50,
			PerKeyBurst: 100,
		},
		TTS: TTSConf{
			Provider:     "tencent",
			DefaultVoice: "101001",
			DefaultSpeed: 1.0,
			Format:       "mp3",
			SampleRate:   16000,
			MaxChars:     800,
			Concurrency:  3,
			QueueSize:    512,
			MaxRetries:   3,
			Timeout:      Duration(30 * time.Second),
		},
		Audio: AudioConf{
			Dir:                    "./data/audio",
			MaxTotalBytes:          2 * 1024 * 1024 * 1024,
			MaxAgeDays:             30,
			AccessFlushIntervalSec: 60,
		},
		Retention: RetentionConf{Enabled: true, Days: 90},
		Janitor:   JanitorConf{IntervalMin: 60},
		Search:    SearchConf{EnableFTS: true},
		Log:       LogConf{Level: "info", Format: "json"},
	}
}

// Validate 执行启动期 fail-fast 校验。返回的错误应直接导致进程退出。
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Server.Addr) == "" {
		return fmt.Errorf("配置校验失败: server.addr 不能为空")
	}
	if strings.TrimSpace(c.DB.Path) == "" {
		return fmt.Errorf("配置校验失败: db.path 不能为空")
	}
	if strings.TrimSpace(c.Audio.Dir) == "" {
		return fmt.Errorf("配置校验失败: audio.dir 不能为空")
	}
	// 架构 §3.2：未配置任何 API Key 时拒绝启动，避免"裸奔上线"。
	if len(c.Auth.APIKeys) == 0 {
		return fmt.Errorf("配置校验失败: auth.apiKeys 为空，请至少配置一个采集器密钥（环境变量 EZNEWS_AUTH_API_KEYS=id:key）")
	}
	for i, k := range c.Auth.APIKeys {
		if strings.TrimSpace(k.ID) == "" || strings.TrimSpace(k.Key) == "" {
			return fmt.Errorf("配置校验失败: auth.apiKeys[%d] 的 id 或 key 为空", i)
		}
	}
	if c.Ingest.MaxBatchItems <= 0 {
		return fmt.Errorf("配置校验失败: ingest.maxBatchItems 必须为正数")
	}
	if c.Ingest.MaxBodyBytes <= 0 {
		return fmt.Errorf("配置校验失败: ingest.maxBodyBytes 必须为正数")
	}
	if c.Auth.Argon2MaxConcurrency <= 0 {
		c.Auth.Argon2MaxConcurrency = 2
	}
	if c.TTS.Concurrency <= 0 {
		c.TTS.Concurrency = 3
	}
	if c.TTS.QueueSize <= 0 {
		c.TTS.QueueSize = 512
	}
	if c.TTS.MaxChars <= 0 {
		c.TTS.MaxChars = 800
	}
	if c.Auth.RefreshCookiePath == "" {
		c.Auth.RefreshCookiePath = "/api/v1/auth"
	}
	// DEC-12：两个 refresh token 传输通道都关闭时，任何客户端都拿不到 refresh token，
	// 症状是「能登录、2 小时后集体静默掉线」——上线后才暴露且极难排查，必须在启动时拦掉。
	if !c.Auth.RefreshCookie && !c.Auth.RefreshTokenInBody {
		return fmt.Errorf("配置校验失败: auth.refreshCookie 与 auth.refreshTokenInBody 不能同时为 false，" +
			"否则 Web 与 Android 端都拿不到 refresh token，会在 access token 过期（2 小时）后集体静默掉线。" +
			"请至少开启其中一个（推荐两个都开）")
	}
	if c.Auth.Argon2QueueTimeoutMs <= 0 {
		c.Auth.Argon2QueueTimeoutMs = 3000
	}
	if c.Auth.RefreshReuseGraceSec < 0 {
		c.Auth.RefreshReuseGraceSec = 60
	}
	if c.Auth.RefreshReplayGuardMax <= 0 {
		c.Auth.RefreshReplayGuardMax = 10
	}
	if c.Auth.Argon2.Memory < 8192 {
		// 低于 8 MiB 的 Argon2id 内存硬度已无实际防护意义，宁可启动失败也不要静默弱哈希。
		return fmt.Errorf("配置校验失败: auth.argon2.memory=%d KiB 过低（最小 8192 KiB = 8 MiB，"+
			"推荐 19456 = 19 MiB，低内存设备可下调至 12288 = 12 MiB）", c.Auth.Argon2.Memory)
	}
	return nil
}

// HasTTSCredentials 判断当前是否配置了可用的云 TTS 凭据。
// 未配置时服务仍应正常启动，仅在合成请求时返回明确错误（见 README「TTS 降级」）。
func (c *Config) HasTTSCredentials() bool {
	return strings.TrimSpace(c.TTS.Credentials.SecretID) != "" &&
		strings.TrimSpace(c.TTS.Credentials.SecretKey) != ""
}

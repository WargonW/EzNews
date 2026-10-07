package config

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load 按「默认值 → YAML 文件 → 环境变量」的顺序构建配置，并做启动期校验。
//
// path 为空表示不加载任何配置文件（纯环境变量部署，便于容器化）。
//
// ★ 文件不存在分两种语义，取决于 explicit（该路径是否被部署者显式指定）：
//
//  1. explicit=false（用的是 -config 的默认值 config.yaml，文件不存在是常态）
//     → 不报错，静默降级为纯环境变量 + 默认值。
//     Dockerfile 刻意不 COPY config.yaml，配置全靠环境变量注入，
//     若此处报错，整个容器化部署路径直接不可用，与项目「轻量、自托管」定位相悖。
//     为了让部署者能确认「配置到底从哪来」，降级时打一条 INFO 日志。
//
//  2. explicit=true（用户通过 -config / EZNEWS_CONFIG 明确指了这个文件）
//     → 仍然报错。显式指定意味着用户期望它被读取，静默降级会让
//     「配置文件路径拼错 / 挂载卷没挂上」这类部署事故无声失效：
//     服务照常起来，但所有配置都悄悄退回默认值，最典型的后果是
//     换了端口/密钥却以为配上了，排查成本极高。
//     同一个错误报出来只要 1 秒，静默失效要排查几小时。
//
// 只判断「文件是否存在」：读得到就解析（未知键仍由 KnownFields(true) 拦下），
// 读不到时用 os.IsNotExist 区分「不存在」与「权限拒绝 / 路径是目录」等真实 IO 故障，
// 后者一律 fail-fast。这里不额外用 os.Stat 先探测再 ReadFile——那多一次 syscall 且留了
// TOCTOU 窗口；直接读文件再分类 err 即可，ReadFile 返回 *PathError，os.IsNotExist 能正确识别。
func Load(path string, explicit bool) (*Config, error) {
	cfg := Defaults()

	if p := strings.TrimSpace(path); p != "" {
		raw, err := os.ReadFile(p)
		switch {
		case err == nil:
			// 展开 ${VAR} 占位符（未定义的环境变量展开为空串，随后由 env 覆盖补齐）。
			expanded := os.Expand(string(raw), os.Getenv)
			// KnownFields(true)：未知键直接报错。否则部署者把 registrationEnabled 写成
			// registration_enabled 会被静默丢弃并回落默认值 true——单人自托管场景下等于
			// 「以为锁了注册门，实际门户大开且毫无提示」，属于不可见的安全开关失效。
			dec := yaml.NewDecoder(strings.NewReader(expanded))
			dec.KnownFields(true)
			if derr := dec.Decode(cfg); derr != nil && derr.Error() != "EOF" {
				return nil, fmt.Errorf("解析配置文件 %s 失败（存在未知配置键，请核对键名，参见 config.example.yaml）: %w", p, derr)
			}
		case os.IsNotExist(err) && !explicit:
			// 默认路径不存在 = 纯环境变量部署，跳过文件加载继续往下走。
			slog.Info("未找到配置文件，仅使用环境变量与内置默认值（纯环境变量部署模式）",
				slog.String("configPath", p),
				slog.String("提示", "如需从文件加载，请用 -config 指定实际存在的文件路径"))
		default:
			return nil, fmt.Errorf("读取配置文件 %s 失败: %w", p, err)
		}
	}

	if err := applyEnvOverrides(cfg); err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEnvOverrides 用 EZNEWS_* 环境变量覆盖已加载的配置项。
// 仅覆盖环境变量中显式出现的键，未设置时保留 YAML/默认值。
//
// 解析失败一律收集为错误并在启动时返回（fail-fast）：容器化部署通常没有 YAML 文件，
// 环境变量是唯一注入路径，静默忽略非法值会导致「部署者以为配了、实际没生效」的静默失效。
func applyEnvOverrides(c *Config) error {
	var errs []string
	fail := func(env string, err error) {
		errs = append(errs, fmt.Sprintf("%s=%v", env, err))
	}

	setString := func(env string, dst *string) {
		if v, ok := os.LookupEnv(env); ok {
			*dst = strings.TrimSpace(v)
		}
	}
	setInt := func(env string, dst *int) {
		if v, ok := os.LookupEnv(env); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				fail(env, err)
				return
			}
			*dst = n
		}
	}
	// setUint32 用于 Argon2id 的 memory(KiB) / time 等无符号字段。
	// 绝不能像以前那样强转成 *int：那会在 64 位平台下写 8 字节、踩掉紧邻的 Time 字段，
	// 导致迭代次数被静默改成 0、密码哈希强度归零且不报错。
	setUint32 := func(env string, dst *uint32) {
		if v, ok := os.LookupEnv(env); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 32)
			if err != nil {
				fail(env, err)
				return
			}
			*dst = uint32(n)
		}
	}
	setUint8 := func(env string, dst *uint8) {
		if v, ok := os.LookupEnv(env); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 8)
			if err != nil {
				fail(env, err)
				return
			}
			*dst = uint8(n)
		}
	}
	setInt64 := func(env string, dst *int64) {
		if v, ok := os.LookupEnv(env); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				fail(env, err)
				return
			}
			*dst = n
		}
	}
	setFloat := func(env string, dst *float64) {
		if v, ok := os.LookupEnv(env); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				fail(env, err)
				return
			}
			*dst = f
		}
	}
	setBool := func(env string, dst *bool) {
		if v, ok := os.LookupEnv(env); ok {
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "1", "true", "yes", "on":
				*dst = true
			case "0", "false", "no", "off":
				*dst = false
			default:
				fail(env, fmt.Errorf("非法布尔值 %q（可用 1/0、true/false、yes/no、on/off）", v))
			}
		}
	}
	setDuration := func(env string, dst *Duration) {
		if v, ok := os.LookupEnv(env); ok {
			if err := dst.Set(v); err != nil {
				fail(env, err)
			}
		}
	}

	setString("EZNEWS_SERVER_ADDR", &c.Server.Addr)
	setDuration("EZNEWS_SERVER_READ_TIMEOUT", &c.Server.ReadTimeout)
	setDuration("EZNEWS_SERVER_WRITE_TIMEOUT", &c.Server.WriteTimeout)
	setDuration("EZNEWS_SERVER_SHUTDOWN_TIMEOUT", &c.Server.ShutdownTimeout)

	setString("EZNEWS_DB_PATH", &c.DB.Path)
	setInt("EZNEWS_DB_BUSY_TIMEOUT_MS", &c.DB.BusyTimeoutMs)

	setString("EZNEWS_AUTH_JWT_SECRET", &c.Auth.JWTSecret)
	setString("EZNEWS_AUTH_JWT_SECRET_PATH", &c.Auth.JWTSecretPath)
	setDuration("EZNEWS_AUTH_ACCESS_TOKEN_TTL", &c.Auth.AccessTokenTTL)
	setDuration("EZNEWS_AUTH_REFRESH_TOKEN_TTL", &c.Auth.RefreshTokenTTL)
	setDuration("EZNEWS_AUTH_LOCKOUT_DURATION", &c.Auth.LockoutDuration)
	setBool("EZNEWS_AUTH_REGISTRATION_ENABLED", &c.Auth.RegistrationEnabled)
	setBool("EZNEWS_AUTH_REQUIRE_KEY_FOR_READ", &c.Auth.RequireKeyForRead)
	setBool("EZNEWS_AUTH_REFRESH_COOKIE", &c.Auth.RefreshCookie)
	setBool("EZNEWS_AUTH_REFRESH_TOKEN_IN_BODY", &c.Auth.RefreshTokenInBody)
	setString("EZNEWS_AUTH_REFRESH_COOKIE_PATH", &c.Auth.RefreshCookiePath)
	setInt("EZNEWS_AUTH_REFRESH_REUSE_GRACE_SEC", &c.Auth.RefreshReuseGraceSec)
	setInt("EZNEWS_AUTH_REFRESH_REPLAY_GUARD_MAX", &c.Auth.RefreshReplayGuardMax)
	// Argon2id 参数：memory 单位是 KiB（默认 19456 = 19 MiB），不是 MiB。
	setUint32("EZNEWS_AUTH_ARGON2_MEMORY", &c.Auth.Argon2.Memory)
	setUint32("EZNEWS_AUTH_ARGON2_TIME", &c.Auth.Argon2.Time)
	setUint8("EZNEWS_AUTH_ARGON2_THREADS", &c.Auth.Argon2.Threads)
	setInt("EZNEWS_AUTH_ARGON2_MAX_CONCURRENCY", &c.Auth.Argon2MaxConcurrency)
	setInt("EZNEWS_AUTH_ARGON2_QUEUE_TIMEOUT_MS", &c.Auth.Argon2QueueTimeoutMs)
	setString("EZNEWS_AUTH_LOGIN_LIMIT_PER_USER", &c.Auth.LoginLimitPerUser)
	setString("EZNEWS_AUTH_LOGIN_LIMIT_PER_IP", &c.Auth.LoginLimitPerIP)

	// API Keys：EZNEWS_AUTH_API_KEYS="id1:key1,id2:key2"
	if v, ok := os.LookupEnv("EZNEWS_AUTH_API_KEYS"); ok {
		if keys := parseAPIKeys(v); len(keys) > 0 {
			c.Auth.APIKeys = keys
		}
	}
	if v, ok := os.LookupEnv("EZNEWS_API_KEY_1"); ok && strings.TrimSpace(v) != "" {
		upsertAPIKey(&c.Auth.APIKeys, APIKey{ID: "crawler-01", Key: strings.TrimSpace(v)})
	}

	setInt("EZNEWS_INGEST_MAX_BATCH_ITEMS", &c.Ingest.MaxBatchItems)
	setInt64("EZNEWS_INGEST_MAX_BODY_BYTES", &c.Ingest.MaxBodyBytes)
	setBool("EZNEWS_INGEST_STORE_CONTENT", &c.Ingest.StoreContent)
	setBool("EZNEWS_INGEST_AUTO_CREATE_SOURCE", &c.Ingest.AutoCreateSource)
	setString("EZNEWS_INGEST_DEFAULT_TIMEZONE", &c.Ingest.DefaultTimezone)

	setFloat("EZNEWS_RATELIMIT_GLOBAL_RPS", &c.RateLimit.GlobalRps)
	setInt("EZNEWS_RATELIMIT_GLOBAL_BURST", &c.RateLimit.GlobalBurst)

	setString("EZNEWS_TTS_PROVIDER", &c.TTS.Provider)
	setString("EZNEWS_TTS_FALLBACK_PROVIDER", &c.TTS.FallbackProvider)
	setString("EZNEWS_TTS_SECRET_ID", &c.TTS.Credentials.SecretID)
	setString("EZNEWS_TTS_SECRET_KEY", &c.TTS.Credentials.SecretKey)
	setString("EZNEWS_TTS_APP_ID", &c.TTS.Credentials.AppID)
	setString("EZNEWS_TTS_DEFAULT_VOICE", &c.TTS.DefaultVoice)
	setFloat("EZNEWS_TTS_DEFAULT_SPEED", &c.TTS.DefaultSpeed)
	setInt("EZNEWS_TTS_CONCURRENCY", &c.TTS.Concurrency)
	setInt("EZNEWS_TTS_MAX_CHARS", &c.TTS.MaxChars)
	setDuration("EZNEWS_TTS_TIMEOUT", &c.TTS.Timeout)

	setString("EZNEWS_AUDIO_DIR", &c.Audio.Dir)
	setInt64("EZNEWS_AUDIO_MAX_TOTAL_BYTES", &c.Audio.MaxTotalBytes)
	setInt("EZNEWS_AUDIO_MAX_AGE_DAYS", &c.Audio.MaxAgeDays)

	setBool("EZNEWS_RETENTION_ENABLED", &c.Retention.Enabled)
	setInt("EZNEWS_RETENTION_DAYS", &c.Retention.Days)
	setInt("EZNEWS_JANITOR_INTERVAL_MIN", &c.Janitor.IntervalMin)
	setBool("EZNEWS_SEARCH_ENABLE_FTS", &c.Search.EnableFTS)

	setString("EZNEWS_LOG_LEVEL", &c.Log.Level)
	setString("EZNEWS_LOG_FORMAT", &c.Log.Format)

	if len(errs) > 0 {
		return fmt.Errorf("环境变量解析失败（已阻止启动，避免静默失效）: %s", strings.Join(errs, "; "))
	}
	return nil
}

// parseAPIKeys 解析 "id1:key1,id2:key2" 形式的密钥列表。
func parseAPIKeys(v string) []APIKey {
	var out []APIKey
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, key, found := strings.Cut(part, ":")
		if !found {
			// 只给了密钥没有 ID：用序号自动生成 ID。
			out = append(out, APIKey{ID: fmt.Sprintf("key-%d", len(out)+1), Key: part})
			continue
		}
		out = append(out, APIKey{ID: strings.TrimSpace(id), Key: strings.TrimSpace(key)})
	}
	return out
}

// upsertAPIKey 按 ID 幂等地插入或替换一个密钥。
func upsertAPIKey(list *[]APIKey, k APIKey) {
	for i := range *list {
		if (*list)[i].ID == k.ID {
			(*list)[i] = k
			return
		}
	}
	*list = append(*list, k)
}

// ParseRate 解析 "5/m" "100/s" "20/h" 形式的速率描述，返回每秒令牌数与突发容量。
//
// ★ burst 必须取分子 N，而不是 int(rps)。这是个真实踩过的坑：
// "5/m" 的 rps = 0.083，int(0.083) = 0 → 兜底成 1，
// 于是配置声称"每分钟 5 次"，实际桶容量只有 1 ——
// 分钟内第 2 次请求就被拒，配置与行为直接对不上，且不报任何错。
// 令牌桶的语义是"桶里预装 N 个令牌、按 N/period 的速率回填"，burst=N 才是配置本意。
func ParseRate(s string) (rps float64, burst int, err error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, 0, fmt.Errorf("速率配置为空")
	}
	numStr := s
	perSecond := 1.0
	switch {
	case strings.HasSuffix(s, "/s"):
		numStr = strings.TrimSuffix(s, "/s")
		perSecond = 1
	case strings.HasSuffix(s, "/m"):
		numStr = strings.TrimSuffix(s, "/m")
		perSecond = 1.0 / 60.0
	case strings.HasSuffix(s, "/h"):
		numStr = strings.TrimSuffix(s, "/h")
		perSecond = 1.0 / 3600.0
	}
	n, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("非法速率 %q: %w", s, err)
	}
	if n <= 0 {
		return 0, 0, fmt.Errorf("速率 %q 必须为正数", s)
	}
	rps = n * perSecond
	burst = int(math.Ceil(n))
	if burst < 1 {
		burst = 1
	}
	return rps, burst, nil
}

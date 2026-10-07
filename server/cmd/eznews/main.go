// Command eznews 是 EZNews 单体服务端入口。
//
// 启动顺序（每一步失败都直接退出，绝不带病运行）：
//
//	配置加载(校验失败即退出) → 打开 SQLite → 迁移 schema → 准备目录 →
//	初始化 TTS provider → 装配 service → 拉起 TTS worker pool → 拉起 Janitor →
//	启动 HTTP 服务器 → 等待信号 → 优雅关闭
//
// 设计取向（ARCHITECTURE.md §2）：单进程单二进制，零外部中间件，
// 常驻内存目标 <150 MB，冷启动 <300 ms。
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/eznews/eznews/internal/auth"
	"github.com/eznews/eznews/internal/config"
	"github.com/eznews/eznews/internal/httpapi"
	"github.com/eznews/eznews/internal/observe"
	"github.com/eznews/eznews/internal/provider/tts"
	"github.com/eznews/eznews/internal/ratelimit"
	"github.com/eznews/eznews/internal/repo"
	"github.com/eznews/eznews/internal/service"
	"github.com/eznews/eznews/internal/store"
)

// buildVersion 由 ldflags -X main.buildVersion=v1.2.3 注入。
var buildVersion = "dev"

func main() {
	var (
		configPath  = flag.String("config", envOr("EZNEWS_CONFIG", "config.yaml"), "配置文件路径（默认值 config.yaml 不存在时仅用环境变量；显式指定后不存在则报错）")
		showVersion = flag.Bool("version", false, "打印版本并退出")
		resetPasswd = flag.String("reset-password", "", "离线重置指定用户的密码（用法: eznews -reset-password <username>）")
		promoteAdm  = flag.String("promote-admin", "", "冷启动：把已有账号提升为管理员（用法: eznews -promote-admin <username>）")
	)
	flag.Parse()

	// 配置文件路径是否被显式指定（-config 或 EZNEWS_CONFIG）。
	// 显式指定却读不到文件必须报错，不能静默退回纯环境变量——否则路径拼错、
	// 挂载卷没挂上这类部署事故会无声失效：服务照常起来但配置全是默认值。
	explicitConfig := flagPassed("config") || os.Getenv("EZNEWS_CONFIG") != ""

	if *showVersion {
		fmt.Printf("eznews %s\n", buildVersion)
		return
	}
	if *resetPasswd != "" {
		if err := runResetPassword(*configPath, *resetPasswd, explicitConfig); err != nil {
			fmt.Fprintf(os.Stderr, "重置密码失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *promoteAdm != "" {
		if err := runPromoteAdmin(*configPath, *promoteAdm, explicitConfig); err != nil {
			fmt.Fprintf(os.Stderr, "提升管理员失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(*configPath, explicitConfig); err != nil {
		// 此时日志系统可能尚未初始化，所以直接写 stderr。
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}
}

// flagPassed 报告指定名字的命令行参数是否被显式传入（区别于 flag 定义的默认值）。
func flagPassed(name string) bool {
	passed := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			passed = true
		}
	})
	return passed
}

// run 执行完整的启动-运行-关闭流程。
func run(configPath string, explicitConfig bool) error {
	// ---- 1. 配置 ----
	cfg, err := config.Load(configPath, explicitConfig)
	if err != nil {
		return fmt.Errorf("加载配置: %w", err)
	}
	logger := observe.Setup(cfg.Log.Level, cfg.Log.Format)

	// JWT 密钥：未显式配置时落盘到 jwtSecretPath，避免每次重启都把所有人踢下线。
	if err := ensureJWTSecret(cfg); err != nil {
		return err
	}

	// ---- 2. 数据库 ----
	db, err := store.Open(store.Options{
		Path:          cfg.DB.Path,
		BusyTimeoutMs: cfg.DB.BusyTimeoutMs,
		CacheSizeKB:   cfg.DB.CacheSizeKB,
		MaxOpenConns:  8,
	})
	if err != nil {
		return fmt.Errorf("打开数据库: %w", err)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := store.Migrate(ctx, db, cfg.Search.EnableFTS); err != nil {
		return fmt.Errorf("执行数据库迁移: %w", err)
	}

	// ---- 3. 目录 ----
	if err := os.MkdirAll(cfg.Audio.Dir, 0o755); err != nil {
		return fmt.Errorf("创建音频目录 %s: %w", cfg.Audio.Dir, err)
	}

	// ---- 4. 仓储层 ----
	articleRepo := repo.NewArticleRepo(db)
	sourceRepo := repo.NewSourceRepo(db)
	audioRepo := repo.NewAudioRepo(db)
	userRepo := repo.NewUserRepo(db)
	stateRepo := repo.NewUserStateRepo(db)

	// ---- 5. JWT ----
	jwtMgr, err := auth.NewJWTManager(cfg.Auth.JWTSecret, cfg.Auth.AccessTokenTTL.Std())
	if err != nil {
		return fmt.Errorf("初始化 JWT: %w", err)
	}

	// ---- 6. TTS provider ----
	ttsMgr := tts.NewManager(&cfg.TTS)
	registerTTSProviders(ttsMgr, cfg)
	if !ttsMgr.Available() {
		logger.Warn("未配置可用的云TTS 凭据，语音播报功能将返回 TTS_UNAVAILABLE；"+
			"其余功能（新闻浏览/收藏/增量同步）不受影响。可用 EZNEWS_TTS_PROVIDER=mock 先跑通链路",
			slog.String("provider", cfg.TTS.Provider))
	}

	// ---- 7. 服务层 ----
	metrics := observe.NewMetrics()
	ingestSvc, err := service.NewIngestService(db, articleRepo, sourceRepo, &cfg.Ingest, metrics)
	if err != nil {
		return fmt.Errorf("初始化 ingest 服务: %w", err)
	}
	articleSvc := service.NewArticleService(db, articleRepo, sourceRepo, audioRepo,
		db.FTSEnabled(), cfg.TTS.DefaultVoice, cfg.TTS.DefaultSpeed)
	sourceSvc := service.NewSourceService(db, sourceRepo, articleRepo)
	audioSvc := service.NewAudioService(db, audioRepo, articleRepo, ttsMgr, &cfg.TTS, &cfg.Audio, metrics)
	authSvc := service.NewAuthService(db, userRepo, jwtMgr, cfg.Auth)
	mergeSvc := service.NewMergeService(db, stateRepo, userRepo)
	stateSvc := service.NewUserStateService(stateRepo, db)
	adminSvc := service.NewAdminService(db, userRepo, articleRepo, audioRepo,
		audioSvc, authSvc, buildVersion)

	// ---- 8. 限流器 ----
	limiter := ratelimit.New(cfg.RateLimit.GlobalRps, cfg.RateLimit.GlobalBurst, 4096, 10*time.Minute)

	// ---- 9. HTTP ----
	srv, err := httpapi.New(httpapi.Deps{
		DB:       db,
		Config:   cfg,
		Metrics:  metrics,
		Logger:   logger,
		Limiter:  limiter,
		UserRepo: userRepo,
		Articles: articleSvc,
		Sources:  sourceSvc,
		Audio:    audioSvc,
		Ingest:   ingestSvc,
		Auth:     authSvc,
		Merge:    mergeSvc,
		State:    stateSvc,
		Admin:    adminSvc,
		JWT:      jwtMgr,
	})
	if err != nil {
		return fmt.Errorf("装配 HTTP 路由: %w", err)
	}
	httpSrv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      srv,
		ReadTimeout:  cfg.Server.ReadTimeout.Std(),
		WriteTimeout: cfg.Server.WriteTimeout.Std(),
		// 空闲连接超时。设 0（不超时）会让 Keep-Alive 连接长期堆积，
		// 在轻量部署的机器上是隐性内存泄漏。
		IdleTimeout: 120 * time.Second,
	}

	// ---- 10. TTS worker pool ----
	// 无 MQ：channel + 固定并发 worker（ARCHITECTURE.md §6）。
	var wg sync.WaitGroup
	workers := cfg.TTS.Concurrency
	if workers <= 0 {
		workers = 3
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for taskID := range audioSvc.Queue() {
				// 每个任务独立 context：不因一个任务超时而拖垮整个 worker。
				tctx, tcancel := context.WithTimeout(context.Background(), 2*time.Minute)
				if err := audioSvc.ProcessTask(tctx, taskID); err != nil {
					logger.Warn("TTS 任务处理失败",
						slog.Int64("taskId", taskID), slog.String("err", err.Error()))
				}
				tcancel()
			}
		}(i)
	}
	audioSvc.StartAccessFlusher(context.Background(),
		time.Duration(cfg.Audio.AccessFlushIntervalSec)*time.Second)
	logger.Info("TTS worker pool 已启动", slog.Int("workers", workers), slog.Int("queueSize", cfg.TTS.QueueSize))

	// ---- 11. Janitor ----
	janitor := service.NewJanitor(db, articleRepo, audioRepo, audioSvc, authSvc, cfg)
	janitor.Start()

	// ---- 12. 首次启动时把 pending 任务捞回队列 ----
	go func() {
		janitorCtx, jcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer jcancel()
		janitor.RunOnce(janitorCtx)
	}()

	// ---- 13. 监听退出信号 ----
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("HTTP 服务器已启动",
			slog.String("addr", cfg.Server.Addr),
			slog.String("version", buildVersion),
			slog.Int64("articles", countArticles(db)))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("HTTP 服务器异常退出: %w", err)
	case sig := <-stop:
		logger.Info("收到退出信号，开始优雅关闭", slog.String("signal", sig.String()))
	}

	// ---- 14. 优雅关闭 ----
	// 先停收新请求，再等后台任务收尾；顺序反了会在关闭期间丢请求。
	shutdownTimeout := cfg.Server.ShutdownTimeout.Std()
	if shutdownTimeout <= 0 {
		shutdownTimeout = 30 * time.Second
	}
	sctx, scancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer scancel()
	if err := httpSrv.Shutdown(sctx); err != nil {
		logger.Warn("HTTP 优雅关闭超时，强制关闭", slog.String("err", err.Error()))
	}
	janitor.Stop()
	audioSvc.Stop()
	// 队列不再有消费者，关闭 channel 让 worker 退出。
	// 注意：此刻 HTTP 已停，不会再有新任务入队。
	audioSvc.CloseQueue()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		logger.Warn("TTS worker 未在超时前退出")
	}

	logger.Info("已退出")
	return nil
}

// registerTTSProviders 按配置注册可用的 TTS provider。
// 主 provider 凭据缺失时自动回落到 mock，保证本地开发链路可跑通。
func registerTTSProviders(mgr *tts.Manager, cfg *config.Config) {
	primary := strings.ToLower(strings.TrimSpace(cfg.TTS.Provider))
	fallback := strings.ToLower(strings.TrimSpace(cfg.TTS.FallbackProvider))

	if cfg.HasTTSCredentials() {
		switch primary {
		case "ali", "aliyun":
			mgr.Register(tts.NewAliProvider(
				cfg.TTS.Credentials.SecretID, cfg.TTS.Credentials.SecretKey, cfg.TTS.Credentials.AppID, cfg.TTS.MaxChars))
		default: // tencent / 空值
			mgr.Register(tts.NewTencentProvider(
				cfg.TTS.Credentials.SecretID, cfg.TTS.Credentials.SecretKey, cfg.TTS.Credentials.AppID,
				cfg.TTS.MaxChars, cfg.TTS.Format, cfg.TTS.SampleRate))
		}
	} else {
		slog.Warn("云 TTS 凭据未配置（EZNEWS_TTS_SECRET_ID / _SECRET_KEY）")
	}

	// 兜底始终注册 mock：既能在无凭据环境下跑通全链路，
	// 也能在云服务故障时至少返回可播放的音频而不是彻底失败。
	mgr.Register(tts.NewMockProvider(cfg.TTS.MaxChars, cfg.TTS.SampleRate))

	// 显式配置了不同的 fallback 时再注册一次阿里云（需要独立的凭据时才生效）。
	if fallback != "" && fallback != primary && cfg.HasTTSCredentials() {
		if fallback == "ali" || fallback == "aliyun" {
			mgr.Register(tts.NewAliProvider(
				cfg.TTS.Credentials.SecretID, cfg.TTS.Credentials.SecretKey, cfg.TTS.Credentials.AppID, cfg.TTS.MaxChars))
		}
	}
}

// ensureJWTSecret 保证 JWT 密钥存在：优先用配置值，否则从 jwtSecretPath 读取/生成。
//
// 没有这一步的话，每次重启都会生成随机密钥，
// 结果是所有已签发的 Access Token 立即失效、用户被集体登出——
// 症状与"被攻击"完全一样，但原因毫无关联，极难排查。
func ensureJWTSecret(cfg *config.Config) error {
	if s := strings.TrimSpace(cfg.Auth.JWTSecret); s != "" {
		return nil
	}
	path := strings.TrimSpace(cfg.Auth.JWTSecretPath)
	if path == "" {
		return errors.New("auth.jwtSecret 与 auth.jwtSecretPath 均未配置，无法初始化 JWT 密钥")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("解析 jwtSecretPath: %w", err)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("读取 JWT 密钥 %s: %w", abs, err)
		}
		secret, gerr := auth.GenerateSecret()
		if gerr != nil {
			return gerr
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			return fmt.Errorf("创建 JWT 密钥目录: %w", err)
		}
		if err := os.WriteFile(abs, []byte(secret), 0o600); err != nil {
			return fmt.Errorf("写入 JWT 密钥 %s: %w", abs, err)
		}
		cfg.Auth.JWTSecret = secret
		slog.Info("已生成并持久化新的 JWT 密钥", slog.String("path", abs))
		return nil
	}
	secret := strings.TrimSpace(string(raw))
	if secret == "" {
		return fmt.Errorf("JWT 密钥文件 %s 内容为空，请删除后重启以重新生成", abs)
	}
	cfg.Auth.JWTSecret = secret
	return nil
}

// runResetPassword 离线重置密码（不进 HTTP 服务）。
//
// 自托管场景下用户把密码忘了、没有邮件找回手段，
// 这是唯一的自救通道——所以它必须独立于服务端运行。
func runResetPassword(configPath, username string, explicitConfig bool) error {
	cfg, err := config.Load(configPath, explicitConfig)
	if err != nil {
		return err
	}
	observe.Setup(cfg.Log.Level, cfg.Log.Format)
	db, err := store.Open(store.Options{Path: cfg.DB.Path, BusyTimeoutMs: cfg.DB.BusyTimeoutMs})
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	users := repo.NewUserRepo(db)
	svc := service.NewAuthService(db, users, nil, cfg.Auth)
	pwd, err := readPasswordTwice()
	if err != nil {
		return err
	}
	hasher := auth.NewArgon2Hasher(cfg.Auth.Argon2.Memory, cfg.Auth.Argon2.Time, cfg.Auth.Argon2.Threads)
	hash, err := hasher.Hash(pwd)
	if err != nil {
		return err
	}
	tx, err := db.BeginWrite(context.Background())
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	u, err := users.FindByUsername(context.Background(), username)
	if err != nil {
		return fmt.Errorf("用户 %s 不存在: %w", username, err)
	}
	now := time.Now().UnixMilli()
	if err := users.UpdatePassword(context.Background(), tx, u.ID, hash, now); err != nil {
		return err
	}
	// 重置密码必须让所有已签发的 Access Token 失效，否则旧 token 在 2 小时内仍可用。
	if _, err := users.BumpTokenVersion(context.Background(), tx, u.ID, now); err != nil {
		return err
	}
	if _, err := users.RevokeAllSessions(context.Background(), tx, u.ID, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_ = svc // AuthService 目前不需要参与重置流程，保留引用避免未使用告警
	fmt.Printf("已重置用户 %s 的密码，并撤销其全部会话。\n", username)
	return nil
}

// runPromoteAdmin 把已有账号提升为管理员。
//
// ★ 存在的理由：brand-new 部署时库里一个管理员都没有，
//
//	而所有 /admin/** 端点都要求管理员身份 —— 鸡生蛋问题。
//	注册接口出于安全考虑只能造出 role=user 的普通账号，
//	所以必须有一条带服务器权限的旁路来打破这个循环。
//
//	它必须是**命令行**而不是某个 HTTP 接口：任何 HTTP 形式的"自动提升"
//	都等价于向任何能注册的人开放管理员权限。
func runPromoteAdmin(configPath, username string, explicitConfig bool) error {
	cfg, err := config.Load(configPath, explicitConfig)
	if err != nil {
		return err
	}
	observe.Setup(cfg.Log.Level, cfg.Log.Format)
	db, err := store.Open(store.Options{Path: cfg.DB.Path, BusyTimeoutMs: cfg.DB.BusyTimeoutMs})
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	// ★ 必须先跑迁移：ALTER TABLE user ADD COLUMN disabled_at 是 005 加的，
	//   对一个还没升级的老库直接查 role/disabled_at 会报 no such column，
	//   而这个报错完全指向错误的方向（看起来像代码 bug 而非"忘记迁移"）。
	if err := store.Migrate(ctx, db, cfg.Search.EnableFTS); err != nil {
		return fmt.Errorf("执行数据库迁移: %w", err)
	}

	users := repo.NewUserRepo(db)
	u, err := users.FindByUsername(ctx, username)
	if err != nil {
		return fmt.Errorf("查询用户 %s 失败: %w", username, err)
	}
	if u.Disabled() {
		return fmt.Errorf("用户 %s 已被停用，请先启用后再提升", username)
	}
	tx, err := db.BeginWrite(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := users.AdminPromoteFirstUser(ctx, tx, u.ID, time.Now().UnixMilli()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	n, _ := users.AdminCountAdmins(ctx)
	fmt.Printf("已将 %s 提升为管理员，当前可用管理员数量: %d\n", username, n)
	return nil
}

// readPasswordTwice 从终端读两次密码，避免误输入。
// 不走命令行参数：`-reset-password user pass` 会把明文密码留在 shell 历史与进程列表里。
func readPasswordTwice() (string, error) {
	fmt.Print("请输入新密码: ")
	first, err := readPassword()
	if err != nil {
		return "", err
	}
	fmt.Print("请再次输入新密码: ")
	second, err := readPassword()
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("两次输入的密码不一致")
	}
	return first, nil
}

// readPassword 读一行输入。终端不回显属于加分项而非必需——
// 为了它引入 golang.org/x/term 依赖，在"节省资源/保持零额外依赖"的取向下不划算；
// 密码会短暂显示在屏幕上，这是可接受的取舍。
func readPassword() (string, error) {
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("读取密码失败: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// countArticles 启动时统计文章数（仅用于日志，便于发现"采集中没数据"）。
func countArticles(db *store.DB) int64 {
	n, err := repo.NewArticleRepo(db).CountAll(context.Background())
	if err != nil {
		return 0
	}
	return n
}

// envOr 返回环境变量值，缺省时用 def。
func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

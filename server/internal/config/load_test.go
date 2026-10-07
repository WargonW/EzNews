package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearEZNEWSEnv 清除进程里所有 EZNEWS_* 环境变量，并在测试结束后恢复原值。
//
// 必须这么做而不是只设需要的变量：CI / 开发机上可能残留 EZNEWS_AUTH_API_KEYS
// （本机跑过服务时常见），一旦漏清，「apiKeys 为空必须 fail-fast」这类
// 用例会因为环境里恰好有密钥而假绿。
func clearEZNEWSEnv(t *testing.T) {
	t.Helper()
	type saved struct {
		val string
		ok  bool
	}
	backup := map[string]saved{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(k, "EZNEWS_") {
			continue
		}
		backup[k] = saved{val: v, ok: true}
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unsetenv %s: %v", k, err)
		}
	}
	t.Cleanup(func() {
		for k, s := range backup {
			if s.ok {
				_ = os.Setenv(k, s.val)
			} else {
				_ = os.Unsetenv(k)
			}
		}
	})
}

// setEnv 设置一个 EZNEWS_* 环境变量（测试结束后由 t.Setenv 自动恢复）。
func setEnv(t *testing.T, key, val string) {
	t.Helper()
	t.Setenv(key, val)
}

// TestLoad_文件不存在但有环境变量_应成功且环境变量生效
//
// 这是本次修复的核心回归用例：修复前 Load 见到默认路径 config.yaml 不存在
// 就直接 fail-fast，导致纯环境变量部署（容器化）完全起不来。
func TestLoad_文件不存在但有环境变量_应成功且环境变量生效(t *testing.T) {
	clearEZNEWSEnv(t)
	setEnv(t, "EZNEWS_AUTH_API_KEYS", "c1:envkey123")
	setEnv(t, "EZNEWS_SERVER_ADDR", "127.0.0.1:18090")
	setEnv(t, "EZNEWS_DB_PATH", "./data/t.db")
	setEnv(t, "EZNEWS_TTS_PROVIDER", "mock")
	// 指向一个确定不存在的路径（模拟容器里没有 config.yaml）。
	missing := filepath.Join(t.TempDir(), "config.yaml")

	cfg, err := Load(missing, false)
	if err != nil {
		t.Fatalf("文件不存在时应降级为纯环境变量部署，却报错: %v", err)
	}
	if cfg.Server.Addr != "127.0.0.1:18090" {
		t.Errorf("环境变量未生效: server.addr=%q", cfg.Server.Addr)
	}
	if cfg.DB.Path != "./data/t.db" {
		t.Errorf("环境变量未生效: db.path=%q", cfg.DB.Path)
	}
	if cfg.TTS.Provider != "mock" {
		t.Errorf("环境变量未生效: tts.provider=%q", cfg.TTS.Provider)
	}
	if len(cfg.Auth.APIKeys) != 1 || cfg.Auth.APIKeys[0].ID != "c1" || cfg.Auth.APIKeys[0].Key != "envkey123" {
		t.Errorf("环境变量未生效: auth.apiKeys=%+v", cfg.Auth.APIKeys)
	}
	// 未出现在环境变量里的项应保留内置默认值，而不是被清零。
	if cfg.Ingest.MaxBatchItems != Defaults().Ingest.MaxBatchItems {
		t.Errorf("未设置的环境项应保留默认值: ingest.maxBatchItems=%d", cfg.Ingest.MaxBatchItems)
	}
}

// TestLoad_文件不存在且无APIKeys_必须失败
//
// 安全底线：降级为纯环境变量部署后，Validate 仍必须拦住「一个密钥都没有」的裸奔启动。
// 如果这条用例挂了，说明降级路径绕过了校验，属于最严重的安全回归。
func TestLoad_文件不存在且无APIKeys_必须失败(t *testing.T) {
	clearEZNEWSEnv(t)
	missing := filepath.Join(t.TempDir(), "config.yaml")

	cfg, err := Load(missing, false)
	if err == nil {
		t.Fatalf("auth.apiKeys 为空时必须 fail-fast，却成功加载: %+v", cfg)
	}
	if !strings.Contains(err.Error(), "apiKeys") {
		t.Errorf("错误信息应指明是 apiKeys 问题，实际: %v", err)
	}
}

// TestLoad_空路径_纯环境变量模式
func TestLoad_空路径_纯环境变量模式(t *testing.T) {
	clearEZNEWSEnv(t)
	setEnv(t, "EZNEWS_AUTH_API_KEYS", "k:v")

	cfg, err := Load("", false)
	if err != nil {
		t.Fatalf("空路径表示纯环境变量部署，不应报错: %v", err)
	}
	if len(cfg.Auth.APIKeys) != 1 {
		t.Errorf("环境变量应生效: auth.apiKeys=%+v", cfg.Auth.APIKeys)
	}
}

// TestLoad_文件存在且环境变量覆盖_环境变量优先
//
// 优先级链：内置默认值 < YAML < 环境变量。这条用例锁住最后一环，
// 防止有人把 applyEnvOverrides 挪到 YAML 之前。
func TestLoad_文件存在且环境变量覆盖_环境变量优先(t *testing.T) {
	clearEZNEWSEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yamlBody := "server:\n  addr: \"127.0.0.1:19000\"\nauth:\n  apiKeys:\n    - id: fromfile\n      key: filekey\n"
	if err := os.WriteFile(path, []byte(yamlBody), 0o600); err != nil {
		t.Fatalf("写入临时配置: %v", err)
	}

	// 先验证 YAML 单独生效。
	cfg, err := Load(path, true)
	if err != nil {
		t.Fatalf("加载 YAML 应成功: %v", err)
	}
	if cfg.Server.Addr != "127.0.0.1:19000" {
		t.Errorf("YAML 值未生效: server.addr=%q", cfg.Server.Addr)
	}
	if len(cfg.Auth.APIKeys) != 1 || cfg.Auth.APIKeys[0].ID != "fromfile" {
		t.Errorf("YAML 的 apiKeys 未生效: %+v", cfg.Auth.APIKeys)
	}

	// 再验证环境变量覆盖 YAML。
	setEnv(t, "EZNEWS_SERVER_ADDR", "0.0.0.0:19191")
	setEnv(t, "EZNEWS_AUTH_API_KEYS", "fromenv:envkey")
	cfg, err = Load(path, true)
	if err != nil {
		t.Fatalf("加载 YAML + 环境变量应成功: %v", err)
	}
	if cfg.Server.Addr != "0.0.0.0:19191" {
		t.Errorf("环境变量应覆盖 YAML: server.addr=%q", cfg.Server.Addr)
	}
	if len(cfg.Auth.APIKeys) != 1 || cfg.Auth.APIKeys[0].ID != "fromenv" {
		t.Errorf("环境变量应覆盖 YAML 的 apiKeys: %+v", cfg.Auth.APIKeys)
	}
}

// TestLoad_YAML含占位符_应展开为环境变量值
//
// 占位符字段刻意选 ingest.defaultTimezone，而不是 auth.apiKeys：
// apiKeys 有 EZNEWS_AUTH_API_KEYS 环境变量兜底（校验要求非空），
// 用它做断言会被 env 覆盖层盖掉，测出来的永远是 env 的值而非展开结果。
func TestLoad_YAML含占位符_应展开为环境变量值(t *testing.T) {
	clearEZNEWSEnv(t)
	setEnv(t, "EZNEWS_AUTH_API_KEYS", "c1:k")
	setEnv(t, "MY_INJECTED_TZ", "Asia/Shanghai")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yamlBody := "ingest:\n  defaultTimezone: \"${MY_INJECTED_TZ}\"\n"
	if err := os.WriteFile(path, []byte(yamlBody), 0o600); err != nil {
		t.Fatalf("写入临时配置: %v", err)
	}

	cfg, err := Load(path, true)
	if err != nil {
		t.Fatalf("加载含占位符的 YAML 应成功: %v", err)
	}
	if cfg.Ingest.DefaultTimezone != "Asia/Shanghai" {
		t.Errorf("${VAR} 未展开为环境变量值: ingest.defaultTimezone=%q", cfg.Ingest.DefaultTimezone)
	}
}

// TestLoad_YAML未定义占位符_展开为空串而非报错
func TestLoad_YAML未定义占位符_展开为空串而非报错(t *testing.T) {
	clearEZNEWSEnv(t)
	setEnv(t, "EZNEWS_AUTH_API_KEYS", "c1:k")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yamlBody := "ingest:\n  defaultTimezone: \"${MY_UNDEFINED_VAR_XYZ}\"\n"
	if err := os.WriteFile(path, []byte(yamlBody), 0o600); err != nil {
		t.Fatalf("写入临时配置: %v", err)
	}

	cfg, err := Load(path, true)
	if err != nil {
		t.Fatalf("未定义占位符应展开为空串而非报错: %v", err)
	}
	if cfg.Ingest.DefaultTimezone != "" {
		t.Errorf("未定义占位符应展开为空串: ingest.defaultTimezone=%q", cfg.Ingest.DefaultTimezone)
	}
}

// TestLoad_显式指定但不存在的路径_必须报错
//
// 本次修复刻意做出的语义选择：显式指定（-config / EZNEWS_CONFIG）的文件不存在时
// 不做静默降级。理由：用户明确指了文件，期待它被读取；悄悄退回默认值会让
// 「路径拼错」「挂载卷没挂上」无声失效，服务正常起来但配置全是默认值。
func TestLoad_显式指定但不存在的路径_必须报错(t *testing.T) {
	clearEZNEWSEnv(t)
	setEnv(t, "EZNEWS_AUTH_API_KEYS", "c1:envkey123")
	missing := filepath.Join(t.TempDir(), "no-such-file.yaml")

	cfg, err := Load(missing, true)
	if err == nil {
		t.Fatalf("显式指定的不存在路径必须报错，却成功加载: %+v", cfg)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("错误信息应包含出错的路径 %q，实际: %v", missing, err)
	}
}

// TestLoad_路径是目录而非文件_必须报错
//
// 目录不是「文件不存在」，不能被降级逻辑吞掉——os.IsNotExist 对 EISDIR 返回 false，
// 这条用例防止有人把判断写成「读不到就降级」。
func TestLoad_路径是目录而非文件_必须报错(t *testing.T) {
	clearEZNEWSEnv(t)
	setEnv(t, "EZNEWS_AUTH_API_KEYS", "c1:envkey123")
	dirPath := t.TempDir() // 是个目录，不是文件

	if _, err := Load(dirPath, false); err == nil {
		t.Fatal("路径是目录时必须报错（权限/类型类 IO 故障不应被静默降级），却成功了")
	}
}

// TestLoad_YAML含未知键_必须报错
//
// KnownFields(true) 的行为不能因为本次改动而放松：键名写错（如
// registration_enabled）若被静默丢弃，会回落默认值 true，
// 部署者以为锁了注册门，实际门户大开。
func TestLoad_YAML含未知键_必须报错(t *testing.T) {
	clearEZNEWSEnv(t)
	setEnv(t, "EZNEWS_AUTH_API_KEYS", "c1:envkey123")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yamlBody := "auth:\n  registration_enabled: false\n"
	if err := os.WriteFile(path, []byte(yamlBody), 0o600); err != nil {
		t.Fatalf("写入临时配置: %v", err)
	}

	cfg, err := Load(path, true)
	if err == nil {
		t.Fatalf("YAML 含未知键时必须报错，却成功加载: %+v", cfg)
	}
	if !strings.Contains(err.Error(), "未知配置键") {
		t.Errorf("错误信息应指明存在未知配置键，实际: %v", err)
	}
}

// TestLoad_环境变量非法值_必须报错
//
// 降级路径不能绕过 applyEnvOverrides 的 fail-fast：容器里环境变量是唯一注入路径，
// 静默忽略非法值等于「以为配了、实际没生效」。
func TestLoad_环境变量非法值_必须报错(t *testing.T) {
	clearEZNEWSEnv(t)
	setEnv(t, "EZNEWS_AUTH_API_KEYS", "c1:envkey123")
	setEnv(t, "EZNEWS_DB_BUSY_TIMEOUT_MS", "not-a-number")

	if _, err := Load(filepath.Join(t.TempDir(), "config.yaml"), false); err == nil {
		t.Fatal("环境变量非法值必须 fail-fast，却成功了")
	}
}

package tts

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/eznews/eznews/internal/config"
)

// 熔断参数（§6.4）：连续 3 次失败 → 标记 unhealthy 30s → 切 fallbackProvider。
const (
	failThreshold = 3
	unhealthyTTL  = 30 * time.Second
	healthWindow  = 10 * time.Minute // 失败计数清零窗口
)

// Manager 负责 provider 注册、选路与健康标记。
type Manager struct {
	mu          sync.RWMutex
	providers   map[string]TtsProvider
	failures    map[string]int
	failedAt    map[string]time.Time
	unhealthyAt map[string]time.Time
	primary     string
	fallback    string
}

// NewManager 按配置注册 provider。
//
// 注册规则：
//   - tts.provider=mock  → 注册 mock（无需密钥，本地联调用）
//   - 配置了腾讯云密钥    → 注册 tencent
//   - 配置了阿里云密钥    → 注册 ali（骨架）
//   - 一个都没注册        → Available()==false，服务仍正常启动，合成请求返回明确错误
func NewManager(cfg *config.TTSConf) *Manager {
	m := &Manager{
		providers:   make(map[string]TtsProvider),
		failures:    make(map[string]int),
		failedAt:    make(map[string]time.Time),
		unhealthyAt: make(map[string]time.Time),
		primary:     cfg.Provider,
		fallback:    cfg.FallbackProvider,
	}
	switch cfg.Provider {
	case "mock":
		m.providers["mock"] = NewMockProvider(cfg.MaxChars, cfg.SampleRate)
	case "ali":
		if cfg.Credentials.SecretID != "" && cfg.Credentials.SecretKey != "" {
			m.providers["ali"] = NewAliProvider(cfg.Credentials.SecretID, cfg.Credentials.SecretKey,
				cfg.Credentials.AppID, cfg.MaxChars)
		}
	case "xunfei":
		// P1 预留：未实现时不注册，合成请求返回 TTS_UNAVAILABLE
		slog.Warn("tts.provider=xunfei 尚未实现，合成功能不可用")
	default: // tencent
		if cfg.Credentials.SecretID != "" && cfg.Credentials.SecretKey != "" {
			m.providers["tencent"] = NewTencentProvider(cfg.Credentials.SecretID, cfg.Credentials.SecretKey,
				cfg.Credentials.AppID, cfg.MaxChars, cfg.Format, cfg.SampleRate)
		}
	}
	// fallback 也尝试注册（若其凭据存在）
	if cfg.FallbackProvider == "mock" {
		m.providers["mock"] = NewMockProvider(cfg.MaxChars, cfg.SampleRate)
	}
	return m
}

// Register 注册一个 provider（便于测试与未来扩展）。
func (m *Manager) Register(p TtsProvider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.providers[p.Name()] = p
}

// Available 返回是否存在可用 provider。
func (m *Manager) Available() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.providers) > 0
}

// PrimaryName 返回主 provider 名称（可能尚未注册）。
func (m *Manager) PrimaryName() string { return m.primary }

// Synth 选路合成：优先主 provider，不健康时回退；返回音频字节与实际使用的 provider 名称。
func (m *Manager) Synth(ctx context.Context, req SynthRequest) ([]byte, string, error) {
	names := m.candidates()
	if len(names) == 0 {
		return nil, "", ErrNoProvider
	}
	var lastErr error
	for _, name := range names {
		if !m.isHealthy(name) {
			lastErr = ErrProviderUnhealthy
			continue
		}
		p, ok := m.get(name)
		if !ok {
			continue
		}
		data, err := p.Synthesize(ctx, req)
		if err == nil {
			m.markSuccess(name)
			return data, name, nil
		}
		lastErr = err
		m.markFailure(name)
		slog.Warn("TTS 合成失败", slog.String("provider", name), slog.String("err", err.Error()))
	}
	if lastErr == nil {
		lastErr = ErrNoProvider
	}
	return nil, "", lastErr
}

// candidates 返回本次尝试顺序：[primary, fallback, 其他已注册 provider]。
func (m *Manager) candidates() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	seen := map[string]bool{}
	add := func(n string) {
		if n == "" || seen[n] {
			return
		}
		if _, ok := m.providers[n]; ok {
			seen[n] = true
			out = append(out, n)
		}
	}
	add(m.primary)
	add(m.fallback)
	for name := range m.providers {
		add(name)
	}
	return out
}

// get 读取 provider 实例。
func (m *Manager) get(name string) (TtsProvider, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.providers[name]
	return p, ok
}

// isHealthy 判断 provider 是否处于健康状态（熔断期内视为不健康）。
func (m *Manager) isHealthy(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if at, ok := m.unhealthyAt[name]; ok && time.Since(at) < unhealthyTTL {
		return false
	}
	return true
}

// markFailure 记录一次失败，达到阈值则熔断。
func (m *Manager) markFailure(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.failedAt[name]; !ok || time.Since(t) > healthWindow {
		m.failures[name] = 0
	}
	m.failures[name]++
	m.failedAt[name] = time.Now()
	if m.failures[name] >= failThreshold {
		m.unhealthyAt[name] = time.Now()
		m.failures[name] = 0
		slog.Warn("TTS provider 已熔断", slog.String("provider", name),
			slog.Duration("ttl", unhealthyTTL))
	}
}

// markSuccess 清零失败计数与健康标记。
func (m *Manager) markSuccess(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures[name] = 0
	delete(m.unhealthyAt, name)
}

// MaxChars 返回主 provider 的字符上限；无 provider 时返回配置值。
func (m *Manager) MaxChars(fallback int) int {
	if p, ok := m.get(m.primary); ok {
		return p.MaxChars()
	}
	return fallback
}

// String 返回 Manager 的可读摘要（用于启动日志）。
func (m *Manager) String() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.providers))
	for n := range m.providers {
		names = append(names, n)
	}
	return fmt.Sprintf("primary=%s fallback=%s registered=%v", m.primary, m.fallback, names)
}

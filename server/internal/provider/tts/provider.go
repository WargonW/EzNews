// Package tts 定义 TtsProvider 抽象与各厂商实现。
//
// 设计约束（ARCHITECTURE.md §6.4）：对外只暴露接口，切换厂商零业务代码改动。
// 本实现**不引入任何云 SDK**：腾讯云走 REST + 自签名 TC3-HMAC-SHA256（零额外依赖），
// 另提供无需密钥的 mock 实现用于本地联调。
package tts

import (
	"context"
	"errors"
)

// SynthRequest 是一次合成请求（文本已清洗并截断）。
type SynthRequest struct {
	Text       string  // 已清洗、已截断
	Voice      string  // 音色 ID（厂商自有命名，由配置映射）
	Speed      float64 // 0.5 ~ 2.0，默认 1.0
	Format     string  // "mp3" | "wav"
	SampleRate int     // 默认 16000
}

// TtsProvider 是语音合成厂商的统一抽象。
type TtsProvider interface {
	// Name 返回 provider 标识："tencent" | "ali" | "xunfei" | "mock"
	Name() string
	// Synthesize 合成音频，返回音频字节（音频容器由实现决定）
	Synthesize(ctx context.Context, req SynthRequest) ([]byte, error)
	// MaxChars 返回单次合成的字符上限
	MaxChars() int
	// AudioFormat 返回该 provider 产出的音频格式（"mp3" / "wav"）
	AudioFormat() string
}

// ErrNoProvider 表示当前没有可用的 TTS provider（未配置密钥等）。
var ErrNoProvider = errors.New("未配置可用的 TTS provider")

// ErrProviderUnhealthy 表示 provider 暂时不可用（连续失败被熔断）。
var ErrProviderUnhealthy = errors.New("TTS provider 暂时不可用")

// ErrEmptyText 表示待合成文本为空。
var ErrEmptyText = errors.New("待合成文本为空")

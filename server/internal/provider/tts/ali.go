package tts

import (
	"context"
	"errors"
)

// ErrNotImplemented 表示 provider 尚未实现（P1 预留骨架）。
var ErrNotImplemented = errors.New("该 TTS provider 尚未实现")

// AliProvider 是阿里云 TTS 的预留骨架（P1）。
//
// 接入步骤（不改动任何业务代码）：实现 Synthesize 后，在 Manager 中按 provider 名称注册即可。
type AliProvider struct {
	AccessKeyID     string
	AccessKeySecret string
	AppKey          string
	MaxCharsV       int
}

// NewAliProvider 创建阿里云 provider 骨架。
func NewAliProvider(accessKeyID, accessKeySecret, appKey string, maxChars int) *AliProvider {
	if maxChars <= 0 {
		maxChars = 800
	}
	return &AliProvider{
		AccessKeyID: accessKeyID, AccessKeySecret: accessKeySecret, AppKey: appKey, MaxCharsV: maxChars,
	}
}

// Name 返回 provider 标识。
func (p *AliProvider) Name() string { return "ali" }

// MaxChars 返回单次字符上限。
func (p *AliProvider) MaxChars() int { return p.MaxCharsV }

// AudioFormat 返回产出音频格式。
func (p *AliProvider) AudioFormat() string { return "mp3" }

// Synthesize 预留实现：当前返回 ErrNotImplemented。
func (p *AliProvider) Synthesize(ctx context.Context, req SynthRequest) ([]byte, error) {
	return nil, ErrNotImplemented
}

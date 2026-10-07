package tts

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// MockProvider 生成一段可播放的 WAV（提示音），用于本地联调与冒烟测试。
// 无需任何云密钥；默认不启用，通过 tts.provider=mock 开启。
type MockProvider struct {
	MaxCharsV  int
	SampleRate int
	// MaxDurationSec 限制单段音频时长（默认 10 秒）
	MaxDurationSec int
}

// NewMockProvider 创建 mock provider。
func NewMockProvider(maxChars int, sampleRate int) *MockProvider {
	if maxChars <= 0 {
		maxChars = 800
	}
	if sampleRate == 0 {
		sampleRate = 16000
	}
	return &MockProvider{MaxCharsV: maxChars, SampleRate: sampleRate, MaxDurationSec: 10}
}

// Name 返回 provider 标识。
func (p *MockProvider) Name() string { return "mock" }

// MaxChars 返回单次字符上限。
func (p *MockProvider) MaxChars() int { return p.MaxCharsV }

// AudioFormat 返回产出音频格式（WAV）。
func (p *MockProvider) AudioFormat() string { return "wav" }

// Synthesize 生成一段 WAV 提示音（音高随文本长度变化，便于区分不同文章）。
func (p *MockProvider) Synthesize(ctx context.Context, req SynthRequest) ([]byte, error) {
	if strings.TrimSpace(req.Text) == "" {
		return nil, ErrEmptyText
	}
	// 时长：每 10 个字符 0.4 秒，落在 [1s, MaxDurationSec] 区间
	chars := utf8.RuneCountInString(req.Text)
	dur := float64(chars)/10.0*0.4 + 1.0
	if dur > float64(p.MaxDurationSec) {
		dur = float64(p.MaxDurationSec)
	}
	// 音高：由文本长度决定，范围 330–660 Hz
	freq := 330.0 + float64(chars%100)/100.0*330.0
	mockDelay() // 模拟一次网络往返，使 worker 链路更贴近真实
	return renderWAV(p.SampleRate, dur, freq), nil
}

// renderWAV 渲染 16-bit 单声道 PCM WAV：双音交替 + 淡入淡出包络。
func renderWAV(sampleRate int, seconds float64, baseFreq float64) []byte {
	frames := int(seconds * float64(sampleRate))
	if frames <= 0 {
		frames = sampleRate
	}
	var buf bytes.Buffer
	// RIFF 头
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+frames*2))
	buf.WriteString("WAVE")
	// fmt chunk
	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))           // chunk size
	binary.Write(&buf, binary.LittleEndian, uint16(1))            // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(1))            // mono
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))   // sample rate
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*2)) // byte rate
	binary.Write(&buf, binary.LittleEndian, uint16(2))            // block align
	binary.Write(&buf, binary.LittleEndian, uint16(16))           // bits per sample
	// data chunk
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(frames*2))

	for i := 0; i < frames; i++ {
		t := float64(i) / float64(sampleRate)
		// 每 0.5 秒在两个音高间切换，形成提示音节奏
		freq := baseFreq
		if math.Mod(t, 1.0) >= 0.5 {
			freq = baseFreq * 1.25
		}
		// 包络：淡入淡出各 40 ms，整体幅度 0.25
		amp := 0.25
		const fade = 0.04
		if t < fade {
			amp *= t / fade
		}
		if remain := seconds - t; remain < fade {
			amp *= remain / fade
		}
		sample := math.Sin(2*math.Pi*freq*t) * amp
		binary.Write(&buf, binary.LittleEndian, int16(sample*32767*0.9))
	}
	return buf.Bytes()
}

// EnsureMockWAVDuration 返回 mock 音频的时长（毫秒），供调用方估算 duration_ms。
func EnsureMockWAVDuration(sizeBytes int64, sampleRate int) int64 {
	if sampleRate <= 0 {
		sampleRate = 16000
	}
	dataBytes := sizeBytes - 44
	if dataBytes <= 0 {
		return 0
	}
	return int64(float64(dataBytes) / float64(sampleRate*2) * 1000)
}

// mockDelay 模拟一次网络往返，使 worker 链路更贴近真实（仅限 mock）。
func mockDelay() {
	time.Sleep(20 * time.Millisecond)
}

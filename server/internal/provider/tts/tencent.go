package tts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/eznews/eznews/internal/util"
)

// 腾讯云 TTS 常量（REST 接口，非 SDK）
const (
	tencentEndpoint = "https://tts.tencentcloudapi.com"
	tencentService  = "tts"
	tencentAction   = "TextToVoice"
	tencentVersion  = "2019-08-23"
	tencentRegion   = "ap-guangzhou"
	// defaultVoiceType 是腾讯云默认音色（智逍遥）。
	defaultVoiceType = 101001
)

// TencentProvider 通过腾讯云 TTS REST API 合成语音（TC3-HMAC-SHA256 自签名，零 SDK 依赖）。
type TencentProvider struct {
	SecretID   string
	SecretKey  string
	AppID      string
	MaxCharsV  int
	Format     string
	SampleRate int
	Client     *http.Client
}

// NewTencentProvider 创建腾讯云 provider。
func NewTencentProvider(secretID, secretKey, appID string, maxChars int, format string, sampleRate int) *TencentProvider {
	if maxChars <= 0 {
		maxChars = 800
	}
	if format == "" {
		format = "mp3"
	}
	if sampleRate == 0 {
		sampleRate = 16000
	}
	return &TencentProvider{
		SecretID: secretID, SecretKey: secretKey, AppID: appID,
		MaxCharsV: maxChars, Format: format, SampleRate: sampleRate,
		Client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Name 返回 provider 标识。
func (p *TencentProvider) Name() string { return "tencent" }

// MaxChars 返回单次字符上限。
func (p *TencentProvider) MaxChars() int { return p.MaxCharsV }

// AudioFormat 返回产出音频格式。
func (p *TencentProvider) AudioFormat() string { return p.Format }

// ttsResponse 是腾讯云 TTS 的响应结构。
type ttsResponse struct {
	Response struct {
		Audio     string `json:"Audio"`
		SessionID string `json:"SessionId"`
		RequestID string `json:"RequestId"`
		Error     *struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error"`
	} `json:"Response"`
}

// Synthesize 调用腾讯云 TextToVoice（同步返回 base64 音频）。
func (p *TencentProvider) Synthesize(ctx context.Context, req SynthRequest) ([]byte, error) {
	if strings.TrimSpace(req.Text) == "" {
		return nil, ErrEmptyText
	}
	voiceType := defaultVoiceType
	if v, err := strconv.Atoi(strings.TrimSpace(req.Voice)); err == nil && v != 0 {
		voiceType = v
	}
	// 语速映射：本项目 0.5~2.0（1.0 为正常）→ 腾讯云 -2~6（0 为正常）
	speed := (req.Speed - 1.0) * 4.0
	if speed < -2 {
		speed = -2
	}
	if speed > 6 {
		speed = 6
	}
	codec := req.Format
	if codec == "" {
		codec = p.Format
	}
	sampleRate := req.SampleRate
	if sampleRate == 0 {
		sampleRate = p.SampleRate
	}
	body := map[string]any{
		"Text":       req.Text,
		"SessionId":  util.RandomString(8),
		"Volume":     0,
		"Speed":      speed,
		"ProjectId":  0,
		"ModelType":  1,
		"VoiceType":  voiceType,
		"Codec":      codec,
		"SampleRate": sampleRate,
	}
	if p.AppID != "" {
		if appID, err := strconv.ParseInt(p.AppID, 10, 64); err == nil {
			body["ProjectId"] = appID
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("序列化合成请求失败: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, tencentEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("创建合成请求失败: %w", err)
	}
	now := time.Now()
	auth := signTC3(p.SecretID, p.SecretKey, tencentService, payload, now)
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	httpReq.Header.Set("Host", "tts.tencentcloudapi.com")
	httpReq.Header.Set("X-TC-Action", tencentAction)
	httpReq.Header.Set("X-TC-Version", tencentVersion)
	httpReq.Header.Set("X-TC-Timestamp", strconv.FormatInt(now.Unix(), 10))
	httpReq.Header.Set("X-TC-Region", tencentRegion)
	httpReq.Header.Set("Authorization", auth)

	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("调用腾讯云 TTS 失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 上限 8 MB，防止异常响应打爆内存
	if err != nil {
		return nil, fmt.Errorf("读取腾讯云响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("腾讯云 TTS 返回 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed ttsResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("解析腾讯云响应失败: %w", err)
	}
	if parsed.Response.Error != nil {
		return nil, fmt.Errorf("腾讯云 TTS 错误 %s: %s",
			parsed.Response.Error.Code, parsed.Response.Error.Message)
	}
	if parsed.Response.Audio == "" {
		return nil, errors.New("腾讯云 TTS 返回空音频")
	}
	return decodeBase64(parsed.Response.Audio)
}

// signTC3 计算腾讯云 TC3-HMAC-SHA256 签名（标准库实现，无 SDK 依赖）。
func signTC3(secretID, secretKey, service string, payload []byte, t time.Time) string {
	const (
		algorithm  = "TC3-HMAC-SHA256"
		signedHdrs = "content-type;host"
		host       = "tts.tencentcloudapi.com"
	)
	canonicalHeaders := "content-type:application/json; charset=utf-8\n" + "host:" + host + "\n"
	hashedPayload := sha256Hex(payload)
	canonicalRequest := strings.Join([]string{
		http.MethodPost,
		"/",
		"",
		canonicalHeaders,
		signedHdrs,
		hashedPayload,
	}, "\n")

	date := t.UTC().Format("2006-01-02")
	timestamp := strconv.FormatInt(t.Unix(), 10)
	credentialScope := date + "/" + service + "/tc3_request"
	stringToSign := strings.Join([]string{
		algorithm,
		timestamp,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("TC3"+secretKey), date)
	kService := hmacSHA256(kDate, service)
	kSigning := hmacSHA256(kService, "tc3_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	return fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, secretID, credentialScope, signedHdrs, signature)
}

// hmacSHA256 计算 HMAC-SHA256 摘要。
func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(data))
	return h.Sum(nil)
}

// sha256Hex 计算 SHA-256 的十六进制摘要。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// truncate 截断字符串用于错误信息。
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

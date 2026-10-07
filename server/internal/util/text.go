package util

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	htmlTagRe     = regexp.MustCompile(`(?is)<[^>]*>`)
	markdownRe    = regexp.MustCompile(`(?m)^\s{0,3}#{1,6}\s*|\*\*|\*|__|~~|` + "`" + `{1,3}|>\s?`)
	urlRe         = regexp.MustCompile(`(?i)https?://\S+|www\.\S+`)
	mdLinkRe      = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	imageRe       = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	wsRe          = regexp.MustCompile(`\s+`)
	controlRe     = regexp.MustCompile(`[\x00-\x08\x0B\x0C\x0E-\x1F\x7F]`)
	sentenceEndRe = regexp.MustCompile(`[。！？!?.；;]`)
)

// CleanTTSText 清洗待合成文本：
// 去 HTML 标签 → 去 Markdown 语法 → 去 URL → 合并空白 → 去控制字符 → 去 emoji（可选）。
func CleanTTSText(raw string, stripEmoji bool) string {
	s := raw
	s = htmlTagRe.ReplaceAllString(s, "")
	s = imageRe.ReplaceAllString(s, "")
	s = mdLinkRe.ReplaceAllString(s, "$1")
	s = markdownRe.ReplaceAllString(s, "")
	s = urlRe.ReplaceAllString(s, "")
	s = controlRe.ReplaceAllString(s, "")
	if stripEmoji {
		s = stripEmojis(s)
	}
	s = wsRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// TruncateText 按 maxChars 截断文本；优先在句末标点处切断，无标点则硬切并追加 "……"。
func TruncateText(s string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	runes := []rune(s)
	// 在 [maxChars-20, maxChars] 区间内寻找最后一个句末标点
	windowStart := maxChars - 20
	if windowStart < 0 {
		windowStart = 0
	}
	for i := maxChars - 1; i >= windowStart; i-- {
		if sentenceEndRe.MatchString(string(runes[i])) {
			return strings.TrimSpace(string(runes[:i+1]))
		}
	}
	return strings.TrimSpace(string(runes[:maxChars])) + "……"
}

// BuildTTSText 组装并清洗合成文本：title + "。" + summary（summary 为空则仅 title），随后截断。
func BuildTTSText(title, summary string, maxChars int, stripEmoji bool) string {
	raw := strings.TrimSpace(title)
	if sum := strings.TrimSpace(summary); sum != "" {
		raw = raw + "。" + sum
	}
	cleaned := CleanTTSText(raw, stripEmoji)
	return TruncateText(cleaned, maxChars)
}

// stripEmojis 移除 emoji 与其他非文本符号（保留 CJK、字母数字与常见标点）。
func stripEmojis(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r):
			b.WriteRune(r)
		case r >= 0x2000 && r <= 0x27BF: // 各类符号/装饰/箭头/杂项符号
			continue
		case r >= 0x1F000: // emoji 平面
			continue
		default:
			// CJK 标点、全角字符等保留
			if unicode.IsPunct(r) || (r >= 0x3000 && r <= 0x303F) || (r >= 0xFF00 && r <= 0xFFEF) {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

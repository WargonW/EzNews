package tts

import (
	"github.com/eznews/eznews/internal/util"
)

// BuildText 组装并清洗待合成文本：title + "。" + summary，随后按 maxChars 截断。
//
// 清洗规则（§6.5）：去 HTML 标签 → 去 Markdown → 去 URL → 合并空白 → 去控制字符 → 去 emoji。
func BuildText(title, summary string, maxChars int) string {
	return util.BuildTTSText(title, summary, maxChars, true)
}

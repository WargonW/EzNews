package util

import (
	"strconv"
	"strings"
	"testing"
)

// TestSHA256Hex_已知向量
//
// ★ 用标准已知向量把实现钉死。哈希算法一旦被"顺手优化"过
// （换成 FNV、加盐、换截断位置），所有已落库的 url_hash / content_hash
// 都会对不上——表现为「全量文章被判定为内容变更，重新抓取一遍」，
// 而且没有任何报错。向量是最便宜的防线。
func TestSHA256Hex_已知向量(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{
			"hello world",
			"b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
		},
	}
	for i, c := range cases {
		if got := SHA256Hex(c.in); got != c.want {
			t.Fatalf("用例 %d：SHA256Hex(%q)\n期望 %s\n实际 %s", i, c.in, c.want, got)
		}
	}
	// 中文没有可信的预计算值可写死，改为断言三条结构性质：
	// 长度 64、小写十六进制、与 UTF-8 字节绑定（不被"字符级"处理）。
	got := SHA256Hex("中文")
	if len(got) != 64 {
		t.Fatalf("摘要长度应为 64，实际 %d", len(got))
	}
	if strings.ToLower(got) != got {
		t.Fatalf("摘要应为小写十六进制，实际 %s", got)
	}
	if got == SHA256Hex("中文 ") {
		t.Fatal("中文摘要不应与带尾空格版本相同")
	}
}

// TestSHA256HexBytes_与字符串版一致
func TestSHA256HexBytes_与字符串版一致(t *testing.T) {
	s := "https://example.com/a?b=1"
	if SHA256HexBytes([]byte(s)) != SHA256Hex(s) {
		t.Fatal("SHA256HexBytes 与 SHA256Hex 结果不一致")
	}
	if SHA256HexBytes(nil) != SHA256Hex("") {
		t.Fatal("nil 字节切片应与空串一致")
	}
}

// TestURLHash_长度与字符集
//
// ★ 长度 32 是库表定义的一部分：ux_article 建在 url_hash 上，
// 若截断长度变化，全表唯一索引的语义随之改变（碰撞概率与去重行为都变）。
func TestURLHash_长度与字符集(t *testing.T) {
	for _, s := range []string{"", "https://example.com", "中文", strings.Repeat("x", 10000)} {
		h := URLHash(s)
		if len(h) != 32 {
			t.Fatalf("URLHash(%q) 长度应为 32，实际 %d (%q)", truncateForMsg(s), len(h), h)
		}
		if strings.ToLower(h) != h {
			t.Fatalf("URLHash 应为小写十六进制，实际 %s", h)
		}
		for _, r := range h {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Fatalf("URLHash 含非十六进制字符 %q", r)
			}
		}
	}
}

// TestURLHash_稳定
//
// ★ 哈希必须跨进程稳定：库里存的是上一次运行算出的值，
// 这次算出来不一样就会全表判为新文章。稳定性靠纯函数 + 固定算法保证。
func TestURLHash_稳定(t *testing.T) {
	cases := []string{"", "https://example.com/a", "https://example.com/a?b=1&c=2", "中文路径"}
	want := make([]string, len(cases))
	for i, c := range cases {
		want[i] = URLHash(c)
	}
	// 连续算1000 次必须完全一致
	for round := 0; round < 1000; round++ {
		for i, c := range cases {
			if got := URLHash(c); got != want[i] {
				t.Fatalf("第 %d 轮 URLHash(%q) 不稳定：%s -> %s", round, truncateForMsg(c), want[i], got)
			}
		}
	}
}

// TestURLHash_与SHA256Hex前32位一致
//
// 实现是 SHA256Hex(urlNorm)[:32]。把这条钉住的意义是：
// 任何"顺便改一下哈希算法/截断位置"的改动都会在这里暴露。
func TestURLHash_与SHA256Hex前32位一致(t *testing.T) {
	for _, s := range []string{"", "https://example.com", "a b c"} {
		if got, want := URLHash(s), SHA256Hex(s)[:32]; got != want {
			t.Fatalf("URLHash(%q) 应等于 SHA256Hex 前 32 位：%s != %s", truncateForMsg(s), got, want)
		}
	}
}

// TestURLHash_不同URL不碰撞
//
// 32 位十六进制 = 128 bit，截断后仍有极低碰撞概率，
// 但实现若误写成 [:16]（64 bit）或取模，碰撞概率会上升到工程上不可忽略。
// 这里用一批构造样本做交叉验证：任意两个哈希不得相同。
func TestURLHash_不同URL不碰撞(t *testing.T) {
	const n = 5000
	seen := make(map[string]string, n)
	for i := 0; i < n; i++ {
		// 构造共享长前缀、仅尾部不同的 URL —— 这是最容易暴露截断缺陷的形态
		prefix := "https://example.com/news/2026/10/07/article-"
		u := prefix + strconv.Itoa(i) + "?from=wx"
		h := URLHash(u)
		if prev, ok := seen[h]; ok {
			t.Fatalf("哈希碰撞：%q 与 %q 都得到 %s", prev, u, h)
		}
		seen[h] = u
	}
	// 单字符差异必须导致哈希不同（排除"只看长度"这类退化实现）
	base := "https://example.com/a"
	for _, c := range "0123456789abcdefghijklmnopqrstuvwxyz" {
		if URLHash(base+string(c)) == URLHash(base) {
			t.Fatalf("仅末字符变化（%q）却得到相同哈希", string(c))
		}
	}
}

// TestURLHash_等价URL变体哈希相同
//
// 端到端的去重保证：先归一、再哈希，两步合起来必须让等价变体得到同一哈希。
// 只测 NormalizeURL 或只测 URLHash 都不够——真正被业务依赖的是这条组合。
func TestURLHash_等价URL变体哈希相同(t *testing.T) {
	groups := [][]string{
		{
			"https://example.com/News/1",
			"HTTPS://EXAMPLE.COM/News/1",
			"https://Example.com/News/1/",
			"https://example.com:443/News/1",
			"https://example.com/News/1#top",
		},
		{
			"https://example.com/n?id=1&b=2",
			"https://example.com/n?b=2&id=1",
		},
	}
	blacklists := [][]string{nil, {"utm_*", "spm"}}
	// 注意：追踪参数的收敛只在传了黑名单时成立——nil 黑名单下
	// utm_source 会被保留，两个 URL 本来就是不同的。所以这组只在
	//有黑名单的迭代里验证。
	trackedVariants := [][]string{
		{
			"https://example.com/n?id=1&utm_source=wx",
			"https://example.com/n?id=1",
		},
	}
	for _, bl := range blacklists {
		for i, group := range groups {
			var want string
			for j, raw := range group {
				norm, err := NormalizeURL(raw, bl)
				if err != nil {
					t.Fatalf("归一化 %q 失败: %v", raw, err)
				}
				h := URLHash(norm)
				if j == 0 {
					want = h
					continue
				}
				if h != want {
					t.Fatalf("等价变体哈希不同（blacklist=%v 组%d）:\n  %q -> %s\n  %q -> %s",
						bl, i, group[0], want, raw, h)
				}
			}
		}
		// 追踪参数组只在非nil 黑名单下检查
		if bl == nil {
			continue
		}
		for i, group := range trackedVariants {
			var want string
			for j, raw := range group {
				norm, err := NormalizeURL(raw, bl)
				if err != nil {
					t.Fatalf("归一化 %q 失败: %v", raw, err)
				}
				h := URLHash(norm)
				if j == 0 {
					want = h
					continue
				}
				if h != want {
					t.Fatalf("追踪参数变体哈希不同（blacklist=%v 组%d）:\n  %q -> %s\n  %q -> %s",
						bl, i, group[0], want, raw, h)
				}
			}
		}
	}
}

// TestContentHashV1_前缀与长度
//
// ★ "v1:" 前缀是内容指纹的版本标记：算法升级时改成 v2，
// 新旧指纹天然不相等，UpdateIfChanged 才会把全部文章判定为"已变更"
// 而不需要任何数据迁移。这条断言防止有人不小心把前缀删掉。
func TestContentHashV1_前缀与长度(t *testing.T) {
	for _, s := range []string{"", "标题\n摘要\n正文", strings.Repeat("x", 100000)} {
		h := ContentHashV1(s)
		if !strings.HasPrefix(h, "v1:") {
			t.Fatalf("内容指纹应以 v1: 开头，实际 %q", h)
		}
		if len(h) != len("v1:")+32 {
			t.Fatalf("内容指纹长度应为 %d，实际 %d (%q)", len("v1:")+32, len(h), h)
		}
	}
}

// TestContentHashV1_稳定且非空内容不碰撞
func TestContentHashV1_稳定且非空内容不碰撞(t *testing.T) {
	const s = "标题\n摘要\n正文\n\n\ntech\n1700000000"
	first := ContentHashV1(s)
	if second := ContentHashV1(s); first != second {
		t.Fatalf("内容指纹不稳定: %s != %s", first, second)
	}
	// 只差一个字符就必须不同 —— 否则"内容变更检测"会漏掉真实变更
	seen := map[string]string{first: s}
	for i := 0; i < 2000; i++ {
		variant := s + strconv.Itoa(i)
		h := ContentHashV1(variant)
		if prev, ok := seen[h]; ok {
			t.Fatalf("内容指纹碰撞：%q 与 %q", prev, variant)
		}
		seen[h] = variant
	}
	if ContentHashV1(s) == ContentHashV1(s+" ") {
		t.Fatal("尾部加一个空格应导致指纹不同")
	}
}

// TestCanonicalContent_字段拼接与trim规则
//
// ★ 语义在实现注释里写死了：title/summary 要 trim，content 不 trim。
// 这个不对称是有意的——正文首尾的空白通常是排版产物，trim 掉能让
// "重新排版但内容没变"被正确判定为无变更；而 content 的缩进/换行
// 在代码块里有语义，trim 会误判。
func TestCanonicalContent_字段拼接与trim规则(t *testing.T) {
	a := CanonicalContent("标题", "摘要", "正文", "img", "作者", "tech", 1700000000)
	b := CanonicalContent("  标题  ", "  摘要  ", "正文", "img", "作者", "tech", 1700000000)
	if a != b {
		t.Fatalf("title/summary 的首尾空白应被 trim:\n%q\n%q", a, b)
	}
	// content 的首尾空白必须保留
	c := CanonicalContent("标题", "摘要", "  正文  ", "img", "作者", "tech", 1700000000)
	if a == c {
		t.Fatal("content 的首尾空白不应被 trim")
	}
	// 换行是字段分隔符：字段里含换行会造成歧义（已知的实现取舍，不改）
	// 这里只钉住「任一字段变化都导致指纹变化」
	if ContentHashV1(a) == ContentHashV1(c) {
		t.Fatal("content 变化应导致指纹变化")
	}
}

// TestCanonicalContent_任一字段变化都改变指纹
//
// 指纹的用途是判定"内容是否真实变更"。任何一个参与指纹的字段
// 漏掉了，就意味着那一维的变化永远检测不到。
func TestCanonicalContent_任一字段变化都改变指纹(t *testing.T) {
	base := CanonicalContent("t", "s", "c", "i", "a", "cat", 1000)
	variants := map[string]string{
		"title 变了":    CanonicalContent("t2", "s", "c", "i", "a", "cat", 1000),
		"summary 变了":  CanonicalContent("t", "s2", "c", "i", "a", "cat", 1000),
		"content 变了":  CanonicalContent("t", "s", "c2", "i", "a", "cat", 1000),
		"image 变了":    CanonicalContent("t", "s", "c", "i2", "a", "cat", 1000),
		"author 变了":   CanonicalContent("t", "s", "c", "i", "a2", "cat", 1000),
		"category 变了": CanonicalContent("t", "s", "c", "i", "a", "cat2", 1000),
		"时间变了":        CanonicalContent("t", "s", "c", "i", "a", "cat", 1001),
	}
	baseHash := ContentHashV1(base)
	for name, v := range variants {
		if ContentHashV1(v) == baseHash {
			t.Fatalf("%s：指纹必须变化", name)
		}
	}
	// publishedAt 参与指纹：正文没变但发布时间变了也算变更（列表排序依赖它）
	if ContentHashV1(CanonicalContent("t", "s", "c", "i", "a", "cat", 2000)) == baseHash {
		t.Fatal("发布时间变化应导致指纹变化")
	}
}

// TestCanonicalContent_不含URL与标签
//
// ★ 指纹刻意**不含** url / externalId / tags：URL 变动不代表内容变动
// （换域名、加参数都会改 URL），若纳入指纹，每次链接变化都会触发全量重抓。
// 这条断言把这个取舍钉住。
func TestCanonicalContent_不含URL与标签(t *testing.T) {
	base := ContentHashV1(CanonicalContent("t", "s", "c", "", "", "cat", 1000))
	if got := ContentHashV1(CanonicalContent("t", "s", "c", "", "", "cat", 1000)); got != base {
		t.Fatal("同样输入应得到同样指纹")
	}
	// 参数位只有 7 个，且 imageURL/author 传空与传空白都会参与拼接 ——
	// 所以这里反向验证：只要七参一致，指纹就一致（URL 不在其中）。
	if ContentHashV1(CanonicalContent("t", "s", "c", "img", "auth", "cat", 1000)) == base {
		t.Fatal("imageURL/author 参与指纹，传了值应不同")
	}
}

// truncateForMsg 截断长输入，避免失败信息刷屏。
func truncateForMsg(s string) string {
	if len(s) <= 40 {
		return s
	}
	return s[:40] + "..."
}

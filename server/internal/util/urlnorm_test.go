package util

import (
	"errors"
	"testing"
)

// TestNormalizeURL_表驱动
//
// ★ 全部期望值按实现的真实行为写（不是按"看起来应该怎样"写）。
// 有几条反直觉的，必须钉住：
//   - http 与 https **不**归一：它们是不同 URL，去重时不能合并。
//   - path **不**小写：URL path 大小写敏感（/A 与 /a 可能是两篇不同文章）。
//   - 无值参数会被补成 "key="：?b 与 ?b= 是同一个 query，Go 的 url.Values
//     解析后都只有一个空值元素，序列化时必然写成b=。
//   - 去末尾 "/" 用的是 TrimRight，会把多个尾斜杠一起去掉。
func TestNormalizeURL_表驱动(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		blacklist []string
		want      string
		wantErr   error
	}{
		// —— 基础 ——
		{name: "原样返回", raw: "https://example.com/a", want: "https://example.com/a"},
		{name: "根路径保留斜杠", raw: "https://example.com/", want: "https://example.com/"},
		{
			name: "去掉末尾斜杠", raw: "https://example.com/a/",
			want: "https://example.com/a",
		},
		{
			name: "去掉多个末尾斜杠", raw: "https://example.com/a///",
			want: "https://example.com/a",
		},
		{name: "首尾空白被trim", raw: "  https://example.com/a  ", want: "https://example.com/a"},

		// —— scheme / host 大小写 ——
		{
			name: "scheme与host小写", raw: "HTTPS://Example.COM/Path",
			want: "https://example.com/Path",
		},
		{
			// ★ path 大小写敏感，绝不能被小写化
			name: "path大小写必须保留", raw: "https://example.com/News/Article",
			want: "https://example.com/News/Article",
		},

		// —— 默认端口 ——
		{
			name: "去https默认端口", raw: "https://example.com:443/a",
			want: "https://example.com/a",
		},
		{
			name: "去http默认端口", raw: "http://example.com:80/a",
			want: "http://example.com/a",
		},
		{
			name: "非默认端口保留", raw: "https://example.com:8443/a",
			want: "https://example.com:8443/a",
		},
		{
			// ★ 端口只按 scheme 匹配：http 的 :443 不是默认端口，必须保留
			name: "http下的443不是默认端口", raw: "http://example.com:443/a",
			want: "http://example.com:443/a",
		},
		{
			name: "https下的80不是默认端口", raw: "https://example.com:80/a",
			want: "https://example.com:80/a",
		},

		// —— http vs https ——
		{
			// ★ http 与 https 不归一。它们指向不同资源（一个明文、一个密文），
			// 合并会让 http 版与 https 版互相覆盖，正文与图片全丢。
			name: "http与https不归一", raw: "http://example.com/a",
			want: "http://example.com/a",
		},

		// —— fragment ——
		{
			name: "去fragment", raw: "https://example.com/a#section",
			want: "https://example.com/a",
		},
		{
			name: "只有fragment的路径也保留", raw: "https://example.com/a#",
			want: "https://example.com/a",
		},

		// —— query 排序 ——
		{
			name: "query按键升序", raw: "https://example.com/a?z=1&a=2",
			want: "https://example.com/a?a=2&z=1",
		},
		{
			name: "query顺序不同归一一致", raw: "https://example.com/a?b=2&a=1",
			want: "https://example.com/a?a=1&b=2",
		},
		{
			name: "同键多值按值升序", raw: "https://example.com/a?t=9&t=1",
			want: "https://example.com/a?t=1&t=9",
		},
		{
			// ★ Go 的 url.Parse 把 ?b 解析成 key=b, value=""，
			// 序列化时必然写成 b=。这不是 bug，是 url.Values 的语义。
			name: "无值参数被补等号", raw: "https://example.com/a?b",
			want: "https://example.com/a?b=",
		},
		{
			// ★ QueryEscape 把空格编成 "+" 而不是 "%20"（这是 Go 的既定行为）。
			// 库里存的是归一串，只要归一过程稳定就不影响去重；
			// 但若哪天有人改用 PathEscape，url_hash 会全表漂移。
			name: "值被百分号编码_空格编成加号", raw: "https://example.com/a?q=中文 空格",
			want: "https://example.com/a?q=%E4%B8%AD%E6%96%87+%E7%A9%BA%E6%A0%BC",
		},
		{
			name: "key也被编码", raw: "https://example.com/a?a b=1",
			want: "https://example.com/a?a+b=1",
		},

		// —— 黑名单 ——
		{
			name: "过滤追踪参数", raw: "https://example.com/a?utm_source=x&id=1",
			blacklist: []string{"utm_*"},
			want:      "https://example.com/a?id=1",
		},
		{
			name: "过滤后无参数则不留问号", raw: "https://example.com/a?utm_source=x",
			blacklist: []string{"utm_*"},
			want:      "https://example.com/a",
		},
		{
			name: "黑名单精确匹配", raw: "https://example.com/a?spm=z&id=1",
			blacklist: []string{"spm"},
			want:      "https://example.com/a?id=1",
		},
		{
			name: "黑名单大小写不敏感", raw: "https://example.com/a?UTM_SOURCE=x",
			blacklist: []string{"utm_*"},
			want:      "https://example.com/a",
		},
		{
			name: "黑名单前缀通配", raw: "https://example.com/a?utm_campaign=x&from=y&id=1",
			blacklist: []string{"utm_*", "from"},
			want:      "https://example.com/a?id=1",
		},
		{
			// ★ 黑名单只按 key 匹配：值恰好等于黑名单词不该被删
			name: "黑名单只看key不看value", raw: "https://example.com/a?id=utm_source",
			blacklist: []string{"utm_*"},
			want:      "https://example.com/a?id=utm_source",
		},
		{
			name: "黑名单空串规则被忽略", raw: "https://example.com/a?a=1",
			blacklist: []string{"", "  "},
			want:      "https://example.com/a?a=1",
		},
		{
			name: "黑名单带空格被trim", raw: "https://example.com/a?spm=x&id=1",
			blacklist: []string{"  spm  "},
			want:      "https://example.com/a?id=1",
		},
		{
			name: "无黑名单时参数全留", raw: "https://example.com/a?utm_source=x",
			want: "https://example.com/a?utm_source=x",
		},

		// —— 中文路径 ——
		{
			name: "中文路径被百分号编码", raw: "https://example.com/中文",
			want: "https://example.com/%E4%B8%AD%E6%96%87",
		},

		// —— 错误 ——
		{name: "空串报错", raw: "", wantErr: ErrEmptyURL},
		{name: "全空白报错", raw: "   ", wantErr: ErrEmptyURL},
		{name: "无host报错", raw: "https:///a", wantErr: ErrInvalidURL},
		{name: "相对路径报错", raw: "/just/a/path", wantErr: ErrInvalidURL},
		{name: "无scheme报错", raw: "example.com/a", wantErr: ErrInvalidURL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NormalizeURL(c.raw, c.blacklist)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("期望错误 %v，实际 err=%v got=%q", c.wantErr, err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("归一化 %q 失败: %v", c.raw, err)
			}
			if got != c.want {
				t.Fatalf("NormalizeURL(%q)\n期望 %q\n实际 %q", c.raw, c.want, got)
			}
		})
	}
}

// TestNormalizeURL_幂等
//
// ★ 这是去重链的前提：normalize(normalize(x))必须等于 normalize(x)。
// 若不幂等，同一篇文章两次采集会算出两个不同的 url_hash，
// 去重失效、库里出现重复文章。这条比任何单条期望都重要。
func TestNormalizeURL_幂等(t *testing.T) {
	raws := []string{
		"https://example.com/a",
		"https://example.com/a/",
		"HTTPS://Example.COM/a/?z=1&utm_source=x&a=2#frag",
		"https://example.com:443/b?b=2&a=1&a=0",
		"http://example.com:80/",
		"https://example.com/中文 路径/",
		"https://example.com/a?b",
	}
	blacklists := [][]string{nil, {"utm_*", "spm", "from"}}
	for _, bl := range blacklists {
		for _, raw := range raws {
			once, err := NormalizeURL(raw, bl)
			if err != nil {
				t.Fatalf("首次归一化 %q 失败: %v", raw, err)
			}
			twice, err := NormalizeURL(once, bl)
			if err != nil {
				t.Fatalf("二次归一化 %q 失败: %v", once, err)
			}
			if once != twice {
				t.Fatalf("归一化不幂等（blacklist=%v）: %q -> %q -> %q", bl, raw, once, twice)
			}
		}
	}
}

// TestNormalizeURL_等价变体收敛到同一串
//
// 去重的核心价值：这些"看起来不一样但其实是同一篇"的写法必须收敛到同一结果。
// 若这里有任何一条不收敛，采集器就会把同一篇文章重复入库。
func TestNormalizeURL_等价变体收敛到同一串(t *testing.T) {
	groups := []struct {
		name      string
		vars      []string
		blacklist []string
	}{
		{
			name: "大小写与末尾斜杠",
			vars: []string{
				"https://example.com/News/1",
				"HTTPS://EXAMPLE.COM/News/1",
				"https://Example.com/News/1/",
				"  https://example.com/News/1  ",
			},
		},
		{
			name: "默认端口",
			vars: []string{
				"https://example.com:443/News/1",
				"https://example.com/News/1",
			},
		},
		{
			// ★ 这里刻意不放 "?a=1&a=1"：重复 key 不折叠（见 TestNormalizeURL_表驱动），
			// 把它当"等价变体"就是在要求一个实现上并不存在的收敛。
			name: "query顺序",
			vars: []string{"https://example.com/n?a=1&b=2", "https://example.com/n?b=2&a=1"},
		},
		{
			name: "fragment与追踪参数",
			vars: []string{
				"https://example.com/n?id=1",
				"https://example.com/n?id=1#top",
				"https://example.com/n?id=1&utm_source=wx&spm=a",
			},
			blacklist: []string{"utm_*", "spm"},
		},
		{
			name: "无值参数与空值参数",
			vars: []string{
				"https://example.com/n?a&b=1",
				"https://example.com/n?a=&b=1",
			},
		},
	}
	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			var want string
			for i, v := range g.vars {
				got, err := NormalizeURL(v, g.blacklist)
				if err != nil {
					t.Fatalf("归一化 %q 失败: %v", v, err)
				}
				if i == 0 {
					want = got
					continue
				}
				if got != want {
					t.Fatalf("等价变体未收敛:\n  %q -> %q\n  %q -> %q", g.vars[0], want, v, got)
				}
			}
		})
	}
}

// TestNormalizeURL_不等价变体必须保持不同
//
// 反向的不变量：不该把两篇不同文章归一成同一个键。
// 这条比"等价收敛"更危险——一旦不区分，采集器会用后一篇覆盖前一篇，
// 而且没有任何报错。
func TestNormalizeURL_不等价变体必须保持不同(t *testing.T) {
	groups := [][]string{
		{"https://example.com/a", "https://example.com/b"},
		{"http://example.com/a", "https://example.com/a"},  // scheme 不同
		{"https://example.com/A", "https://example.com/a"}, // path 大小写不同
		{"https://example.com/a", "https://example.com:8443/a"},
		{"https://example.com/a?id=1", "https://example.com/a?id=2"},
		{"https://example.com/a", "https://other.com/a"},
		{"https://example.com/a?x=1", "https://example.com/a?x=2"},
	}
	for _, group := range groups {
		t.Run(group[0]+" vs "+group[1], func(t *testing.T) {
			a, err := NormalizeURL(group[0], nil)
			if err != nil {
				t.Fatalf("归一化失败: %v", err)
			}
			b, err := NormalizeURL(group[1], nil)
			if err != nil {
				t.Fatalf("归一化失败: %v", err)
			}
			if a == b {
				t.Fatalf("不等价的两个 URL 被归一成同一个串 %q", a)
			}
		})
	}
}

// TestNormalizeURL_matchBlacklist 直接测前缀通配与精确匹配
func TestNormalizeURL_matchBlacklist(t *testing.T) {
	cases := []struct {
		key       string
		blacklist []string
		want      bool
	}{
		{key: "utm_source", blacklist: []string{"utm_*"}, want: true},
		{key: "UTM_SOURCE", blacklist: []string{"UTM_*"}, want: true},
		{key: "utmx", blacklist: []string{"utm_*"}, want: false},
		{key: "xutm", blacklist: []string{"utm_*"}, want: false},
		{key: "spm", blacklist: []string{"spm"}, want: true},
		{key: "spmid", blacklist: []string{"spm"}, want: false},
		{key: "spm", blacklist: []string{"*"}, want: true},
		{key: "", blacklist: []string{"*"}, want: true},
		{key: "a", blacklist: nil, want: false},
		{key: "a", blacklist: []string{"", "  "}, want: false},
	}
	for _, c := range cases {
		if got := matchBlacklist(c.key, c.blacklist); got != c.want {
			t.Fatalf("matchBlacklist(%q, %v) = %v，期望 %v", c.key, c.blacklist, got, c.want)
		}
	}
}

// TestIsHTTPURL_表驱动
func TestIsHTTPURL_表驱动(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{raw: "http://example.com", want: true},
		{raw: "https://example.com", want: true},
		{raw: "https://example.com/a?b=1#c", want: true},
		{raw: "  https://example.com  ", want: true},
		{
			// ★ url.Parse 会把 scheme 归一成小写，所以大写 scheme 也能通过。
			//（这与 NormalizeURL 里显式 strings.ToLower 是两回事——那是双保险。）
			raw: "HTTPS://example.com", want: true,
		},
		{raw: "https://example.com:8443/a", want: true},
		{raw: "ftp://example.com", want: false},
		{raw: "example.com", want: false},
		{raw: "", want: false},
		{raw: "   ", want: false},
		{raw: "https://", want: false},
		{raw: "javascript:alert(1)", want: false},
	}
	for _, c := range cases {
		if got := IsHTTPURL(c.raw); got != c.want {
			t.Fatalf("IsHTTPURL(%q) = %v，期望 %v", c.raw, got, c.want)
		}
	}
}

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 本文件钉住查询参数解析的边界。
//
// ★ 为什么值得测：`limit` 越界返回 400 是**设计如此**（openapi 明写最小/最大值），
//   但「只读 Centrifuge 缺省值」和「传了非法值」是两条完全不同的路径 ——
//   前者必须静默取缺省，后者必须明确报错。
//   把这两条弄反，客户端会得到两种同样糟糕的体验：
//   「忘传参数却得到冷冰冰的错误」或「传了乱七八糟的值却拿到默认页，
//   用户以为筛选生效了其实没有」。后者尤其隐蔽，因为它不报错。

func reqWithQuery(t *testing.T, raw string) *http.Request {
	t.Helper()
	// ★ 不能直接拼进 httptest.NewRequest 的 target：
	//   查询串里含未转义空格（如 "limit=   "）会让 url.Parse 失败，
	//   httptest.NewRequest 内部对解析失败直接 panic —— 而我们要测的恰恰就是
	//   这些"带空白/非法字符"的输入，会被这一 panic 挡在门外。
	//   直接赋 RawQuery 可以绕过 target 解析，URL.Query() 依旧正常工作。
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.URL.RawQuery = raw
	return r
}

func TestQueryInt_缺省取fallback且必须不报错(t *testing.T) {
	r := reqWithQuery(t, "")
	// 空 / 只有空白都应视为"未提供"，走 fallback 而不是报错。
	for _, raw := range []string{"", "   "} {
		r = reqWithQuery(t, "limit="+raw)
		got, err := queryInt(r, "limit", 20, 1, 100)
		if err != nil {
			t.Fatalf("%q 应取 fallback 不报错，实际 %v", raw, err)
		}
		if got != 20 {
			t.Errorf("%q 应得 fallback 20，实际 %d", raw, got)
		}
	}
}

func TestQueryInt_边界值恰好可用(t *testing.T) {
	cases := []struct {
		raw    string
		expect int
	}{
		{"1", 1},     // 下界
		{"100", 100}, // 上界
		{"50", 50},
	}
	for _, c := range cases {
		got, err := queryInt(reqWithQuery(t, "limit="+c.raw), "limit", 20, 1, 100)
		if err != nil {
			t.Errorf("limit=%s 应合法，实际报错: %v", c.raw, err)
		}
		if got != c.expect {
			t.Errorf("limit=%s 应得 %d，实际 %d", c.raw, c.expect, got)
		}
	}
}

// 越限必须报错，且不能被静默截断到边界值 ——
// 静默截断会让客户端以为自己拿到了请求的数量。
func TestQueryInt_越界与非整数必须报错(t *testing.T) {
	for _, raw := range []string{"0", "-1", "101", "abc", "1.5", "1e3", "０"} {
		if _, err := queryInt(reqWithQuery(t, "limit="+raw), "limit", 20, 1, 100); err == nil {
			t.Errorf("limit=%s 应报错，实际静默通过", raw)
		}
	}
}

// 全角数字 ０ 能被 strconv.Atoi 拒绝这条值得单独钉住：
// 它长得像合法输入，在某些 locale 下从前端表单进来并不罕见。
func TestQueryInt_全角数字必须被拒(t *testing.T) {
	if _, err := queryInt(reqWithQuery(t, "limit=１０"), "limit", 20, 1, 100); err == nil {
		t.Error("全角数字应被拒绝")
	}
}

func TestQueryInt64_必须大于零(t *testing.T) {
	if _, err := queryInt64(reqWithQuery(t, "id=0"), "id", 0); err == nil {
		t.Error("id=0 应报错（articleId 从 1 起）")
	}
	if _, err := queryInt64(reqWithQuery(t, "id=-5"), "id", 0); err == nil {
		t.Error("负数 id 应报错")
	}
	got, err := queryInt64(reqWithQuery(t, "id=42"), "id", 0)
	if err != nil || got != 42 {
		t.Errorf("id=42 应得 42，实际 %d err=%v", got, err)
	}
}

// ★ 刻意不用 strconv.ParseBool，而是手写 switch 额外接受 yes/on/no/off。
//
// 这条比《标准库更宽松，是有意的》—— HTML 表单含常见旁边必须严格 localStorage —— 我第一版按
// 「ParseBool 语义」写期望，把 yes/no/on 全部列为非法，结果全部红。
// **错的是测试不是实现**：ASSERT的实现,不要根据自己的假设。
func TestQueryBool_接受的形式(t *testing.T) {
	cases := map[string]bool{
		"true":   true,
		"false":  false,
		"1":      true,
		"0":      false,
		"TRUE":   true,
		"False":  false, // 大小写不敏感
		"yes":    true,  // 以下四个是 ParseBool 不支持、但本项目有意兼容的
		"no":     false,
		"on":     true,
		"off":    false,
		" true ": true, // 首尾空白应被 Trim
	}
	for raw, want := range cases {
		// 注意：直接赋 RawQuery，raw 里的空格不能进入 URL target（会 panic）。
		got, err := queryBool(reqWithQuery(t, "flag="+raw), "flag", !want)
		if err != nil {
			t.Errorf("flag=%q 应合法，实际 %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("flag=%q 应得 %v，实际 %v", raw, want, got)
		}
	}
}

func TestQueryBool_真正非法的值才报错(t *testing.T) {
	for _, raw := range []string{"maybe", "2", "-1", "t rue", "真"} {
		if _, err := queryBool(reqWithQuery(t, "flag="+raw), "flag", false); err == nil {
			t.Errorf("flag=%q 应报错，实际静默通过", raw)
		}
	}
}

// 空（含纯空白）视为「未提供」走 fallback，不得报错。
// 这条和 queryInt 的空值语义一致，但两者是两份实现，不能只测一边。
func TestQueryBool_缺省取fallback(t *testing.T) {
	for _, raw := range []string{"", "   "} {
		got, err := queryBool(reqWithQuery(t, "flag="+raw), "flag", true)
		if err != nil || got != true {
			t.Errorf("flag=%q 应得 fallback true，实际 %v err=%v", raw, got, err)
		}
		got, err = queryBool(reqWithQuery(t, "flag="+raw), "flag", false)
		if err != nil || got != false {
			t.Errorf("flag=%q 应得 fallback false，实际 %v err=%v", raw, got, err)
		}
	}
}

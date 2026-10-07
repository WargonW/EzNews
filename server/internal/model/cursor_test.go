package model

import (
	"encoding/base64"
	"testing"
)

// 本文件钉住 EncodeCursor / DecodeCursor 的往返契约。
//
// ★ 为什么要单独测它：cursor 是 openapi 里声明的 opaque 字符串，
//   客户端把它原样带回。**一旦编码格式改了（比如换了分隔符或加了字段），
//   老客户端手上所有已保存的游标会一次性全部失效**，而且是静默的
//   —— 服务端只会以为"没传游标"，然后返回首屏数据，
//   客户端本地已同步的水位就此丢失，表现为"刷新后文章重复出现"。
//   repo 层的测试验的是 "(ts, id) 行值比较" 的 SQL 语义，
//   **测不到这里的字符串格式**，所以这条链路只有本文件能守住。

func TestCursor_往返必须完全一致(t *testing.T) {
	cases := []struct{ ts, id int64 }{
		{0, 0},
		{1, 1},
		{1700000800, 42},
		{-1, -1}, // 负数是合法 int64，不得被吞掉
		{9223372036854775807, 9223372036854775807},
	}
	for _, c := range cases {
		got, err := DecodeCursor(EncodeCursor(c.ts, c.id))
		if err != nil {
			t.Fatalf("(%d,%d) 解码应成功，实际报错: %v", c.ts, c.id, err)
		}
		if got.TS != c.ts || got.ID != c.id {
			t.Errorf("(%d,%d) 往返后得到 (%d,%d)", c.ts, c.id, got.TS, got.ID)
		}
	}
}

// 编码必须是「无 padding 的 base64url + 冒号分隔」这一段既有格式。
// 写成标准是防止有人顺手改成 "%d_%d" 或加 JSON 包装。
func TestEncodeCursor_格式是裸可打印字符串(t *testing.T) {
	s := EncodeCursor(1700000800, 42)
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("必须是合法的 RawURL base64: %v", err)
	}
	if want := "1700000800:42"; string(raw) != want {
		t.Errorf("明文应为 %q，实际 %q", want, raw)
	}
	if s != base64.RawURLEncoding.EncodeToString([]byte("1700000800:42")) {
		t.Errorf("编码结果 %q 与预期不符", s)
	}
}

// 空串 = "没传游标"，必须返回零值且不报错，而不是报错。
// 首屏请求（不带 cursor）走的正是这条路径。
func TestDecodeCursor_空串视为未提供游标(t *testing.T) {
	for _, s := range []string{"", "   ", "\t"} {
		got, err := DecodeCursor(s)
		if err != nil {
			t.Fatalf("%q 应解码成功（视为未提供），实际报错: %v", s, err)
		}
		if got != ZeroCursor {
			t.Errorf("%q 应返回 ZeroCursor，实际 %+v", s, got)
		}
	}
}

func TestDecodeCursor_非法输入必须报错而非返回脏值(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"非 base64 字符", "!!!!"},
		{"缺冒号分隔符", base64.RawURLEncoding.EncodeToString([]byte("1700000800"))},
		{"时间戳非整数", base64.RawURLEncoding.EncodeToString([]byte("abc:42"))},
		{"ID 非整数", base64.RawURLEncoding.EncodeToString([]byte("1700000800:xyz"))},
		{"时间戳溢出 int64", base64.RawURLEncoding.EncodeToString([]byte("99999999999999999999:1"))},
		{"只有冒号", base64.RawURLEncoding.EncodeToString([]byte(":"))},
	}
	for _, c := range cases {
		got, err := DecodeCursor(c.in)
		if err == nil {
			t.Errorf("%s: 应报错，实际返回 %+v", c.name, got)
		}
		if got != ZeroCursor {
			t.Errorf("%s: 失败时应返回 ZeroCursor，实际 %+v", c.name, got)
		}
	}
}

// 宽容性：带 padding 的标准 base64url 也要能解。
// 第三方客户端（未来可能的 Android 端）未必知道我们用的是 RawURLEncoding。
func TestDecodeCursor_兼容带padding的标准base64url(t *testing.T) {
	padded := base64.URLEncoding.EncodeToString([]byte("1700000800:42"))
	got, err := DecodeCursor(padded)
	if err != nil {
		t.Fatalf("带 padding 的 base64url 应兼容: %v", err)
	}
	if got.TS != 1700000800 || got.ID != 42 {
		t.Errorf("解码得到 %+v", got)
	}
}

// 多个冒号时按第一个切分，后面的归 ID 解析（必然失败）——
// 这个行为本身不重要，重要的是「不能 panic 也不能静默返回半个值」。
func TestDecodeCursor_多个冒号不应静默取半(t *testing.T) {
	in := base64.RawURLEncoding.EncodeToString([]byte("1:2:3"))
	if _, err := DecodeCursor(in); err == nil {
		t.Error("\"1:2:3\" 的 ID 部分是 \"2:3\"，解析必须失败")
	}
}

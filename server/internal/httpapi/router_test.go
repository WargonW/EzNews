package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eznews/eznews/internal/config"
)

// 本文件钉住「路由表声明」与「实际挂载」的一致性，以及四类鉴权的判别矩阵。
//
// ★ 为什么必须测这个（这是 httpapi 层最容易出的事故）：
//   `Routes()` 是给文档和自检用的**手抄表**，`routes()` 才是真正调用的注册。
//   两份是同一个端点的两次书写 —— 后人在 `routes()` 里加一行
//   `s.route(authRequired, "GET /api/v1/me/xxx", ...)` 却忘了往 `Routes()` 里补，
//   **编译不会报错、现有测试也不会红**，只是这个端点不出现在/api 文档里。
//   反过来更危险：`Routes()` 里声明了 `authRequired`，实际注册成 `authOptional`
//   —— 一个本该只有登录用户能访问的端点就此向所有人开放，属于静默的安全回归。
//   这两类错误都只能靠本文件拦住。

// newTestServer 构造一个只带默认配置的最小 Server。
//
// 其余依赖（service / repo / db）全部为 nil：**鉴权中间件在无凭证时会短路返回 401，
// 根本不会走到 handler**，所以这里不需要真实的 service。
// 若某天 requireAuth 改成"先查库再判 401"，本文件会立即炸掉 —— 那正是我们想要告警的信号。
func newTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Deps{Config: config.Defaults()})
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}
	return s
}

// missingTokenMessage 是 RequireAuth 中间件在「无凭证」时使用的固定文案。
// 复述在这里是为了让上面的守卫能区分「中间件拦截」与「handler 自己判的业务 401」。
// ★ 若哪天改了中间的文案，这里的守卫会失效 —— 所以它下面那条测试断言、
//
//	以及 TestWrapAuth 系列一起，构成了对这条文案的双保险。
const missingTokenMessage = "缺少访问令牌"

// TestMissingTokenMessage_中间件文案未漂移
//
// 上面那条守卫依赖这个字符串，一旦中间件改了文案守卫会静默失效
// （所有端点都判成"业务 401"从而全部放行）。这里把它钉住。
func TestMissingTokenMessage_中间件文案未漂移(t *testing.T) {
	s := newTestServer(t)
	// /api/v1/me 是确定无疑的 authRequired 端点。
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("/api/v1/me 无凭证应 401，实际 %d", w.Code)
	}
	var env struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应无法解析: %v", err)
	}
	if env.Message != missingTokenMessage {
		t.Errorf("中间件文案已变更为 %q，请同步更新 missingTokenMessage 常量，否则上面的守卫会失效",
			env.Message)
	}
}

// concretePath 把 openapi 风格路径模板里的 {xxx} 换成具体值，才能真的发出请求。
func concretePath(p string) string {
	out := p
	for {
		i := strings.Index(out, "{")
		if i < 0 {
			break
		}
		j := strings.Index(out, "}")
		if j < 0 {
			break
		}
		out = out[:i] + "1" + out[j+1:]
	}
	return out
}

// TestRoutes_声明的每一个端点都必须真的挂载了
//
// 判据用 `mux.Handler(req)` 返回的 pattern：落到兜底 "/" 就说明该路径没注册。
// 直接看响应状态码是不够的 —— 未注册的路径会命中兜底返回 404，
// 而某些端点在依赖为 nil 时也会 500，两者靠状态码区分不了。
func TestRoutes_声明的每一个端点都必须真的挂载了(t *testing.T) {
	s := newTestServer(t)
	for _, rt := range s.Routes() {
		req := httptest.NewRequest(rt.Method, concretePath(rt.Path), nil)
		_, pattern := s.mux.Handler(req)
		if pattern == "/" {
			t.Errorf("声明了 %s %s 但mux 上没注册（落到了 404 兜底）", rt.Method, rt.Path)
		}
	}
}

// TestRoutes_鉴权矩阵 是核心断言：四类鉴权在无凭证时的行为必须严格区分。
func TestRoutes_鉴权矩阵(t *testing.T) {
	s := newTestServer(t)
	for _, rt := range s.Routes() {
		req := httptest.NewRequest(rt.Method, concretePath(rt.Path), nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)

		switch rt.Auth {
		case authRequired:
			// 登录才可访问的端点，无凭证必须 401。
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s 声明 required，无凭证应 401，实际 %d", rt.Method, rt.Path, w.Code)
			}
		case authAPIKey:
			// 采集器端点走 X-Api-Key，无 key 同样必须 401。
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s 声明 apikey，无凭证应 401，实际 %d", rt.Method, rt.Path, w.Code)
			}
		case authOptional, authNone:
			// ★ 关键方向：这两类**绝不能**被鉴权中间件拦下。
			//   这条守卫的是 Web 端游客体验的硬要求 —— token 过期或压根没有，
			//   阅读接口必须照常可用。
			//
			//   但不能简单地断言「不得 401」：
			//   `POST /api/v1/auth/refresh` 这类端点在没有有效凭证时返回 401 是
			//   **业务正确行为**（没有可用 refresh token 当然要拒绝），
			//   它与「中间件把端点误配成 authRequired」在状态码上完全一样。
			//   两者的唯一区别是 message —— 中间件用的是固定措辞 "缺少访问令牌"，
			//   业务 401 各有自己的文案。所以判据落到 message 上。
			if w.Code == http.StatusUnauthorized {
				var env struct {
					Message string `json:"message"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
					t.Fatalf("%s %s 的 401 响应无法解析: %v", rt.Method, rt.Path, err)
				}
				if env.Message == missingTokenMessage {
					t.Errorf("%s %s 声明 %s，却被 RequireAuth 中间件拦下（%q）",
						rt.Method, rt.Path, rt.Auth, env.Message)
				}
			}
		}
	}
}

// TestWrapAuth_未知鉴权类型必须 fail-closed
//
// 宁可 500 也不能放行：一个拼错的 authType 若被当成「不鉴权」处理，
// 效果是悄悄把私有端点公开出去，且不报错。
func TestWrapAuth_未知鉴权类型必须failClosed(t *testing.T) {
	s := newTestServer(t)
	h := s.wrapAuth("totally-unknown", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, "/whatever", nil))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("未知鉴权类型应 500 fail-closed，实际 %d", w.Code)
	}
	var env struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, w.Body.String())
	}
	if env.Code != "INTERNAL_ERROR" {
		t.Errorf("错误码应为 INTERNAL_ERROR，实际 %q", env.Code)
	}
}

// TestUnmatchedPath_返回JSON格式404 而非标准库纯文本。
// 客户端统一按 Envelope 解析响应体，纯文本会让它 JSON 解析失败。
func TestUnmatchedPath_返回JSON格式404(t *testing.T) {
	s := newTestServer(t)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/does-not-exist", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("未匹配路径应 404，实际 %d", w.Code)
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("404 必须是 JSON Envelope，实际 body=%s", w.Body.String())
	}
	if env.Code != "NOT_FOUND" {
		t.Errorf("错误码应为 NOT_FOUND，实际 %q", env.Code)
	}
}

// TestNew_nilConfig必须报错：装配期编程错误要立刻暴露，
// 而不是留到首次请求时才 panic。
func TestNew_nilConfig必须报错(t *testing.T) {
	if _, err := New(Deps{}); err == nil {
		t.Error("Config 为 nil 时必须返回错误")
	}
}

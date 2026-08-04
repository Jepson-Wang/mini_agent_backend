package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/handler"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ptr"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
	"github.com/Jepson-Wang/mini_agent_backend/internal/service"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

// fakeAuth 是 handler.AuthService 的假实现。
// 用它而不是真 service:这一层要验的是"进来的 JSON 怎么解析、出去的
// 错误怎么翻译",跟数据库和 bcrypt 没关系。
type fakeAuth struct {
	cred *service.Credential
	err  error

	gotEmail    string
	gotPassword string
	gotNickname string
}

func (f *fakeAuth) Register(_ context.Context, email, password, nickname string) (*service.Credential, error) {
	f.gotEmail, f.gotPassword, f.gotNickname = email, password, nickname
	return f.cred, f.err
}

func (f *fakeAuth) Login(_ context.Context, email, password string) (*service.Credential, error) {
	f.gotEmail, f.gotPassword = email, password
	return f.cred, f.err
}

func okCredential() *service.Credential {
	return &service.Credential{
		User: &model.User{
			ID:           7,
			Email:        "alice@example.com",
			PasswordHash: "$2a$12$SECRET_HASH_MUST_NOT_LEAK",
			Nickname:     ptr.To("爱丽丝"),
			Status:       "active",
			CreatedAt:    time.Now(),
		},
		Token:     "the.jwt.token",
		ExpiresAt: time.Now().Add(time.Hour),
	}
}

func newRouter(svc handler.AuthService) *gin.Engine {
	return handler.NewRouter(handler.Deps{Auth: svc})
}

// post 发一个 JSON 请求。body 传字符串是为了能构造非法 JSON。
func post(r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) response.Response {
	t.Helper()
	var got response.Response
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是统一外壳: %v (body=%s)", err, w.Body.String())
	}
	return got
}

// fieldsOf 取出参数校验失败时 data.fields 里的 字段 → 提示。
func fieldsOf(t *testing.T, w *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body struct {
		Data struct {
			Fields map[string]string `json:"fields"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 data.fields 失败: %v (body=%s)", err, w.Body.String())
	}
	return body.Data.Fields
}

func TestRegister_OK(t *testing.T) {
	svc := &fakeAuth{cred: okCredential()}
	r := newRouter(svc)

	w := post(r, "/api/v1/auth/register",
		`{"email":"Alice@Example.com","password":"correct horse","nickname":"爱丽丝"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	got := decode(t, w)
	if got.Code != response.CodeOK {
		t.Fatalf("code = %d, body = %s", got.Code, w.Body.String())
	}

	// handler 原样往下传,大小写规范化是 service 的事 —— 登录侧必须用同一套规则,
	// 只在一边做比两边都不做更糟(能注册进去,登录时却找不到)。
	if svc.gotEmail != "Alice@Example.com" {
		t.Errorf("传给 service 的 email = %q", svc.gotEmail)
	}
	if svc.gotNickname != "爱丽丝" {
		t.Errorf("传给 service 的 nickname = %q", svc.gotNickname)
	}

	data, _ := got.Data.(map[string]any)
	if data["access_token"] != "the.jwt.token" {
		t.Errorf("access_token = %#v", data["access_token"])
	}
	if data["token_type"] != "Bearer" {
		t.Errorf("token_type = %#v", data["token_type"])
	}
	if got.TraceID == "" {
		t.Error("trace_id 为空")
	}
}

// 成功响应里也不能有散列 —— 它就在 Credential.User 上,一个 json.Marshal
// 之差。这条盯的是 dto 转换有没有被绕过。
func TestRegister_ResponseHasNoPasswordHash(t *testing.T) {
	cred := okCredential()
	r := newRouter(&fakeAuth{cred: cred})

	w := post(r, "/api/v1/auth/register", `{"email":"a@b.com","password":"correct horse"}`)

	body := w.Body.String()
	if strings.Contains(body, cred.User.PasswordHash) || strings.Contains(body, "$2a$") {
		t.Fatalf("散列出现在响应里: %s", body)
	}
}

// 校验失败要给出 字段 → 人话,而不是 validator 的原文。
func TestRegister_ValidationErrors(t *testing.T) {
	r := newRouter(&fakeAuth{cred: okCredential()})

	cases := []struct {
		name  string
		body  string
		field string
		want  string
	}{
		{"没填邮箱", `{"password":"correct horse"}`, "email", "邮箱不能为空"},
		{"邮箱格式不对", `{"email":"not-an-email","password":"correct horse"}`, "email", "请填写正确的邮箱地址"},
		// 带前后空白的邮箱在 binding 这一步就被拒了,轮不到 service 去 TrimSpace。
		// 也就是说:去空白是客户端的责任。记一条用例是为了别在改动后才发现。
		{"邮箱带空白", `{"email":" a@b.com ","password":"correct horse"}`, "email", "请填写正确的邮箱地址"},
		{"没填密码", `{"email":"a@b.com"}`, "password", "密码不能为空"},
		{"密码太短", `{"email":"a@b.com","password":"1234"}`, "password", "密码至少 8 个字符"},
		// 30 个汉字 = 90 字节 > bcrypt 的 72。内置的 max 按字符算,拦不住。
		{"密码超字节上限",
			`{"email":"a@b.com","password":"` + strings.Repeat("口", 30) + `"}`,
			"password", "密码最长 72 字节(一个汉字算 3 字节)"},
		{"昵称太长",
			`{"email":"a@b.com","password":"correct horse","nickname":"` + strings.Repeat("字", 65) + `"}`,
			"nickname", "昵称最多 64 个字符"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := post(r, "/api/v1/auth/register", c.body)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, 期望 400 (body=%s)", w.Code, w.Body.String())
			}
			if got := decode(t, w); got.Code != response.CodeInvalidParam {
				t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeInvalidParam)
			}

			fields := fieldsOf(t, w)
			if got := fields[c.field]; got != c.want {
				t.Errorf("fields[%q] = %q, 期望 %q (全部: %v)", c.field, got, c.want, fields)
			}
		})
	}
}

// 校验失败的响应里不能出现用户填的口令,也不能出现内部命名。
func TestRegister_ValidationLeaksNothing(t *testing.T) {
	r := newRouter(&fakeAuth{cred: okCredential()})
	const password = "sh0rt" // 太短,会被 min 拦下

	w := post(r, "/api/v1/auth/register", `{"email":"bad","password":"`+password+`"}`)

	body := w.Body.String()
	if strings.Contains(body, password) {
		t.Errorf("响应回显了用户填的口令: %s", body)
	}
	for _, leak := range []string{"RegisterReq", "Error:Field validation", "binding", "validator"} {
		if strings.Contains(body, leak) {
			t.Errorf("响应泄露了内部细节 %q: %s", leak, body)
		}
	}
}

// 压根解析不了的 body 走另一条路:没有 fields,只有一句笼统的话。
func TestRegister_BadJSON(t *testing.T) {
	r := newRouter(&fakeAuth{cred: okCredential()})

	for name, body := range map[string]string{
		"语法错":    `{"email":`,
		"空 body": ``,
		"类型不对":   `{"email":123,"password":"correct horse"}`,
		"不是对象":   `[1,2,3]`,
	} {
		t.Run(name, func(t *testing.T) {
			w := post(r, "/api/v1/auth/register", body)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, 期望 400 (body=%s)", w.Code, w.Body.String())
			}
			got := decode(t, w)
			if got.Code != response.CodeInvalidParam {
				t.Fatalf("code = %d", got.Code)
			}
			if got.Message != "请求体格式不正确" {
				t.Errorf("message = %q", got.Message)
			}
			// json 包的原文带 Go 结构体名,不能漏出去。
			if strings.Contains(w.Body.String(), "RegisterReq") {
				t.Errorf("响应里出现了结构体名: %s", w.Body.String())
			}
		})
	}
}

func TestRegister_EmailOccupied(t *testing.T) {
	r := newRouter(&fakeAuth{err: model.ErrEmailOccupied})

	w := post(r, "/api/v1/auth/register", `{"email":"a@b.com","password":"correct horse"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d (body=%s)", w.Code, w.Body.String())
	}
	got := decode(t, w)
	if got.Code != response.CodeInvalidParam {
		t.Fatalf("code = %d", got.Code)
	}
	// 不能说成"该邮箱已注册",那就是个账号探测接口。
	if strings.Contains(got.Message, "已注册") || strings.Contains(got.Message, "已存在") {
		t.Errorf("文案泄露了邮箱是否已注册: %q", got.Message)
	}
}

// 没登记的错误一律 500,原文不出现在响应里。
func TestRegister_UnmappedErrorIsInternal(t *testing.T) {
	r := newRouter(&fakeAuth{err: errBoom{}})

	w := post(r, "/api/v1/auth/register", `{"email":"a@b.com","password":"correct horse"}`)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d (body=%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "pq: relation \"users\"") {
		t.Errorf("底层错误原文进了响应: %s", w.Body.String())
	}
}

type errBoom struct{}

func (errBoom) Error() string { return `pq: relation "users" does not exist` }

func TestLogin_OK(t *testing.T) {
	svc := &fakeAuth{cred: okCredential()}
	r := newRouter(svc)

	w := post(r, "/api/v1/auth/login", `{"email":"a@b.com","password":"correct horse"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	data, _ := decode(t, w).Data.(map[string]any)
	if data["access_token"] != "the.jwt.token" {
		t.Errorf("access_token = %#v", data["access_token"])
	}
	if _, ok := data["expires_at"]; !ok {
		t.Error("缺少 expires_at")
	}
}

// 登录不带 min=8:注册时的强度规则不该把老账号挡在外面。
func TestLogin_ShortPasswordStillReachesService(t *testing.T) {
	svc := &fakeAuth{err: model.ErrInvalidCredentials}
	r := newRouter(svc)

	w := post(r, "/api/v1/auth/login", `{"email":"a@b.com","password":"1234"}`)

	if svc.gotPassword != "1234" {
		t.Fatalf("短口令没有进到 service(gotPassword=%q),说明 LoginReq 上有强度校验", svc.gotPassword)
	}
	if got := decode(t, w); got.Code != response.CodeUnauthorized {
		t.Errorf("code = %d, 期望 %d —— 短口令要和口令错误返回同一个码",
			got.Code, response.CodeUnauthorized)
	}
}

func TestLogin_InvalidCredentials(t *testing.T) {
	r := newRouter(&fakeAuth{err: model.ErrInvalidCredentials})

	w := post(r, "/api/v1/auth/login", `{"email":"a@b.com","password":"whatever!"}`)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d (body=%s)", w.Code, w.Body.String())
	}
	got := decode(t, w)
	if got.Code != response.CodeUnauthorized {
		t.Fatalf("code = %d", got.Code)
	}
	// 文案不能透露是邮箱不存在还是口令错。
	for _, leak := range []string{"不存在", "未注册", "密码错误"} {
		if strings.Contains(got.Message, leak) {
			t.Errorf("文案泄露了失败原因 %q: %s", leak, got.Message)
		}
	}
}

func TestLogin_Disabled(t *testing.T) {
	r := newRouter(&fakeAuth{err: model.ErrUserDisabled})

	w := post(r, "/api/v1/auth/login", `{"email":"a@b.com","password":"correct horse"}`)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, 期望 403 (body=%s)", w.Code, w.Body.String())
	}
	if got := decode(t, w); got.Code != response.CodeForbidden {
		t.Fatalf("code = %d", got.Code)
	}
}

// 路由注册了但没注入 service,必须 500,不能 panic 也不能装作成功。
func TestAuth_NilServiceFailsClosed(t *testing.T) {
	r := newRouter(nil)

	for _, path := range []string{"/api/v1/auth/register", "/api/v1/auth/login"} {
		w := post(r, path, `{"email":"a@b.com","password":"correct horse"}`)
		if w.Code != http.StatusInternalServerError {
			t.Errorf("%s status = %d, 期望 500 (body=%s)", path, w.Code, w.Body.String())
		}
		if got := decode(t, w); got.Code != response.CodeInternal {
			t.Errorf("%s code = %d", path, got.Code)
		}
	}
}

// 用错方法要回 40500 而不是 404 —— 路径是存在的。
func TestAuth_MethodNotAllowed(t *testing.T) {
	r := newRouter(&fakeAuth{cred: okCredential()})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/login", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, 期望 405 (body=%s)", w.Code, w.Body.String())
	}
	if got := decode(t, w); got.Code != response.CodeMethodNotAllowed {
		t.Errorf("code = %d", got.Code)
	}
}

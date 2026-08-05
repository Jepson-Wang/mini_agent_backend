package middleware_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/Jepson-Wang/mini_agent_backend/internal/middleware"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/token"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

func newManager(t *testing.T) *token.Manager {
	t.Helper()
	mgr, err := token.NewManager(testSecret, "mini-agent", time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr
}

// newEngine 装一个最小的私有路由:Auth 通过后回显 MustUserID 取到的用户。
func newEngine(v middleware.TokenVerifier) *gin.Engine {
	r := gin.New()
	r.Use(middleware.TraceID())
	private := r.Group("")
	private.Use(middleware.Auth(v))
	private.GET("/me", func(c *gin.Context) {
		response.OK(c, gin.H{"uid": middleware.MustUserID(c)})
	})
	return r
}

func do(r *gin.Engine, authHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// decode 解出统一外壳。
func decode(t *testing.T, w *httptest.ResponseRecorder) response.Response {
	t.Helper()
	var got response.Response
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是统一外壳: %v (body=%s)", err, w.Body.String())
	}
	return got
}

func TestAuth_ValidToken(t *testing.T) {
	mgr := newManager(t)
	r := newEngine(mgr)

	tok, _, err := mgr.Issue(42)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	w := do(r, "Bearer "+tok)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	got := decode(t, w)
	if got.Code != response.CodeOK {
		t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeOK)
	}
	// userID 确实进了 c.Set,handler 能取到。
	data, ok := got.Data.(map[string]any)
	if !ok {
		t.Fatalf("data = %#v", got.Data)
	}
	if uid, _ := data["uid"].(float64); int64(uid) != 42 {
		t.Fatalf("uid = %#v, 期望 42", data["uid"])
	}
	if got.TraceID == "" {
		t.Error("trace_id 为空")
	}
}

// bearer 前缀按 RFC 6750 大小写不敏感。
func TestAuth_BearerPrefixCaseInsensitive(t *testing.T) {
	mgr := newManager(t)
	r := newEngine(mgr)

	tok, _, err := mgr.Issue(1)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	for _, prefix := range []string{"Bearer ", "bearer ", "BEARER ", "BeArEr "} {
		if w := do(r, prefix+tok); w.Code != http.StatusOK {
			t.Errorf("前缀 %q 被拒: status=%d", prefix, w.Code)
		}
	}
}

func TestAuth_RejectsBadHeaders(t *testing.T) {
	mgr := newManager(t)
	r := newEngine(mgr)

	tok, _, err := mgr.Issue(1)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	cases := map[string]string{
		"没有头":       "",
		"只有令牌没前缀":   tok,
		"前缀不对":      "Token " + tok,
		"只有 Bearer": "Bearer",
		"前缀后是空的":    "Bearer    ",
		"Basic 认证":  "Basic dXNlcjpwYXNz",
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			w := do(r, header)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, 期望 401 (body=%s)", w.Code, w.Body.String())
			}
			if got := decode(t, w); got.Code != response.CodeUnauthorized {
				t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeUnauthorized)
			}
		})
	}
}

func TestAuth_RejectsBadTokens(t *testing.T) {
	mgr := newManager(t)
	r := newEngine(mgr)

	// 过期的。直接手签而不是拿一个极短 TTL 的 Manager ——
	// 校验侧留了 30s 时钟偏差,刚过期的令牌是会被放行的。
	past := time.Now().Add(-2 * time.Hour)
	expired, err := jwt.NewWithClaims(jwt.SigningMethodHS256, token.Claims{
		UID: 1,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "mini-agent",
			IssuedAt:  jwt.NewNumericDate(past),
			ExpiresAt: jwt.NewNumericDate(past.Add(time.Minute)),
		},
	}).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("构造过期令牌: %v", err)
	}

	// 别人用另一个密钥签的
	forgedMgr, err := token.NewManager("ffffffffffffffffffffffffffffffff", "mini-agent", time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	forged, _, err := forgedMgr.Issue(1)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	cases := map[string]string{
		"过期":   expired,
		"签名不对": forged,
		"不是令牌": "not-a-jwt",
		"段数不对": "aaa.bbb",
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			w := do(r, "Bearer "+tok)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, 期望 401 (body=%s)", w.Code, w.Body.String())
			}
			got := decode(t, w)
			if got.Code != response.CodeUnauthorized {
				t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeUnauthorized)
			}
			// 失败原因不能回给客户端 —— 区分"签名不对"和"过期"等于帮人调试伪造。
			lower := strings.ToLower(got.Message)
			for _, leak := range []string{"signature", "expired", "malformed", "jwt", "token"} {
				if strings.Contains(lower, leak) {
					t.Errorf("响应文案泄露了失败细节 %q: %s", leak, got.Message)
				}
			}
		})
	}
}

// 路由挂了 Auth 却没注入实现,必须 fail closed,绝不能放行。
func TestAuth_NilVerifierFailsClosed(t *testing.T) {
	r := newEngine(nil)

	mgr := newManager(t)
	tok, _, err := mgr.Issue(1)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	w := do(r, "Bearer "+tok)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, 期望 500 —— 未注入 Verifier 时不能放行", w.Code)
	}
	if got := decode(t, w); got.Code != response.CodeInternal {
		t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeInternal)
	}
}

// 被拒的请求不能进到 handler。Auth 里漏了 Abort 的话这条会挂。
func TestAuth_AbortsBeforeHandler(t *testing.T) {
	reached := false
	r := gin.New()
	r.Use(middleware.TraceID())
	private := r.Group("")
	private.Use(middleware.Auth(newManager(t)))
	private.GET("/me", func(c *gin.Context) {
		reached = true
		c.Status(http.StatusOK)
	})

	do(r, "Bearer garbage")
	if reached {
		t.Fatal("令牌非法却仍然进了 handler")
	}
}

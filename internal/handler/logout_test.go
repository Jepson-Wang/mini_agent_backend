package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/handler"
	"github.com/Jepson-Wang/mini_agent_backend/internal/middleware"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
)

// 登出接口的测试。
//
// 它挂在**私有路由组**上(走 middleware.Auth),因为要知道当前是哪个令牌
// 才能吊销它。

// logoutVerifier 是能返回完整声明的假校验器。
type logoutVerifier struct {
	uid int64
	jti string
	exp time.Time
}

func (v *logoutVerifier) Verify(context.Context, string) (int64, error) {
	return v.uid, nil
}

func (v *logoutVerifier) VerifyClaims(context.Context, string) (*middleware.TokenClaims, error) {
	return &middleware.TokenClaims{
		UserID:    v.uid,
		JTI:       v.jti,
		ExpiresAt: v.exp,
	}, nil
}

// newLogoutRouter 装一个带登出接口的路由。
func newLogoutRouter(svc handler.AuthService, v middleware.TokenVerifier) *gin.Engine {
	r := gin.New()
	r.Use(middleware.TraceID())
	private := r.Group("")
	private.Use(middleware.Auth(v, nil))
	private.POST("/api/v1/auth/logout", handler.NewAuthHandler(svc).Logout)
	return r
}

// postLogout 发一个登出请求。
func postLogout(r *gin.Engine, authHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout",
		bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// 登出要吊销**当前这个令牌**。
//
// 传下去的是 jti 和 exp —— exp 是必需的:黑名单的 TTL 取 exp - now。
func TestLogout_RevokesCurrentToken(t *testing.T) {
	auth := &fakeAuth{}
	exp := time.Now().Add(time.Hour)
	v := &logoutVerifier{uid: 42, jti: "JTI-LOGOUT-1", exp: exp}
	r := newLogoutRouter(auth, v)

	w := postLogout(r, "Bearer sometoken")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, 期望 200 (body=%s)", w.Code, w.Body.String())
	}
	if auth.gotLogoutJTI != "JTI-LOGOUT-1" {
		t.Errorf("吊销的 jti = %q,期望 %q", auth.gotLogoutJTI, "JTI-LOGOUT-1")
	}
	if !auth.gotLogoutExp.Equal(exp) {
		t.Errorf("吊销的 exp = %v,期望 %v", auth.gotLogoutExp, exp)
	}
}

// 没有令牌不能登出 —— 挂在私有组上就该走鉴权。
func TestLogout_RequiresToken(t *testing.T) {
	auth := &fakeAuth{}
	r := newLogoutRouter(auth, &logoutVerifier{uid: 42, jti: "JTI-1"})

	w := postLogout(r, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, 期望 401", w.Code)
	}
	if auth.gotLogoutJTI != "" {
		t.Error("没通过鉴权就不该吊销任何东西")
	}
}

// 吊销失败要报错 —— 不能假装成功。
//
// 前端需要知道这次登出没生效,否则它清掉本地 token 后用户会以为
// 已经安全了,而服务端的令牌还能用。
func TestLogout_RevokeErrorReported(t *testing.T) {
	auth := &fakeAuth{logoutErr: errors.New("redis 连不上")}
	r := newLogoutRouter(auth, &logoutVerifier{uid: 42, jti: "JTI-1"})

	w := postLogout(r, "Bearer sometoken")
	if w.Code == http.StatusOK {
		t.Fatal("吊销失败不能回 200")
	}
	if got := decode(t, w); got.Code == response.CodeOK {
		t.Errorf("code = %d, 期望不是 OK", got.Code)
	}
}

// 响应不能泄露吊销失败的细节。
func TestLogout_ErrorDoesNotLeak(t *testing.T) {
	auth := &fakeAuth{logoutErr: errors.New("dial tcp 10.0.0.7:6379: connection refused")}
	r := newLogoutRouter(auth, &logoutVerifier{uid: 42, jti: "JTI-1"})

	w := postLogout(r, "Bearer sometoken")

	body := w.Body.String()
	lower := strings.ToLower(body)
	for _, leak := range []string{"10.0.0.7", "6379", "redis", "dial tcp"} {
		if strings.Contains(lower, leak) {
			t.Errorf("响应体泄露了内部细节 %q: %s", leak, body)
		}
	}
}

// 响应是统一外壳。
func TestLogout_ReturnsEnvelope(t *testing.T) {
	auth := &fakeAuth{}
	r := newLogoutRouter(auth, &logoutVerifier{uid: 42, jti: "JTI-1"})

	w := postLogout(r, "Bearer sometoken")
	var env struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		TraceID string `json:"trace_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是统一外壳: %v", err)
	}
	if env.Code != response.CodeOK {
		t.Errorf("code = %d,期望 %d", env.Code, response.CodeOK)
	}
	if env.TraceID == "" {
		t.Error("trace_id 为空")
	}
}

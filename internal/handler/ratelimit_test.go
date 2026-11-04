// ratelimit_test.go 只测限流中间件在路由上的接线:挂在哪、不挂在哪、
// Limiter 为 nil 时是否真的什么都不挂。限流器本身的判定逻辑在 middleware 和 pkg/ratelimit 的测试里。
//
// 本文件的假实现全部以 rl 开头,避免和同目录(同一个 handler_test 包)里
// 其它测试文件的名字撞车。
package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Jepson-Wang/mini_agent_backend/internal/handler"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ratelimit"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
	"github.com/Jepson-Wang/mini_agent_backend/internal/service"
)

const rlUID = "01J8ZQXK7P0000000000000000"

// rlRejectAll 是永远拒绝的限流器,同时记下被调用了几次。
type rlRejectAll struct{ calls int }

func (l *rlRejectAll) Allow(context.Context, []ratelimit.Bucket) (ratelimit.Result, error) {
	l.calls++
	return ratelimit.Result{Allowed: false, RetryAfter: time.Second, Scope: "ip"}, nil
}

type rlAuth struct{}

func (rlAuth) Register(context.Context, string, string, string) (*service.Credential, error) {
	return &service.Credential{User: &model.User{ID: 1}, Token: "t", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (rlAuth) Login(context.Context, string, string) (*service.Credential, error) {
	return &service.Credential{User: &model.User{ID: 1}, Token: "t", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

type rlVerifier struct{}

func (rlVerifier) Verify(context.Context, string) (int64, error) { return 7, nil }

type rlSession struct{}

func (rlSession) CreateWithFirstTask(context.Context, int64, *string, *string) (*model.Session, *model.Task, error) {
	return &model.Session{}, nil, nil
}

func (rlSession) ListSessionsByUserID(context.Context, int64, int, int) ([]model.Session, error) {
	return nil, nil
}

func (rlSession) SessionDetail(context.Context, int64, string) (*model.Session, error) {
	return &model.Session{}, nil
}

func (rlSession) ModifyTheTitle(context.Context, int64, string, *string) (*model.Session, error) {
	return &model.Session{}, nil
}

func (rlSession) SoftDelete(context.Context, int64, string) error { return nil }

func (rlSession) CreateTaskWithSessionID(context.Context, int64, string, *string) (*model.Task, error) {
	return &model.Task{}, nil
}

func (rlSession) ListTasksWithSessionID(context.Context, int64, string, int, int) ([]model.Task, error) {
	return nil, nil
}

type rlTask struct{}

func (rlTask) GetTaskDetailByTaskID(context.Context, int64, string) (*model.Task, error) {
	return &model.Task{}, nil
}

func (rlTask) Cancel(context.Context, int64, string, string) (*model.Task, error) {
	return &model.Task{}, nil
}

// rlProber 是 handler.RedisProber 的假实现。
type rlProber struct{ err error }

func (p rlProber) Ping(context.Context) error { return p.err }

// rlDo 发一个请求并返回响应码和业务码。
func rlDo(r *gin.Engine, method, path, body string) (int, int) {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer whatever")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, rlBizCode(w)
}

func rlBizCode(w *httptest.ResponseRecorder) int {
	var resp response.Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		return -1
	}
	return resp.Code
}

func rlFullDeps(l *rlRejectAll) handler.Deps {
	d := handler.Deps{
		Auth:     rlAuth{},
		Verifier: rlVerifier{},
		Session:  rlSession{},
		Task:     rlTask{},
	}
	if l != nil {
		d.Limiter = l
	}
	return d
}

const rlLoginBody = `{"email":"a@example.com","password":"password123"}`
const rlRegisterBody = `{"email":"a@example.com","password":"password123","nickname":"a"}`

func TestRouter_NoLimiterStillServesLogin(t *testing.T) {
	r := handler.NewRouter(handler.Deps{Auth: rlAuth{}})
	status, code := rlDo(r, http.MethodPost, "/api/v1/auth/login", rlLoginBody)
	if code == response.CodeRateLimited {
		t.Fatalf("没有 Limiter 时不该出现 42901")
	}
	if status != http.StatusOK {
		t.Errorf("状态码 = %d,期望 200", status)
	}
}

// 回归守卫:已有的多请求测试(如 TestRegister_ValidationErrors 一个 router 连发七次)
// 全部共用 httptest 写死的同一个 RemoteAddr。Limiter 为 nil 时若仍挂了中间件,它们会被挤爆。
func TestRouter_NoLimiterManyRequestsAllSucceed(t *testing.T) {
	r := handler.NewRouter(handler.Deps{Auth: rlAuth{}})
	for i := 0; i < 10; i++ {
		_, code := rlDo(r, http.MethodPost, "/api/v1/auth/register", rlRegisterBody)
		if code == response.CodeRateLimited {
			t.Fatalf("第 %d 次请求被限流,Limiter 为 nil 时不该挂限流中间件", i+1)
		}
	}
}

func TestRouter_AuthRateLimitMountedOnPublicGroup(t *testing.T) {
	l := &rlRejectAll{}
	r := handler.NewRouter(rlFullDeps(l))

	for _, p := range []struct{ path, body string }{
		{"/api/v1/auth/login", rlLoginBody},
		{"/api/v1/auth/register", rlRegisterBody},
	} {
		status, code := rlDo(r, http.MethodPost, p.path, p.body)
		if code != response.CodeRateLimited {
			t.Errorf("%s 业务码 = %d,期望 42901", p.path, code)
		}
		if status != http.StatusTooManyRequests {
			t.Errorf("%s 状态码 = %d,期望 429", p.path, status)
		}
	}
}

func TestRouter_ChatRateLimitOnlyOnCreatingRoutes(t *testing.T) {
	r := handler.NewRouter(rlFullDeps(&rlRejectAll{}))
	sid := "/api/v1/sessions/" + rlUID

	limited := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/sessions", `{"input":"hi"}`},
		{http.MethodPost, sid + "/tasks", `{"input":"hi"}`},
	}
	for _, c := range limited {
		if _, code := rlDo(r, c.method, c.path, c.body); code != response.CodeRateLimited {
			t.Errorf("%s %s 业务码 = %d,期望 42901", c.method, c.path, code)
		}
	}

	unlimited := []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/sessions", ""},
		{http.MethodGet, "/api/v1/tasks/" + rlUID, ""},
		{http.MethodPost, "/api/v1/tasks/" + rlUID + "/cancel", ""},
	}
	for _, c := range unlimited {
		if _, code := rlDo(r, c.method, c.path, c.body); code == response.CodeRateLimited {
			t.Errorf("%s %s 被限流了,只有建任务的两条路由该限流", c.method, c.path)
		}
	}
}

// append 别名守卫:两条建任务路由必须各自拥有完整的 [限流, handler] 链。
// 若共用一个切片再 append,后注册的会覆盖先注册的末尾元素。
func TestRouter_ChatRateLimitChainsBothRoutes(t *testing.T) {
	l := &rlRejectAll{}
	r := handler.NewRouter(rlFullDeps(l))

	for _, path := range []string{
		"/api/v1/sessions",
		"/api/v1/sessions/" + rlUID + "/tasks",
	} {
		before := l.calls
		if _, code := rlDo(r, http.MethodPost, path, `{"input":"hi"}`); code != response.CodeRateLimited {
			t.Errorf("POST %s 业务码 = %d,期望 42901", path, code)
		}
		if l.calls != before+1 {
			t.Errorf("POST %s 限流器被调用 %d 次,期望恰好 1 次", path, l.calls-before)
		}
	}
}

func TestRouter_HealthNeedsNoLimiter(t *testing.T) {
	r := handler.NewRouter(handler.Deps{})
	status, code := rlDo(r, http.MethodGet, "/health", "")
	if status != http.StatusOK || code != 0 {
		t.Errorf("/health status=%d code=%d,期望 200 / 0", status, code)
	}
}

// 带限流器时 /health 也不该被限流(它不在 public 组里)。
func TestRouter_HealthNotLimitedWithLimiter(t *testing.T) {
	r := handler.NewRouter(rlFullDeps(&rlRejectAll{}))
	if _, code := rlDo(r, http.MethodGet, "/health", ""); code == response.CodeRateLimited {
		t.Errorf("/health 被限流了")
	}
}

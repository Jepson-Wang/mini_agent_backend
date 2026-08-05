package handler_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Jepson-Wang/mini_agent_backend/internal/handler"
	"github.com/Jepson-Wang/mini_agent_backend/internal/middleware"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/response"
)

const callbackPath = "/internal/tasks/" + sampleUID + "/callback"

// withSecret 带上正确的共享密钥。
func withSecret() map[string]string {
	return map[string]string{middleware.InternalTokenHeader: internalSecret}
}

func TestCallback_OK(t *testing.T) {
	svc := &fakeTask{applied: true}
	w := do(internalRouter(svc), callbackPath, `{"status":"succeeded"}`, withSecret())

	got := decode(t, w)
	if got.Code != response.CodeOK {
		t.Fatalf("code = %d, body=%s", got.Code, w.Body.String())
	}
	if svc.gotUID != sampleUID || svc.gotStatus != model.TaskSucceeded {
		t.Errorf("service 收到 uid=%q status=%q", svc.gotUID, svc.gotStatus)
	}
	if !strings.Contains(w.Body.String(), `"applied":true`) {
		t.Errorf("响应里应当带 applied: %s", w.Body.String())
	}
}

// 没生效也回 200。回调方是机器,给它 4xx/5xx 只会换来无限重试,
// 而"任务已经是终态"重试多少次都是同一个结果。
func TestCallback_NotAppliedIsStill200(t *testing.T) {
	w := do(internalRouter(&fakeTask{applied: false}), callbackPath, `{"status":"succeeded"}`, withSecret())

	got := decode(t, w)
	if got.Code != response.CodeOK {
		t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeOK)
	}
	if w.Code != http.StatusOK {
		t.Errorf("HTTP 状态 = %d, 期望 200", w.Code)
	}
	// 但必须能看出来没生效,否则排查时和成功推进长得一样。
	if !strings.Contains(w.Body.String(), `"applied":false`) {
		t.Errorf("响应里应当带 applied:false: %s", w.Body.String())
	}
}

// 这条是这个接口最重要的一条:Agent 不许把任务改成"已取消"。
// 放开的话,一个被攻破的 Agent 就能悄悄吞掉任务 ——
// 用户看到的是"自己取消的",根本不会去查。
func TestCallback_RejectsCanceled(t *testing.T) {
	svc := &fakeTask{applied: true}
	w := do(internalRouter(svc), callbackPath, `{"status":"canceled"}`, withSecret())

	if got := decode(t, w); got.Code != response.CodeInvalidParam {
		t.Fatalf("code = %d, 期望 %d (body=%s)", got.Code, response.CodeInvalidParam, w.Body.String())
	}
	if svc.gotStatus != "" {
		t.Errorf("canceled 不该走到 service,实际收到 %q", svc.gotStatus)
	}
}

func TestCallback_RejectsUnknownStatus(t *testing.T) {
	for _, body := range []string{
		`{"status":"done"}`,
		`{"status":""}`,
		`{}`,
		`{"status":"SUCCEEDED"}`, // oneof 区分大小写
	} {
		svc := &fakeTask{applied: true}
		w := do(internalRouter(svc), callbackPath, body, withSecret())
		if got := decode(t, w); got.Code != response.CodeInvalidParam {
			t.Errorf("body=%s: code = %d, 期望 %d", body, got.Code, response.CodeInvalidParam)
		}
		if svc.gotStatus != "" {
			t.Errorf("body=%s: 不该走到 service", body)
		}
	}
}

// error_msg 只在 failed 时留下,而且空白串归一成 nil ——
// 否则库里会同时存在 NULL 和 "" 两种"没有原因"。
func TestCallback_NormalizesErrorMsg(t *testing.T) {
	for _, tc := range []struct {
		body string
		want *string
	}{
		{`{"status":"failed","error_msg":"模型超时"}`, strPtr("模型超时")},
		{`{"status":"failed","error_msg":"  模型超时  "}`, strPtr("模型超时")},
		{`{"status":"failed","error_msg":"   "}`, nil},
		{`{"status":"failed"}`, nil},
		// 成功的任务不该留着一条失败原因。
		{`{"status":"succeeded","error_msg":"不该被写进去"}`, nil},
	} {
		svc := &fakeTask{applied: true}
		do(internalRouter(svc), callbackPath, tc.body, withSecret())

		switch {
		case tc.want == nil && svc.gotErrMsg != nil:
			t.Errorf("body=%s: 期望 nil, 实际 %q", tc.body, *svc.gotErrMsg)
		case tc.want != nil && svc.gotErrMsg == nil:
			t.Errorf("body=%s: 期望 %q, 实际 nil", tc.body, *tc.want)
		case tc.want != nil && *svc.gotErrMsg != *tc.want:
			t.Errorf("body=%s: 期望 %q, 实际 %q", tc.body, *tc.want, *svc.gotErrMsg)
		}
	}
}

// error_msg 会原样落进 tasks.error_msg(text,数据库自己不设上限)。
// 不挡的话对面一次能往库里塞几 MB。
func TestCallback_RejectsHugeErrorMsg(t *testing.T) {
	svc := &fakeTask{applied: true}
	body := `{"status":"failed","error_msg":"` + strings.Repeat("x", 2001) + `"}`
	w := do(internalRouter(svc), callbackPath, body, withSecret())

	if got := decode(t, w); got.Code != response.CodeInvalidParam {
		t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeInvalidParam)
	}
	if svc.gotStatus != "" {
		t.Error("超长的 error_msg 不该走到 service")
	}
}

// ---- 共享密钥 ----

func TestCallback_RequiresSecret(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"没带", nil},
		{"带错了", map[string]string{middleware.InternalTokenHeader: "wrong-but-same-length-000000000000"}},
		{"空串", map[string]string{middleware.InternalTokenHeader: ""}},
		// 这个头是给用户令牌用的,不能拿来顶内部密钥。
		{"塞进 Authorization", map[string]string{"Authorization": "Bearer " + internalSecret}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeTask{applied: true}
			w := do(internalRouter(svc), callbackPath, `{"status":"succeeded"}`, tc.headers)

			if got := decode(t, w); got.Code != response.CodeUnauthorized {
				t.Fatalf("code = %d, 期望 %d (body=%s)", got.Code, response.CodeUnauthorized, w.Body.String())
			}
			if svc.gotStatus != "" {
				t.Error("鉴权失败的请求不该走到 service")
			}
		})
	}
}

// 漏配密钥必须全拒,不能放行。这条路径能直接改任务状态,
// 放行等于把它开给全世界。
func TestCallback_NoSecretConfiguredFailsClosed(t *testing.T) {
	svc := &fakeTask{applied: true}
	r := handler.NewRouter(handler.Deps{TaskCallback: svc}) // InternalToken 为空

	w := do(r, callbackPath, `{"status":"succeeded"}`, withSecret())

	if got := decode(t, w); got.Code != response.CodeInternal {
		t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeInternal)
	}
	if svc.gotStatus != "" {
		t.Error("未配置密钥时不该走到 service")
	}
}

// 内部路由不该因为没有用户令牌就 401 —— 它压根不走用户鉴权。
// 反过来说,带了用户令牌也不能当内部密钥用(上面 TestCallback_RequiresSecret 盯着)。
func TestCallback_DoesNotRequireUserToken(t *testing.T) {
	svc := &fakeTask{applied: true}
	// Verifier 为 nil:私有组会一律 500,内部组不该受影响。
	r := handler.NewRouter(handler.Deps{TaskCallback: svc, InternalToken: internalSecret})

	w := do(r, callbackPath, `{"status":"running"}`, withSecret())

	if got := decode(t, w); got.Code != response.CodeOK {
		t.Fatalf("code = %d, body=%s", got.Code, w.Body.String())
	}
}

func TestCallback_NilServiceFailsClosed(t *testing.T) {
	r := handler.NewRouter(handler.Deps{InternalToken: internalSecret}) // TaskCallback 为 nil
	w := do(r, callbackPath, `{"status":"succeeded"}`, withSecret())

	if got := decode(t, w); got.Code != response.CodeInternal {
		t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeInternal)
	}
}

func TestCallback_BadUID(t *testing.T) {
	svc := &fakeTask{applied: true}
	w := do(internalRouter(svc), "/internal/tasks/short/callback", `{"status":"succeeded"}`, withSecret())

	if got := decode(t, w); got.Code != response.CodeInvalidParam {
		t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeInvalidParam)
	}
}

// 数据库挂了这种才值得让对面重试。
func TestCallback_RealErrorIs500(t *testing.T) {
	raw := "pq: connection refused host=10.0.0.1 password=hunter2"
	w := do(internalRouter(&fakeTask{err: errors.New(raw)}), callbackPath, `{"status":"succeeded"}`, withSecret())

	got := decode(t, w)
	if got.Code != response.CodeInternal {
		t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeInternal)
	}
	for _, leak := range []string{"password", "hunter2", "10.0.0.1"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("响应里出现了 %q: %s", leak, w.Body.String())
		}
	}
}

func strPtr(s string) *string { return &s }

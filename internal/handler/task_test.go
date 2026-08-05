package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
)

// 一个合法的 26 位 ULID,和 tasks.task_uid 的 char(26) 对齐。
const sampleUID = "01J8ZQXK7P0000000000000000"

const internalSecret = "0123456789abcdef0123456789abcdef" // 32 字节

// fakeTokenVerifier 让私有路由组能放行。这一层不验 JWT 的真伪,
// 那是 pkg/token 和 middleware 的事。
type fakeTokenVerifier struct{ userID int64 }

func (f fakeTokenVerifier) Verify(_ context.Context, _ string) (int64, error) {
	return f.userID, nil
}

// fakeTask 是 handler.TaskService 和 handler.TaskCallbackService 的假实现。
type fakeTask struct {
	task    *model.Task
	err     error
	applied bool

	gotUserID  int64
	gotUID     string
	gotStatus  string
	gotErrMsg  *string
	cancelCall int
}

func (f *fakeTask) GetTaskDetailByTaskID(_ context.Context, userID int64, taskUID string) (*model.Task, error) {
	f.gotUserID, f.gotUID = userID, taskUID
	return f.task, f.err
}

func (f *fakeTask) Cancel(_ context.Context, userID int64, taskUID string) (*model.Task, error) {
	f.cancelCall++
	f.gotUserID, f.gotUID = userID, taskUID
	return f.task, f.err
}

func (f *fakeTask) HandleCallback(_ context.Context, taskUID, status string, errMsg *string) (bool, error) {
	f.gotUID, f.gotStatus, f.gotErrMsg = taskUID, status, errMsg
	return f.applied, f.err
}

func sampleTask() *model.Task {
	now := time.Now()
	return &model.Task{
		ID:         42, // 不该出现在响应里
		TaskUID:    sampleUID,
		SessionID:  "01J8ZQXK7P1111111111111111",
		UserID:     7, // 同样不该出现在响应里
		Status:     model.TaskCanceled,
		Input:      ptr.To("帮我订张票"),
		StartedAt:  &now,
		FinishedAt: &now,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

func taskRouter(svc handler.TaskService) *gin.Engine {
	return handler.NewRouter(handler.Deps{
		Task:     svc,
		Verifier: fakeTokenVerifier{userID: 7},
	})
}

func internalRouter(svc handler.TaskCallbackService) *gin.Engine {
	return handler.NewRouter(handler.Deps{
		TaskCallback:  svc,
		InternalToken: internalSecret,
	})
}

// do 发一个带自定义头的 JSON 请求。
func do(r *gin.Engine, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func authed(r *gin.Engine, path string) *httptest.ResponseRecorder {
	return do(r, path, "", map[string]string{"Authorization": "Bearer whatever"})
}

// ---- POST /api/v1/tasks/:uid/cancel ----

func TestCancel_OK(t *testing.T) {
	svc := &fakeTask{task: sampleTask()}
	w := authed(taskRouter(svc), "/api/v1/tasks/"+sampleUID+"/cancel")

	got := decode(t, w)
	if got.Code != response.CodeOK {
		t.Fatalf("code = %d, body=%s", got.Code, w.Body.String())
	}
	if svc.gotUID != sampleUID {
		t.Errorf("service 收到的 uid = %q", svc.gotUID)
	}
	// 用户身份必须来自令牌,不能来自请求体或路径。
	if svc.gotUserID != 7 {
		t.Errorf("service 收到的 userID = %d, 期望来自令牌的 7", svc.gotUserID)
	}
}

// 自增主键和 user_id 不该出现在响应里。
func TestCancel_RespHidesInternalIDs(t *testing.T) {
	w := authed(taskRouter(&fakeTask{task: sampleTask()}), "/api/v1/tasks/"+sampleUID+"/cancel")

	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	for _, k := range []string{"id", "ID", "user_id", "UserID"} {
		if _, ok := body.Data[k]; ok {
			t.Errorf("响应里出现了 %q: %s", k, w.Body.String())
		}
	}
	if body.Data["task_uid"] != sampleUID {
		t.Errorf("缺少 task_uid: %s", w.Body.String())
	}
}

// 没带令牌就该被 Auth 中间件拦下,service 一次都不该被调用。
func TestCancel_RequiresAuth(t *testing.T) {
	svc := &fakeTask{task: sampleTask()}
	w := do(taskRouter(svc), "/api/v1/tasks/"+sampleUID+"/cancel", "", nil)

	if got := decode(t, w); got.Code != response.CodeUnauthorized {
		t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeUnauthorized)
	}
	if svc.cancelCall != 0 {
		t.Errorf("未认证的请求不该走到 service,实际调了 %d 次", svc.cancelCall)
	}
}

// 「任务不存在」和「不是你的任务」在 service 那边就被归成了同一个
// model.ErrTaskNotFound(见 TaskService.mustOwn)。这条盯的是 handler
// 这一端:它必须原样回 40400,不能顺手改写成 40300 之类能区分出来的码 ——
// 一旦能区分,这个接口就成了 task_uid 探测器。
//
// 而且被 %w 包过一层也得照样命中:errRules 用 errors.Is 匹配就是为了这个。
func TestCancel_NotFoundIsOpaque(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"裸的", model.ErrTaskNotFound},
		{"包过一层", fmt.Errorf("service: 查任务失败: %w", model.ErrTaskNotFound)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := authed(taskRouter(&fakeTask{err: tc.err}), "/api/v1/tasks/"+sampleUID+"/cancel")

			got := decode(t, w)
			if got.Code != response.CodeNotFound {
				t.Fatalf("code = %d, 期望 %d (body=%s)", got.Code, response.CodeNotFound, w.Body.String())
			}
			// 文案里不能出现任何暗示"存在但不属于你"的词。
			for _, leak := range []string{"权限", "属于", "无权", "他人"} {
				if strings.Contains(got.Message, leak) {
					t.Errorf("文案泄露了归属信息: %q", got.Message)
				}
			}
		})
	}
}

func TestCancel_AlreadyFinished(t *testing.T) {
	svc := &fakeTask{err: model.ErrTaskNotCancelable}
	w := authed(taskRouter(svc), "/api/v1/tasks/"+sampleUID+"/cancel")

	got := decode(t, w)
	if got.Code != response.CodeConflict {
		t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeConflict)
	}
	if w.Code != http.StatusConflict {
		t.Errorf("HTTP 状态 = %d, 期望 409", w.Code)
	}
}

func TestCancel_BadUID(t *testing.T) {
	svc := &fakeTask{task: sampleTask()}
	for _, uid := range []string{
		"short",
		strings.Repeat("A", 27),
		strings.Repeat("A", 4096), // 别让这种东西走到数据库
	} {
		w := authed(taskRouter(svc), "/api/v1/tasks/"+uid+"/cancel")
		if got := decode(t, w); got.Code != response.CodeInvalidParam {
			t.Errorf("uid=%.20q: code = %d, 期望 %d", uid, got.Code, response.CodeInvalidParam)
		}
	}
	if svc.cancelCall != 0 {
		t.Errorf("格式不对的 uid 不该走到 service,实际调了 %d 次", svc.cancelCall)
	}
}

// 没登记的错误一律 50000,原文不进响应体 —— 数据库报错里可能带表名和连接串。
func TestCancel_UnknownErrorLeaksNothing(t *testing.T) {
	raw := "pq: relation \"tasks\" does not exist, host=10.0.0.1 password=hunter2"
	w := authed(taskRouter(&fakeTask{err: errors.New(raw)}), "/api/v1/tasks/"+sampleUID+"/cancel")

	got := decode(t, w)
	if got.Code != response.CodeInternal {
		t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeInternal)
	}
	for _, leak := range []string{"tasks", "password", "hunter2", "10.0.0.1"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("响应里出现了 %q: %s", leak, w.Body.String())
		}
	}
}

// 依赖没注入时要 500,不能 404,也不能 panic。
func TestCancel_NilServiceFailsClosed(t *testing.T) {
	r := handler.NewRouter(handler.Deps{Verifier: fakeTokenVerifier{userID: 7}}) // Task 为 nil
	w := authed(r, "/api/v1/tasks/"+sampleUID+"/cancel")

	if got := decode(t, w); got.Code != response.CodeInternal {
		t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeInternal)
	}
}

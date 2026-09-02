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

const sampleSID = "01J8ZQXK7P1111111111111111"

// fakeSession 是 handler.SessionService 的假实现,记下收到的每个参数。
type fakeSession struct {
	sess  *model.Session
	task  *model.Task
	list  []model.Session
	tasks []model.Task
	err   error

	gotUserID int64
	gotSID    string
	gotTitle  *string
	gotInput  *string
	gotLimit  int
	gotOffset int
	calls     int
}

func (f *fakeSession) CreateWithFirstTask(_ context.Context, userID int64, title, input *string) (*model.Session, *model.Task, error) {
	f.calls++
	f.gotUserID, f.gotTitle, f.gotInput = userID, title, input
	return f.sess, f.task, f.err
}

func (f *fakeSession) ListSessionsByUserID(_ context.Context, userID int64, limit, offset int) ([]model.Session, error) {
	f.calls++
	f.gotUserID, f.gotLimit, f.gotOffset = userID, limit, offset
	return f.list, f.err
}

func (f *fakeSession) SessionDetail(_ context.Context, userID int64, sessionID string) (*model.Session, error) {
	f.calls++
	f.gotUserID, f.gotSID = userID, sessionID
	return f.sess, f.err
}

func (f *fakeSession) ModifyTheTitle(_ context.Context, userID int64, sessionID string, title *string) (*model.Session, error) {
	f.calls++
	f.gotUserID, f.gotSID, f.gotTitle = userID, sessionID, title
	return f.sess, f.err
}

func (f *fakeSession) SoftDelete(_ context.Context, userID int64, sessionID string) error {
	f.calls++
	f.gotUserID, f.gotSID = userID, sessionID
	return f.err
}

func (f *fakeSession) CreateTaskWithSessionID(_ context.Context, userID int64, sessionID string, input *string) (*model.Task, error) {
	f.calls++
	f.gotUserID, f.gotSID, f.gotInput = userID, sessionID, input
	return f.task, f.err
}

func (f *fakeSession) ListTasksWithSessionID(_ context.Context, userID int64, sessionID string, limit, offset int) ([]model.Task, error) {
	f.calls++
	f.gotUserID, f.gotSID, f.gotLimit, f.gotOffset = userID, sessionID, limit, offset
	return f.tasks, f.err
}

func sampleSession() *model.Session {
	now := time.Now()
	return &model.Session{
		SessionID: sampleSID,
		UserID:    7, // 不该出现在响应里
		Status:    "active",
		Title:     ptr.To("订机票"),
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func sessionRouter(svc handler.SessionService) *gin.Engine {
	return handler.NewRouter(handler.Deps{
		Session:  svc,
		Verifier: fakeTokenVerifier{userID: 7},
	})
}

// req 发一个带登录态的请求,方法和 body 都可指定。
func req(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	hr := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer whatever")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, hr)
	return w
}

func dataOf(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析失败: %v (body=%s)", err, w.Body.String())
	}
	return body.Data
}

// ---- 路由本身 ----

// 每条路由都必须挂上 Auth,而且没带令牌时 service 一次都不能被调用。
// 漏挂一条的话,MustUserID 会 panic 变成 50000 —— 这条用例就是那道闸。
func TestSessionRoutes_AllRequireAuth(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/sessions"},
		{http.MethodGet, "/api/v1/sessions"},
		{http.MethodGet, "/api/v1/sessions/" + sampleSID},
		{http.MethodPatch, "/api/v1/sessions/" + sampleSID},
		{http.MethodDelete, "/api/v1/sessions/" + sampleSID},
		{http.MethodPost, "/api/v1/sessions/" + sampleSID + "/tasks"},
		{http.MethodGet, "/api/v1/sessions/" + sampleSID + "/tasks"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			svc := &fakeSession{sess: sampleSession(), task: sampleTask()}
			hr := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			hr.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			sessionRouter(svc).ServeHTTP(w, hr)

			if got := decode(t, w); got.Code != response.CodeUnauthorized {
				t.Fatalf("code = %d, 期望 %d (body=%s)", got.Code, response.CodeUnauthorized, w.Body.String())
			}
			if svc.calls != 0 {
				t.Errorf("未认证的请求走到了 service")
			}
		})
	}
}

// 依赖没注入时一律 500,不能 404,也不能 panic。
func TestSessionRoutes_NilServiceFailsClosed(t *testing.T) {
	r := handler.NewRouter(handler.Deps{Verifier: fakeTokenVerifier{userID: 7}}) // Session 为 nil
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/sessions"},
		{http.MethodGet, "/api/v1/sessions"},
		{http.MethodGet, "/api/v1/sessions/" + sampleSID},
		{http.MethodPatch, "/api/v1/sessions/" + sampleSID},
		{http.MethodDelete, "/api/v1/sessions/" + sampleSID},
		{http.MethodPost, "/api/v1/sessions/" + sampleSID + "/tasks"},
		{http.MethodGet, "/api/v1/sessions/" + sampleSID + "/tasks"},
	} {
		w := req(r, tc.method, tc.path, `{}`)
		if got := decode(t, w); got.Code != response.CodeInternal {
			t.Errorf("%s %s: code = %d, 期望 %d", tc.method, tc.path, got.Code, response.CodeInternal)
		}
	}
}

// ---- POST /sessions ----

func TestCreateSession_OK(t *testing.T) {
	svc := &fakeSession{sess: sampleSession(), task: sampleTask()}
	w := req(sessionRouter(svc), http.MethodPost, "/api/v1/sessions", `{"title":"订机票","input":"帮我订张票"}`)

	if got := decode(t, w); got.Code != response.CodeOK {
		t.Fatalf("code = %d, body=%s", got.Code, w.Body.String())
	}
	// 身份必须来自令牌,不是 body。
	if svc.gotUserID != 7 {
		t.Errorf("userID = %d, 期望来自令牌的 7", svc.gotUserID)
	}
	if svc.gotTitle == nil || *svc.gotTitle != "订机票" {
		t.Errorf("title = %v", svc.gotTitle)
	}

	// 会话和首个任务要一起返回,客户端建完就得拿 task_uid 去轮询。
	data := dataOf(t, w)
	if data["session"] == nil || data["task"] == nil {
		t.Fatalf("session 和 task 都该有: %s", w.Body.String())
	}
}

// 两个字段都可空:允许先开一个空会话。
func TestCreateSession_EmptyBody(t *testing.T) {
	svc := &fakeSession{sess: sampleSession(), task: sampleTask()}
	w := req(sessionRouter(svc), http.MethodPost, "/api/v1/sessions", `{}`)

	if got := decode(t, w); got.Code != response.CodeOK {
		t.Fatalf("code = %d, body=%s", got.Code, w.Body.String())
	}
	if svc.gotTitle != nil || svc.gotInput != nil {
		t.Errorf("空 body 应当传 nil,实际 title=%v input=%v", svc.gotTitle, svc.gotInput)
	}
}

// 建会话这一侧不一样:input 只有空白等于"先开个空会话",
// 是正常用法而不是坏请求 —— 回 200,但传给 service 的是 nil,
// 于是那一趟不建任务、不派发。
func TestCreateSession_BlankInputCreatesNoTask(t *testing.T) {
	for _, body := range []string{`{"input":""}`, `{"input":"   "}`, `{"title":"订机票","input":"\t\n"}`} {
		svc := &fakeSession{sess: sampleSession()}
		w := req(sessionRouter(svc), http.MethodPost, "/api/v1/sessions", body)

		if got := decode(t, w); got.Code != response.CodeOK {
			t.Fatalf("body=%s: code = %d, body=%s", body, got.Code, w.Body.String())
		}
		if svc.gotInput != nil {
			t.Errorf("body=%s: 该给 service 传 nil,实际 %q", body, *svc.gotInput)
		}
	}
}

// 有正文的话去掉首尾空白再往下传。
func TestCreateSession_TrimsInput(t *testing.T) {
	svc := &fakeSession{sess: sampleSession(), task: sampleTask()}
	req(sessionRouter(svc), http.MethodPost, "/api/v1/sessions", `{"input":"  帮我订张票  "}`)

	if svc.gotInput == nil || *svc.gotInput != "帮我订张票" {
		t.Errorf("input = %v, 期望去掉首尾空白", svc.gotInput)
	}
}

// title 对齐 varchar(128),input 对齐"别让 text 列被塞满"。
func TestCreateSession_RejectsOversized(t *testing.T) {
	for _, body := range []string{
		`{"title":"` + strings.Repeat("好", 129) + `"}`,
		`{"input":"` + strings.Repeat("好", 8001) + `"}`,
	} {
		svc := &fakeSession{sess: sampleSession(), task: sampleTask()}
		w := req(sessionRouter(svc), http.MethodPost, "/api/v1/sessions", body)

		if got := decode(t, w); got.Code != response.CodeInvalidParam {
			t.Errorf("code = %d, 期望 %d", got.Code, response.CodeInvalidParam)
		}
		if svc.calls != 0 {
			t.Error("超长的输入不该走到 service")
		}
	}
}

// ---- GET /sessions ----

// 空列表必须是 [] 不是 null。null 的话客户端一个 items.map 就炸,
// 而且只在"这个用户一条数据都没有"时炸 —— 本地基本复现不出来。
func TestListSessions_EmptyIsArrayNotNull(t *testing.T) {
	w := req(sessionRouter(&fakeSession{list: nil}), http.MethodGet, "/api/v1/sessions", "")

	if !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("空列表应当是 []: %s", w.Body.String())
	}
}

func TestListSessions_PassesPaging(t *testing.T) {
	svc := &fakeSession{list: []model.Session{*sampleSession()}}
	w := req(sessionRouter(svc), http.MethodGet, "/api/v1/sessions?limit=5&offset=10", "")

	if got := decode(t, w); got.Code != response.CodeOK {
		t.Fatalf("code = %d, body=%s", got.Code, w.Body.String())
	}
	if svc.gotLimit != 5 || svc.gotOffset != 10 {
		t.Errorf("limit=%d offset=%d, 期望 5/10", svc.gotLimit, svc.gotOffset)
	}
	// 生效的分页参数要回显,否则客户端不知道自己的 limit 被夹过。
	data := dataOf(t, w)
	if data["limit"] != float64(5) || data["offset"] != float64(10) {
		t.Errorf("回显不对: %s", w.Body.String())
	}
}

// limit=-1 是最危险的一个:GORM 的 Limit(负数) 是"整个不写 LIMIT 子句",
// 也就是全表。必须在入口就拦下。
func TestListSessions_RejectsBadPaging(t *testing.T) {
	for _, q := range []string{
		"?limit=-1",
		"?limit=0&offset=-1",
		"?limit=101",
		"?limit=abc",
		"?offset=" + strings.Repeat("9", 30), // 溢出 int64
	} {
		svc := &fakeSession{list: []model.Session{}}
		w := req(sessionRouter(svc), http.MethodGet, "/api/v1/sessions"+q, "")

		if got := decode(t, w); got.Code != response.CodeInvalidParam {
			t.Errorf("%s: code = %d, 期望 %d (body=%s)", q, got.Code, response.CodeInvalidParam, w.Body.String())
		}
		if svc.calls != 0 {
			t.Errorf("%s: 不该走到 service", q)
		}
	}
}

// 不传分页参数是合法的,零值交给 service 去套默认值。
func TestListSessions_NoPagingIsOK(t *testing.T) {
	svc := &fakeSession{list: []model.Session{}}
	w := req(sessionRouter(svc), http.MethodGet, "/api/v1/sessions", "")

	if got := decode(t, w); got.Code != response.CodeOK {
		t.Fatalf("code = %d, body=%s", got.Code, w.Body.String())
	}
	if svc.gotLimit != 0 || svc.gotOffset != 0 {
		t.Errorf("limit=%d offset=%d, 期望都是 0", svc.gotLimit, svc.gotOffset)
	}
}

// ---- GET /sessions/:id ----

// user_id 是内部主键,不该散播到每一个会话响应里。
func TestSessionDetail_HidesUserID(t *testing.T) {
	w := req(sessionRouter(&fakeSession{sess: sampleSession()}), http.MethodGet, "/api/v1/sessions/"+sampleSID, "")

	data := dataOf(t, w)
	for _, k := range []string{"user_id", "UserID", "deleted_at", "DeletedAt"} {
		if _, ok := data[k]; ok {
			t.Errorf("响应里出现了 %q: %s", k, w.Body.String())
		}
	}
	if data["session_id"] != sampleSID {
		t.Errorf("缺少 session_id: %s", w.Body.String())
	}
}

// 「不存在」和「不是你的」在 service 那边就被归成了同一个 ErrSessionNotFound。
// handler 必须原样回 40400,不能改写成能区分出来的码 —— 一旦能区分,
// 这组接口就成了 session_id 探测器。被 %w 包过一层也得照样命中。
func TestSessionDetail_NotFoundIsOpaque(t *testing.T) {
	for _, err := range []error{
		model.ErrSessionNotFound,
		fmt.Errorf("service: 查会话失败: %w", model.ErrSessionNotFound),
	} {
		w := req(sessionRouter(&fakeSession{err: err}), http.MethodGet, "/api/v1/sessions/"+sampleSID, "")

		got := decode(t, w)
		if got.Code != response.CodeNotFound {
			t.Fatalf("code = %d, 期望 %d (body=%s)", got.Code, response.CodeNotFound, w.Body.String())
		}
		for _, leak := range []string{"权限", "属于", "无权", "他人"} {
			if strings.Contains(got.Message, leak) {
				t.Errorf("文案泄露了归属信息: %q", got.Message)
			}
		}
	}
}

func TestSessionDetail_BadID(t *testing.T) {
	svc := &fakeSession{sess: sampleSession()}
	for _, id := range []string{"short", strings.Repeat("A", 27), strings.Repeat("A", 4096)} {
		w := req(sessionRouter(svc), http.MethodGet, "/api/v1/sessions/"+id, "")
		if got := decode(t, w); got.Code != response.CodeInvalidParam {
			t.Errorf("id=%.20q: code = %d, 期望 %d", id, got.Code, response.CodeInvalidParam)
		}
	}
	if svc.calls != 0 {
		t.Error("格式不对的 id 不该走到 service")
	}
}

// 没登记的错误一律 50000,原文不进响应体。
func TestSessionDetail_UnknownErrorLeaksNothing(t *testing.T) {
	raw := `pq: relation "sessions" does not exist, host=10.0.0.1 password=hunter2`
	w := req(sessionRouter(&fakeSession{err: errors.New(raw)}), http.MethodGet, "/api/v1/sessions/"+sampleSID, "")

	if got := decode(t, w); got.Code != response.CodeInternal {
		t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeInternal)
	}
	for _, leak := range []string{"sessions", "password", "hunter2", "10.0.0.1"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("响应里出现了 %q: %s", leak, w.Body.String())
		}
	}
}

// ---- PATCH /sessions/:id ----

func TestUpdateTitle_Normalizes(t *testing.T) {
	for _, tc := range []struct {
		body string
		want *string
	}{
		{`{"title":"新标题"}`, ptr.To("新标题")},
		{`{"title":"  新标题  "}`, ptr.To("新标题")},
		{`{"title":""}`, nil},    // 空串 = 清空
		{`{"title":"   "}`, nil}, // 全空白也是清空
	} {
		svc := &fakeSession{sess: sampleSession()}
		w := req(sessionRouter(svc), http.MethodPatch, "/api/v1/sessions/"+sampleSID, tc.body)

		if got := decode(t, w); got.Code != response.CodeOK {
			t.Fatalf("%s: code = %d (body=%s)", tc.body, got.Code, w.Body.String())
		}
		switch {
		case tc.want == nil && svc.gotTitle != nil:
			t.Errorf("%s: 期望 nil, 实际 %q", tc.body, *svc.gotTitle)
		case tc.want != nil && svc.gotTitle == nil:
			t.Errorf("%s: 期望 %q, 实际 nil", tc.body, *tc.want)
		case tc.want != nil && *svc.gotTitle != *tc.want:
			t.Errorf("%s: 期望 %q, 实际 %q", tc.body, *tc.want, *svc.gotTitle)
		}
	}
}

// {} 和 {"title":null} 都必须被拒。
// 放过去的话它们和 {"title":""} 在 Go 这边都是 nil,
// "没提这个字段"就会被当成"要求清空"—— 语义完全相反。
func TestUpdateTitle_RequiresExplicitField(t *testing.T) {
	for _, body := range []string{`{}`, `{"title":null}`, ``} {
		svc := &fakeSession{sess: sampleSession()}
		w := req(sessionRouter(svc), http.MethodPatch, "/api/v1/sessions/"+sampleSID, body)

		if got := decode(t, w); got.Code != response.CodeInvalidParam {
			t.Errorf("body=%q: code = %d, 期望 %d", body, got.Code, response.CodeInvalidParam)
		}
		if svc.calls != 0 {
			t.Errorf("body=%q: 不该走到 service", body)
		}
	}
}

// ---- DELETE /sessions/:id ----

func TestDeleteSession_OK(t *testing.T) {
	svc := &fakeSession{}
	w := req(sessionRouter(svc), http.MethodDelete, "/api/v1/sessions/"+sampleSID, "")

	got := decode(t, w)
	if got.Code != response.CodeOK {
		t.Fatalf("code = %d, body=%s", got.Code, w.Body.String())
	}
	if got.Data != nil {
		t.Errorf("data 应当是 null, 实际 %#v", got.Data)
	}
	if svc.gotSID != sampleSID || svc.gotUserID != 7 {
		t.Errorf("service 收到 sid=%q userID=%d", svc.gotSID, svc.gotUserID)
	}
}

// 删一个已经删掉的返回 40400。一律回 200 的话,
// 客户端就再也分不出"删掉了"和"这个 id 根本不存在"。
func TestDeleteSession_AlreadyGone(t *testing.T) {
	w := req(sessionRouter(&fakeSession{err: model.ErrSessionNotFound}),
		http.MethodDelete, "/api/v1/sessions/"+sampleSID, "")

	if got := decode(t, w); got.Code != response.CodeNotFound {
		t.Fatalf("code = %d, 期望 %d", got.Code, response.CodeNotFound)
	}
}

// ---- /sessions/:id/tasks ----

// 建任务时 input 是必填的:建空会话说得通,建一个没有输入的任务说不通。
// 空串和纯空白也算没填。required 单独用挡不住它们:Input 是 *string,
// required 对指针只判 nil —— 少了 notblank 的话 {"input":""} 会一路进到
// service,建出一个空任务派给 Agent 空跑一轮。
func TestCreateTask_RequiresInput(t *testing.T) {
	for _, body := range []string{`{}`, `{"input":null}`, `{"input":""}`, `{"input":"   "}`} {
		svc := &fakeSession{task: sampleTask()}
		w := req(sessionRouter(svc), http.MethodPost, "/api/v1/sessions/"+sampleSID+"/tasks", body)

		if got := decode(t, w); got.Code != response.CodeInvalidParam {
			t.Errorf("body=%s: code = %d, 期望 %d", body, got.Code, response.CodeInvalidParam)
		}
		if svc.calls != 0 {
			t.Errorf("body=%s: 不该走到 service", body)
		}
	}
}

func TestCreateTask_OK(t *testing.T) {
	svc := &fakeSession{task: sampleTask()}
	w := req(sessionRouter(svc), http.MethodPost, "/api/v1/sessions/"+sampleSID+"/tasks", `{"input":"再订一张"}`)

	if got := decode(t, w); got.Code != response.CodeOK {
		t.Fatalf("code = %d, body=%s", got.Code, w.Body.String())
	}
	if svc.gotSID != sampleSID {
		t.Errorf("sessionID = %q", svc.gotSID)
	}
	if svc.gotInput == nil || *svc.gotInput != "再订一张" {
		t.Errorf("input = %v", svc.gotInput)
	}
	// 任务响应同样不能带自增主键和 user_id。
	data := dataOf(t, w)
	for _, k := range []string{"id", "user_id"} {
		if _, ok := data[k]; ok {
			t.Errorf("响应里出现了 %q: %s", k, w.Body.String())
		}
	}
}

// 正文两侧的空白去掉再往下传:它会跟着进库,也会跟着发给 Agent。
func TestCreateTask_TrimsInput(t *testing.T) {
	svc := &fakeSession{task: sampleTask()}
	req(sessionRouter(svc), http.MethodPost, "/api/v1/sessions/"+sampleSID+"/tasks", `{"input":"  再订一张  "}`)

	if svc.gotInput == nil || *svc.gotInput != "再订一张" {
		t.Errorf("input = %v, 期望去掉首尾空白", svc.gotInput)
	}
}

func TestListTasks_EmptyIsArrayNotNull(t *testing.T) {
	w := req(sessionRouter(&fakeSession{tasks: nil}), http.MethodGet, "/api/v1/sessions/"+sampleSID+"/tasks", "")

	if !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("空列表应当是 []: %s", w.Body.String())
	}
}

func TestListTasks_PassesSessionAndPaging(t *testing.T) {
	svc := &fakeSession{tasks: []model.Task{*sampleTask()}}
	w := req(sessionRouter(svc), http.MethodGet, "/api/v1/sessions/"+sampleSID+"/tasks?limit=3&offset=6", "")

	if got := decode(t, w); got.Code != response.CodeOK {
		t.Fatalf("code = %d, body=%s", got.Code, w.Body.String())
	}
	if svc.gotSID != sampleSID || svc.gotLimit != 3 || svc.gotOffset != 6 {
		t.Errorf("service 收到 sid=%q limit=%d offset=%d", svc.gotSID, svc.gotLimit, svc.gotOffset)
	}
}

// ---- GET /tasks/:uid ----

func TestTaskDetail_OK(t *testing.T) {
	svc := &fakeTask{task: sampleTask()}
	w := req(taskRouter(svc), http.MethodGet, "/api/v1/tasks/"+sampleUID, "")

	if got := decode(t, w); got.Code != response.CodeOK {
		t.Fatalf("code = %d, body=%s", got.Code, w.Body.String())
	}
	if svc.gotUID != sampleUID || svc.gotUserID != 7 {
		t.Errorf("service 收到 uid=%q userID=%d", svc.gotUID, svc.gotUserID)
	}
}

// 方法用错要回 40500,不能退化成 40400 —— 路径是存在的,只是方法不对。
func TestSessionRoutes_MethodNotAllowed(t *testing.T) {
	w := req(sessionRouter(&fakeSession{}), http.MethodPut, "/api/v1/sessions/"+sampleSID, `{}`)

	if got := decode(t, w); got.Code != response.CodeMethodNotAllowed {
		t.Fatalf("code = %d, 期望 %d (body=%s)", got.Code, response.CodeMethodNotAllowed, w.Body.String())
	}
}

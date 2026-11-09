// helpers_test.go 放本包测试共用的东西:事务夹具、假依赖、造数据。
//
// 判断一段代码该不该放这儿:**它有没有断言?** 有断言的是用例,留在
// 各自的 xxx_test.go;只是把用例跑起来的脚手架,放这儿。
package service

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jepson-Wang/mini_agent_backend/internal/agentclient"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ptr"
	"github.com/Jepson-Wang/mini_agent_backend/internal/repository"
)

// testPassword 是所有用例共用的明文口令。
// 各处断言"这个串不能出现在错误里 / 库里"都指着它。
const testPassword = "correct horse battery"

var emailSeq atomic.Int64

// uniqueEmail 每次给一个没用过的邮箱。
//
// 用例是跑在事务里、结束回滚的,理论上不会互相撞;但 users.email 上有唯一索引,
// 而一次约束冲突会让整个外层事务进入 aborted 状态(SQLSTATE 25P02),
// 后面所有语句全失败 —— 那种报错看起来和被测逻辑毫无关系,极难查。
// 用递增序号从源头上避免。
func uniqueEmail() string {
	return fmt.Sprintf("svc%d@test.local", emailSeq.Add(1))
}

// withTx 给出一个绑在独立事务上的仓库,用例结束一律回滚。
//
// 这是本包所有需要数据库的用例的唯一入口。绕开它直接用 testDB 的话,
// 用例之间会互相看见对方的数据,在 t.Parallel() 下偶发失败。
//
// 事务是 GORM 的嵌套事务:里层再开事务时它用的是 SAVEPOINT 而不是真嵌套,
// 所以被测代码里的 repo.Transaction 照常工作,最后一起被这里回滚掉。
func withTx(t *testing.T) *repository.Repository {
	t.Helper()
	if testDB == nil {
		t.Skip("未设置 TEST_DSN,跳过需要数据库的用例")
	}
	tx := testDB.Begin()
	if tx.Error != nil {
		t.Fatalf("开启事务失败: %v", tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
	return repository.New(tx)
}

// 这里原来有个泛型的 withSvc,专门服务"构造函数除了 repo 什么都不要"的
// service。SessionService 加上 Agent 客户端之后,三个 service 就都有假依赖了,
// 它一个用户也没剩下,所以删掉 —— 换成下面三个各自的夹具,
// 用例还能顺手拿到那个假依赖去断言。
//
// ---- 认证 ----

// fakeIssuer 记下拿到的 userID,顺便省掉真造一个 32 字节密钥。
type fakeIssuer struct {
	lastUID int64
	err     error
}

func (f *fakeIssuer) Issue(userID int64) (string, time.Time, error) {
	f.lastUID = userID
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	return "tok-" + strconv.FormatInt(userID, 10), time.Now().Add(time.Hour), nil
}

// withAuthSvc 在独立事务里跑一个 AuthService 用例,顺带把假签发器交给用例 ——
// 好些断言要看它收到的 userID。
func withAuthSvc(t *testing.T, fn func(t *testing.T, s *AuthService, r *repository.Repository, iss *fakeIssuer)) {
	t.Helper()
	r := withTx(t)
	iss := &fakeIssuer{}
	fn(t, NewAuthService(r, iss, nil), r, iss)
}

// ---- 会话 ----

// withSessionSvc 在独立事务里跑一个 SessionService 用例,Agent 是假的。
// 用例能拿到那个假 Agent,验"建完任务有没有派发出去"。
func withSessionSvc(t *testing.T, fn func(t *testing.T, s *SessionService, r *repository.Repository, ag *fakeAgent)) {
	t.Helper()
	repo := withTx(t)
	ag := &fakeAgent{}
	fn(t, NewSessionService(repo, ag, nil), repo, ag)
}

// seedUser 建一个用户,返回它的 id。邮箱自动取唯一值。
func seedUser(t *testing.T, r *repository.Repository) int64 {
	t.Helper()

	u, err := r.User.Create(context.Background(), uniqueEmail(),
		"$2a$04$fakefakefakefakefakefakefakefakefakefakefa", nil)
	if err != nil {
		t.Fatalf("建用户: %v", err)
	}
	return u.ID
}

// seed 建一个用户、一个会话和一个任务。
//
// 传 nil 当 Agent:这些用例关心的是数据,不是派发。要验派发的
// 用 withSessionSvc,那里的假 Agent 会把 StartTask 记下来。
func seed(t *testing.T, r *repository.Repository) (userID int64, sess *model.Session, task *model.Task) {
	t.Helper()
	ctx := context.Background()

	u, err := r.User.Create(ctx, uniqueEmail(),
		"$2a$04$fakefakefakefakefakefakefakefakefakefakefa", nil)
	if err != nil {
		t.Fatalf("建用户: %v", err)
	}
	sess, task, err = NewSessionService(r, nil, nil).CreateWithFirstTask(ctx, u.ID,
		ptr.To("测试会话"), ptr.To("跑一下"))
	if err != nil {
		t.Fatalf("建会话和任务失败: %v", err)
	}
	return u.ID, sess, task
}

// ---- 任务 ----

// fakeAgent 记下派发和取消,可以分别指定让它们报错。
//
// startErr 和 err 分开:派发失败要标记任务失败,取消失败只记日志,
// 两条路径的策略不一样,得能单独触发。
type fakeAgent struct {
	started  []agentclient.StartTaskReq
	canceled []agentclient.CancelTaskReq

	startErr error // StartTask 的返回
	err      error // CancelTask 的返回

	// onStart 在 StartTask 里同步回调,用来观察"被派发的那一刻"
	// 数据库是什么样子。TestCreate_DispatchesAfterCommit 靠它。
	onStart func(agentclient.StartTaskReq)
}

func (f *fakeAgent) StartTask(_ context.Context, req agentclient.StartTaskReq) error {
	f.started = append(f.started, req)
	if f.onStart != nil {
		f.onStart(req)
	}
	return f.startErr
}

func (f *fakeAgent) CancelTask(_ context.Context, req agentclient.CancelTaskReq) error {
	f.canceled = append(f.canceled, req)
	return f.err
}

// Health / BreakerState 只为满足 agentclient.Client。
// service 这一层用不上它们 —— 探活是 /health 的事,归 handler。
func (f *fakeAgent) Health(context.Context) (agentclient.HealthStatus, error) {
	return agentclient.HealthStatus{OK: true}, nil
}

func (f *fakeAgent) BreakerState() string { return "closed" }

// canceledUIDs 只要 task_uid,大多数用例关心的就是这个。
func (f *fakeAgent) canceledUIDs() []string {
	uids := make([]string, 0, len(f.canceled))
	for _, r := range f.canceled {
		uids = append(uids, r.TaskUID)
	}
	return uids
}

// withTaskSvc 在独立事务里跑一个 TaskService 用例,Agent 是假的。
// 要验"没注入 Agent"那种场景就别用它,直接 withTx + NewTaskService(r, nil)。
func withTaskSvc(t *testing.T, fn func(t *testing.T, s *TaskService, r *repository.Repository, ag *fakeAgent)) {
	t.Helper()
	repo := withTx(t)
	ag := &fakeAgent{}
	fn(t, NewTaskService(repo, ag), repo, ag)
}

// ---- 任务事件 ----

// statusCall 是 fakeStatusWriter 记下的一次调用。
type statusCall struct {
	taskUID string
	status  string
	errMsg  *string
	output  *string
}

// fakeStatusWriter 是 taskStatusWriter 的假实现,用来验事件到状态的映射 ——
// 这一层要看的是"哪种事件写哪个状态、写不写",跟数据库没关系。
// applied 和 err 可以指定,用来模拟"迟到的事件"和"库挂了"。
type fakeStatusWriter struct {
	calls   []statusCall
	applied bool
	err     error

	// touched 记下每次心跳的 task_uid,用来验"进度事件有没有续命"。
	touched []string
	// touchErr 让心跳报错,验它不会把事件处理带崩。
	touchErr error

	// running / runningErr 驱动 IsRunning,验重发前那道闸门。
	running    bool
	runningErr error
}

func newFakeStatusWriter() *fakeStatusWriter {
	return &fakeStatusWriter{applied: true} // 默认当作改到了行
}

// 报错时**照样**返回 running,不返回零值。真仓库当然是返回零值的,
// 但那样这个假实现就把"报错要不要重发"这件事替被测代码答了 ——
// ShouldRetry 里那个 if err != nil 删掉用例照样绿。
func (f *fakeStatusWriter) IsRunning(_ context.Context, _ string) (bool, error) {
	return f.running, f.runningErr
}

func (f *fakeStatusWriter) HandleCallback(_ context.Context, taskUID, status string,
	res CallbackResult) (bool, error) {

	f.calls = append(f.calls, statusCall{
		taskUID: taskUID, status: status, errMsg: res.ErrorMsg, output: res.Output,
	})
	if f.err != nil {
		return false, f.err
	}
	return f.applied, nil
}

func (f *fakeStatusWriter) Heartbeat(_ context.Context, taskUID string) error {
	f.touched = append(f.touched, taskUID)
	return f.touchErr
}

// seedTask 建一个用户、一个会话和一个 pending 任务,返回用户 id 和 task_uid。
// 邮箱自动取唯一值,调用方不用操心。
func seedTask(t *testing.T, r *repository.Repository) (int64, string) {
	t.Helper()
	uid, _, task := seed(t, r)
	return uid, task.TaskUID
}

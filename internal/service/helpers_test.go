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

	agentv1 "github.com/Jepson-Wang/mini_agent_backend/gen/agent/v1"
	"github.com/Jepson-Wang/mini_agent_backend/internal/agent"
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

// withSvc 在独立事务里跑一个用例,service 由 newSvc 现场构造。
//
//	withSvc(t, NewSessionService, func(t *testing.T, s *SessionService, r *repository.Repository) {
//		...
//	})
//
// 只服务"构造函数除了 repo 什么都不要"的 service。AuthService 要 TokenIssuer、
// TaskService 要 agent.Client,而且用例还得能拿到那个假的去断言 ——
// 那两个用下面的 withAuthSvc / withTaskSvc。
//
// 别为了统一硬塞进来:把假依赖也做成类型参数的话,SessionService 那边
// 就得传一个 struct{} 占位,读起来比现在多两个专用夹具更费劲。
func withSvc[S any](t *testing.T, newSvc func(*repository.Repository) S,
	fn func(t *testing.T, s S, r *repository.Repository)) {

	t.Helper()
	r := withTx(t)
	fn(t, newSvc(r), r)
}

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
	fn(t, NewAuthService(r, iss), r, iss)
}

// ---- 会话 ----

func seed(t *testing.T, r *repository.Repository) (userID int64, sess *model.Session, task *model.Task) {
	t.Helper()
	ctx := context.Background()

	u, err := r.User.Create(ctx, uniqueEmail(),
		"$2a$04$fakefakefakefakefakefakefakefakefakefakefa", nil)
	if err != nil {
		t.Fatalf("建用户: %v", err)
	}
	sess, task, err = NewSessionService(r).CreateWithFirstTask(ctx, u.ID,
		ptr.To("测试会话"), ptr.To("跑一下"))
	if err != nil {
		t.Fatalf("建会话和任务失败: %v", err)
	}
	return u.ID, sess, task
}

// ---- 任务 ----

// fakeAgent 记下被要求取消了哪些任务,可以指定让 CancelTask 报错。
type fakeAgent struct {
	canceled []string
	err      error
}

func (f *fakeAgent) StartTask(context.Context, agentclient.StartTaskReq) error { return nil }

func (f *fakeAgent) CancelTask(_ context.Context, request *agentv1.CancelTaskRequest) error {
	f.canceled = append(f.canceled, request.TaskUid)
	return f.err
}

// withTaskSvc 在独立事务里跑一个 TaskService 用例,Agent 是假的。
// 要验"没注入 Agent"那种场景就别用它,直接 withTx + NewTaskService(r, nil)。
func withTaskSvc(t *testing.T, fn func(t *testing.T, s *TaskService, r *repository.Repository, ag *fakeAgent)) {
	t.Helper()
	repo := withTx(t)
	ag := &fakeAgent{}
	fn(t, NewTaskService(repo, ag), repo, ag)
}

// seedTask 建一个用户、一个会话和一个 pending 任务,返回用户 id 和 task_uid。
// 邮箱自动取唯一值,调用方不用操心。
func seedTask(t *testing.T, r *repository.Repository) (int64, string) {
	t.Helper()
	uid, _, task := seed(t, r)
	return uid, task.TaskUID
}

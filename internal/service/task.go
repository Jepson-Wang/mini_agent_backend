package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Jepson-Wang/mini_agent_backend/internal/agent"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/trace"
	"github.com/Jepson-Wang/mini_agent_backend/internal/repository"
)

// notifyTimeout 通知 Agent 取消时等它的时间。
// 短一点:这一步是 best-effort,不该让用户的取消请求跟着它一起卡。
const notifyTimeout = 3 * time.Second

// TaskService 负责任务的读取、取消,以及消费 Agent 报上来的状态回调。
type TaskService struct {
	repo  *repository.Repository
	agent agent.Client
}

// NewTaskService 组装任务服务。
//
// ag 传 nil 表示 Agent 还没接上:取消照样落库,只是通知发不出去,
// 会记一条 Warn。不设计成"没有 Agent 就报错" —— 库里的状态才是权威,
// 通知只是让对面早点收手。
func NewTaskService(repo *repository.Repository, ag agent.Client) *TaskService {
	return &TaskService{repo: repo, agent: ag}
}

// GetTaskDetailByTaskID 返回属于该用户的任务详情。
func (s *TaskService) GetTaskDetailByTaskID(ctx context.Context, userID int64,
	taskUID string) (*model.Task, error) {

	return s.mustOwn(ctx, userID, taskUID)
}

// Cancel 取消一个任务,返回取消后的任务。
//
// 顺序是**先落库、再通知 Agent**,不能反过来。
// 先通知的话,Agent 完全可能在我们写库之前就把 succeeded 回调回来,
// 于是我们紧接着把一个已经跑完的任务盖成 canceled,用户看到的状态是错的。
// 先写 canceled 则相反:迟到的回调撞上 CAS 的 from=[pending,running],
// 自然落空,不用额外加锁。
func (s *TaskService) Cancel(ctx context.Context, userID int64, taskUID string) (*model.Task, error) {
	t, err := s.mustOwn(ctx, userID, taskUID)
	if err != nil {
		return nil, err
	}

	// 这一步只是为了给出一句准确的错误(「已经成功了」而不是「取消失败」),
	// 真正的仲裁在下面那条 UPDATE 里 —— 这中间随时可能有回调插进来。
	if model.IsTerminal(t.Status) {
		return nil, fmt.Errorf("%w: status=%s", model.ErrTaskNotCancelable, t.Status)
	}

	ok, err := s.repo.Task.MarkCanceled(ctx, taskUID)
	if err != nil {
		return nil, err
	}
	if !ok {
		// 刚才读到的还是中间态,这条 UPDATE 却没匹配上 ——
		// 说明这两句之间 Agent 的回调先落地了。以库里的为准,不重试。
		return nil, fmt.Errorf("%w: 状态已被并发改写", model.ErrTaskNotCancelable)
	}

	s.notifyCancel(ctx, taskUID)

	// 重新查一次。上面那份 t 是 UPDATE 之前读的,status / finished_at
	// / updated_at 都已经过期,直接返回它等于骗客户端。
	return s.repo.Task.GetByUID(ctx, taskUID)
}

// notifyCancel 通知 Agent 别跑了。best-effort,失败只记日志。
//
// 库里已经是 canceled,那才是权威状态;通知不到顶多让 Agent 白跑一会儿,
// 它回来的结果会被 CAS 丢掉。为了一次通知失败就把用户的取消判为失败,
// 会让用户重试 —— 而重试同样会失败,因为任务已经是终态了。
func (s *TaskService) notifyCancel(ctx context.Context, taskUID string) {
	traceID := trace.FromContext(ctx)

	if s.agent == nil {
		slog.Warn("未注入 Agent 客户端,取消只落了库",
			"task_uid", taskUID, "trace_id", traceID)
		return
	}

	// WithoutCancel 保留 trace id,但断开请求的取消信号:
	// 事务已经提交了,客户端这时候断开连接不该让这条通知也跟着黄。
	// 断开之后必须自己配一个超时,否则这里可能一直挂着。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
	defer cancel()

	if err := s.agent.CancelTask(ctx, taskUID); err != nil {
		slog.Warn("通知 Agent 取消任务失败",
			"task_uid", taskUID, "trace_id", traceID, "error", err)
	}
}

// HandleCallback 消费 Agent 报上来的任务状态,返回这条回调有没有真的改到行。
//
// applied=false 不是错误:任务已经是终态(最常见的是用户抢先取消了),
// 或者 task_uid 根本不存在。两种都不往上抛 —— 回调方是机器,
// 拿到 5xx 只会无限重试,而这两种情况重试一万次结果都一样。
// 真正值得重试的是数据库不可用,那个会作为 error 抛出去。
//
// 注意这里没有用户身份可用:调用方是 Agent,不是人。所以这个方法
// 只能按 taskUID 推进状态,绝不能有任何"按入参当身份"的读写。
func (s *TaskService) HandleCallback(ctx context.Context, taskUID, status string,
	errMsg *string) (applied bool, err error) {

	if taskUID == "" {
		return false, model.ErrTaskNotFound
	}

	switch status {
	case model.TaskRunning:
		applied, err = s.repo.Task.MarkRunning(ctx, taskUID)
	case model.TaskSucceeded:
		applied, err = s.repo.Task.MarkSucceeded(ctx, taskUID)
	case model.TaskFailed:
		applied, err = s.repo.Task.MarkFailed(ctx, taskUID, errMsg)
	default:
		// canceled 不在列表里是刻意的:取消只能由用户发起。
		// 让 Agent 能把任务改成"已取消",等于给一个被攻破的 Agent
		// 一个悄悄吞掉任务的开关 —— 用户看到的是自己取消的,不会去查。
		//
		// handler 的 oneof 已经挡过一层,这里再挡一次:
		// service 不能假设自己只会被 HTTP 调用。
		return false, fmt.Errorf("%w: %q", model.ErrTaskStatusInvalid, status)
	}
	if err != nil {
		return false, err
	}
	if !applied {
		s.logIgnoredCallback(ctx, taskUID, status)
	}
	return applied, nil
}

// logIgnoredCallback 区分"迟到"和"根本不存在"。
//
// CAS 分不出这两种(都是 0 行受影响),但运维意义差很远:
// 迟到是正常现象,而一个查不到的 task_uid 说明两边的数据对不上了,得有人看。
// 只在没生效的分支多查这一次,正常路径不受影响。
//
// 这也是"静默失败"和"消化掉错误"的区别 —— 我们不把它抛给回调方,
// 但一定要在自己这边留下记录。
func (s *TaskService) logIgnoredCallback(ctx context.Context, taskUID, status string) {
	traceID := trace.FromContext(ctx)

	t, err := s.repo.Task.GetByUID(ctx, taskUID)
	switch {
	case errors.Is(err, model.ErrTaskNotFound):
		slog.Error("收到未知任务的回调",
			"task_uid", taskUID, "status", status, "trace_id", traceID)
	case err != nil:
		slog.Warn("回调未生效,复查任务也失败",
			"task_uid", taskUID, "status", status, "trace_id", traceID, "error", err)
	default:
		slog.Info("忽略迟到的回调",
			"task_uid", taskUID, "status", status,
			"current_status", t.Status, "trace_id", traceID)
	}
}

// mustOwn 取出任务并确认它属于 userID。
//
// 别人的任务一律返回 model.ErrTaskNotFound,不返回"无权限" ——
// 403 和 404 分开的话,这个接口就能用来问"这个 task_uid 存不存在"。
// 和 SessionService.mustOwn 是同一条规则。
//
// tasks.user_id 是从会话冗余下来的一列,就是为了这一步不用 join。
func (s *TaskService) mustOwn(ctx context.Context, userID int64, taskUID string) (*model.Task, error) {
	if userID <= 0 || taskUID == "" {
		return nil, model.ErrTaskNotFound
	}

	t, err := s.repo.Task.GetByUID(ctx, taskUID)
	if err != nil {
		return nil, err // 已翻译成 model.ErrTaskNotFound
	}
	if t.UserID != userID {
		return nil, model.ErrTaskNotFound
	}
	return t, nil
}

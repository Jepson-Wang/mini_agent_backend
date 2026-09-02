package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Jepson-Wang/mini_agent_backend/internal/agentclient"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/trace"
	"github.com/Jepson-Wang/mini_agent_backend/internal/repository"
)

// notifyTimeout 通知 Agent 取消时等它的时间。
// 短一点:这一步是 best-effort,不该让用户的取消请求跟着它一起卡。
const notifyTimeout = 3 * time.Second

// touchInterval 同一个任务两次心跳之间的最小间隔。
//
// 不加节流的话,一个话痨 Agent 每秒推几十条进度事件,就是每秒几十条
// UPDATE 打在同一行上 —— 行锁排队,还把 idx_tasks_status_updated
// 搅得到处是死元组。而心跳要回答的问题只有一个:"这任务过去 N 分钟
// 有没有动静",精确到秒毫无意义。
//
// 它必须远小于 reconcile.stale_after(默认 15 分钟),否则续命的频率
// 追不上判死的窗口,长任务照样会被回收器误杀。
const touchInterval = 30 * time.Second

// TaskService 负责任务的读取、取消,以及消费 Agent 报上来的状态回调。
type TaskService struct {
	repo  *repository.Repository
	agent agentclient.Client
}

// NewTaskService 组装任务服务。
//
// ag 传 nil 表示 Agent 还没接上:取消照样落库,只是通知发不出去,
// 会记一条 Warn。不设计成"没有 Agent 就报错" —— 库里的状态才是权威,
// 通知只是让对面早点收手。
func NewTaskService(repo *repository.Repository, ag agentclient.Client) *TaskService {
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
// reason 是用户填的取消理由,空串表示没填,落库存 NULL。
func (s *TaskService) Cancel(ctx context.Context, userID int64,
	taskUID, reason string) (*model.Task, error) {

	t, err := s.mustOwn(ctx, userID, taskUID)
	if err != nil {
		return nil, err
	}

	// 这一步只是为了给出一句准确的错误(「已经成功了」而不是「取消失败」),
	// 真正的仲裁在下面那条 UPDATE 里 —— 这中间随时可能有回调插进来。
	if model.IsTerminal(t.Status) {
		return nil, fmt.Errorf("%w: status=%s", model.ErrTaskNotCancelable, t.Status)
	}

	ok, err := s.repo.Task.MarkCanceled(ctx, taskUID, normalizeReason(reason))
	if err != nil {
		return nil, err
	}
	if !ok {
		// 刚才读到的还是中间态,这条 UPDATE 却没匹配上 ——
		// 说明这两句之间 Agent 的回调先落地了。以库里的为准,不重试。
		return nil, fmt.Errorf("%w: 状态已被并发改写", model.ErrTaskNotCancelable)
	}

	s.notifyCancel(ctx, taskUID, reason)

	// 重新查一次。上面那份 t 是 UPDATE 之前读的,status / finished_at
	// / updated_at 都已经过期,直接返回它等于骗客户端。
	return s.repo.Task.GetByUID(ctx, taskUID)
}

// notifyCancel 通知 Agent 别跑了。best-effort,失败只记日志。
//
// 库里已经是 canceled,那才是权威状态;通知不到顶多让 Agent 白跑一会儿,
// 它回来的结果会被 CAS 丢掉。为了一次通知失败就把用户的取消判为失败,
// 会让用户重试 —— 而重试同样会失败,因为任务已经是终态了。
func (s *TaskService) notifyCancel(ctx context.Context, taskUID, reason string) {
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

	// trace_id 从 ctx 取,不从入参传 —— 让 handler 显式往下递一个 trace id,
	// 等于同一个值有两条管道,迟早会有一条忘了接。
	req := agentclient.CancelTaskReq{TaskUID: taskUID, Reason: reason, TraceID: traceID}
	if err := s.agent.CancelTask(ctx, req); err != nil {
		slog.Warn("通知 Agent 取消任务失败",
			"task_uid", taskUID, "trace_id", traceID, "error", err)
	}
}

// CallbackResult 是一次回调带来的**结果内容**,和状态分开传。
//
// 合成一个结构体而不是继续往参数表上加 *string:两个相邻的可空字符串参数
// 是典型的"传反了编译器也不吭声"的形状,而它们一个是给用户看的答复、
// 一个是失败原因 —— 弄反了会把报错当成答案显示出来。
type CallbackResult struct {
	// ErrorMsg 只在 status=failed 时有意义。
	ErrorMsg *string

	// Output 只在 status=succeeded 时有意义,是 Agent 的最终答复。
	// 在此之前这个值是被丢掉的,于是"任务成功了"能查到、
	// "它到底答了什么"查不到,前端没法做对话界面。
	Output *string
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
	res CallbackResult) (applied bool, err error) {

	if taskUID == "" {
		return false, model.ErrTaskNotFound
	}

	switch status {
	case model.TaskRunning:
		applied, err = s.repo.Task.MarkRunning(ctx, taskUID)
	case model.TaskSucceeded:
		applied, err = s.repo.Task.MarkSucceeded(ctx, taskUID, res.Output)
	case model.TaskFailed:
		applied, err = s.repo.Task.MarkFailed(ctx, taskUID, res.ErrorMsg)
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

// Heartbeat 给一个正在跑的任务续命,让回收器别把它当成卡住的。
//
// 只有 running 的任务会被续(见 repository.TaskRepo.Touch),而且有节流,
// 所以调用方可以对**每一条**进度事件都调它,不用自己判断该不该调。
//
// 返回 error 只代表数据库出问题了。"没续上"(任务已经是终态、
// 或者离上次续命不够久)不是错误,也不需要让调用方知道 ——
// 这是尽力而为的一步:漏一次心跳最坏的结果是任务早 15 分钟被收掉,
// 而为了它去打断事件处理反而更糟。
func (s *TaskService) Heartbeat(ctx context.Context, taskUID string) error {
	if taskUID == "" {
		return nil
	}
	_, err := s.repo.Task.Touch(ctx, taskUID, time.Now().Add(-touchInterval))
	return err
}

// IsRunning 报告这个任务现在是不是还在 running。
//
// 只给重发那条路用(TaskEventHandler.ShouldRetry):流断了要不要原样重发,
// 取决于这个任务还在不在跑。用户取消过、或者回收器已经先收了尾的话,
// 重发就是白烧一轮 LLM —— 跑出来的结果撞上 cas 也写不进去。
//
// 刻意不判 pending:走到重发这一步说明我们收到过事件,任务早该是 running 了。
// 还停在 pending 意味着 started 那条 cas 没生效,那是另一个问题,
// 不该由重发来掩盖。
func (s *TaskService) IsRunning(ctx context.Context, taskUID string) (bool, error) {
	t, err := s.repo.Task.GetByUID(ctx, taskUID)
	if err != nil {
		return false, err
	}
	return t.Status == model.TaskRunning, nil
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

// normalizeReason 去空白;空的返回 nil,对应库里的 NULL。
// 和 normalizeNickname 同一个理由:别让库里同时存在 NULL 和 "" 两种"没填"。
func normalizeReason(reason string) *string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil
	}
	return &reason
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

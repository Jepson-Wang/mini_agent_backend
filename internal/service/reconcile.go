package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Jepson-Wang/mini_agent_backend/internal/config"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
)

// staleReason 是被回收的任务写进 error_msg 的固定文案。
//
// 说"没有收到执行结果"而不是"执行失败":我们并不知道 Agent 那边到底
// 跑成了没有,只知道结果没回来。写"执行失败"是在替它下结论,
// 而用户看到这句话之后的动作(重发一次)恰好是对的。
const staleReason = "任务超时未响应:长时间没有收到执行器的进展,已自动结束"

// reconcileTimeout 一轮回收最多跑多久。
//
// 必须有:这个循环跑在后台,ctx 只在进程退出时才会被取消。
// 库卡住的时候不加超时,回收协程会一直挂在那条 SELECT 上,
// 而下一次 tick 也就永远等不到 —— 回收器静默停摆,没有任何日志。
const reconcileTimeout = 30 * time.Second

// staleLister 是回收器需要的查询能力,生产实现是 *repository.TaskRepo。
//
// 定成接口(而不是直接收 *repository.Repository)是为了这一层能脱库测试:
// 回收器的逻辑是"捞出来、逐个收尾、别被单个失败带崩",跟 SQL 没关系。
type staleLister interface {
	ListStale(ctx context.Context, before time.Time, limit int) ([]model.Task, error)
}

// Reconciler 定期把卡住的任务收尾。
//
// 为什么需要它:**同步 Agent 只有事件流这一条汇报通道**。
// 进程重启、Pod 被驱逐、流被中间设备掐断、Agent 被 OOM kill ——
// 这些情况下 OnStreamError 根本没机会跑,任务就永远停在 pending / running。
// 用户界面上是一个转到天荒地老的圈,而且没有任何地方能解释为什么。
//
// 它只负责**收尾**,不负责重跑:一个跑了一半的 LLM 任务重跑一遍
// 可能重复扣费、重复调外部工具。要不要重来是用户的决定,不是回收器的。
type Reconciler struct {
	lister staleLister
	tasks  taskStatusWriter

	interval   time.Duration
	staleAfter time.Duration
	batch      int
}

// NewReconciler 组装回收器。cfg.Interval <= 0 时返回 (nil, nil) ——
// 表示这个部署不启用回收,调用方判 nil 就行,不用自己读配置再判一遍。
func NewReconciler(lister staleLister, tasks taskStatusWriter, cfg config.Reconcile) (*Reconciler, error) {
	if cfg.Interval <= 0 {
		return nil, nil
	}
	if lister == nil || tasks == nil {
		return nil, errors.New("回收器缺少依赖:lister / tasks 不能为 nil")
	}
	// config.Validate 已经把这几条挡过一遍了,这里再挡一次:
	// 这个构造函数在测试里会被直接调用,不经过 config.Load。
	if cfg.StaleAfter <= 0 {
		return nil, fmt.Errorf("reconcile.stale_after 必须大于 0,当前为 %v", cfg.StaleAfter)
	}
	if cfg.Batch <= 0 {
		return nil, fmt.Errorf("reconcile.batch 必须大于 0,当前为 %d", cfg.Batch)
	}
	return &Reconciler{
		lister:     lister,
		tasks:      tasks,
		interval:   cfg.Interval,
		staleAfter: cfg.StaleAfter,
		batch:      cfg.Batch,
	}, nil
}

// Run 阻塞着按 interval 扫,直到 ctx 被取消。调用方通常起一个协程跑它。
//
// 启动时**不立刻扫一轮**,先等一个 interval。刚起来的进程什么都没派发过,
// 库里所有中间态任务都是上一代进程留下的 —— 那些确实该收,但不急在这一秒,
// 而启动瞬间正是连接池最紧张的时候。
func (r *Reconciler) Run(ctx context.Context) {
	slog.Info("任务回收器已启动",
		"interval", r.interval.String(),
		"stale_after", r.staleAfter.String(),
		"batch", r.batch)

	t := time.NewTicker(r.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("任务回收器已停止")
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

// tick 跑一轮,失败只记日志 —— 后台循环不能因为一次数据库抖动就退出。
func (r *Reconciler) tick(ctx context.Context) {
	runCtx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()

	n, err := r.RunOnce(runCtx)
	if err != nil {
		slog.Error("回收卡住的任务失败,等下一轮", "error", err)
		return
	}
	if n > 0 {
		// 只在真收了东西时打 INFO。每分钟一条"本轮 0 个"的日志
		// 会把真正有意义的那条淹掉。
		slog.Warn("回收了卡住的任务", "count", n)
	}
}

// RunOnce 扫一轮,返回这轮收掉了几个。导出是为了测试能直接驱动它,
// 不用等 ticker。
func (r *Reconciler) RunOnce(ctx context.Context) (int, error) {
	before := time.Now().Add(-r.staleAfter)

	stale, err := r.lister.ListStale(ctx, before, r.batch)
	if err != nil {
		return 0, fmt.Errorf("查询卡住的任务: %w", err)
	}

	reason := staleReason
	done := 0
	for _, t := range stale {
		// 每个任务单独判 ctx:一轮有 batch 个,超时了就停在这儿,
		// 剩下的下一轮接着收 —— 硬跑完只会让后面每一条都超时报错。
		if ctx.Err() != nil {
			slog.Warn("本轮回收被中断,剩下的留给下一轮",
				"done", done, "remaining", len(stale)-done)
			break
		}

		// 走 HandleCallback 而不是直接调仓库层。那条路上有三件不能绕开的事:
		// canceled 白名单、cas(所以一个在我们捞出来之后才完成的任务
		// 不会被盖成 failed)、以及没生效时的日志。
		applied, err := r.tasks.HandleCallback(ctx, t.TaskUID, model.TaskFailed,
			CallbackResult{ErrorMsg: &reason})
		if err != nil {
			// 单个失败不中断整轮:一条坏记录不该让后面几十个都收不掉。
			slog.Error("回收任务失败",
				"task_uid", t.TaskUID, "status", t.Status, "error", err)
			continue
		}
		if !applied {
			// 捞出来之后、UPDATE 之前它自己完成了。正常竞态,不是问题。
			slog.Info("待回收的任务已经自己结束了",
				"task_uid", t.TaskUID, "status", t.Status)
			continue
		}

		done++
		slog.Warn("任务长时间无响应,已标记为失败",
			"task_uid", t.TaskUID,
			"previous_status", t.Status,
			"last_update", t.UpdatedAt.Format(time.RFC3339),
			"stale_after", r.staleAfter.String())
	}
	return done, nil
}

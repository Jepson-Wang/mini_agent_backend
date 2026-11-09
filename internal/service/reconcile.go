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

// reconcileLockTTL 是选主锁的租约时长。
//
// **必须大于 reconcileTimeout(30s)。** 小于的话,一轮还没跑完锁就过期了,
// 另一个副本进来重复扫 —— 静默失效,没有任何迹象。
//
// **也不该远大于 Interval(默认 1m)。** 大太多的话,持锁副本崩溃后
// 锁要等 TTL 才释放,期间没有任何副本在扫,回收延迟被拉长。
//
// 所以取 45s:夹在 30s 和 60s 之间,两头都满足。
//
// **刻意不做成配置项。** 它是从 reconcileTimeout 推导出来的,
// 独立可配就意味着有人能配出一个小于 30s 的值,那时锁静默失效而毫无迹象。
// 写成由 reconcileTimeout 算出来的常量,改上面那个时这里自动跟上。
const reconcileLockTTL = reconcileTimeout + 15*time.Second

// reconcileReleaseTimeout 是释放选主锁那一次调用的超时。
//
// 单独一个短超时,**不复用 runCtx** —— 释放发生在 defer 里,那时 runCtx
// 很可能已经因为 reconcileTimeout 耗尽或进程关闭而失效了,拿它去 Release
// 会在本地直接失败、根本碰不到 Redis,锁于是挂满整个 TTL。
// 释放是一条 EVALSHA,正常亚毫秒级。
const reconcileReleaseTimeout = 2 * time.Second

// reconcileLockKey 是选主锁的 key。
//
// 带上服务名前缀:同一个 Redis 实例可能被别的服务共用
// (限流用的就是同一个 Redis),不加前缀会和别人的 key 撞上。
const reconcileLockKey = "lock:mini-agent:reconcile"

// LeaseLock 是回收器需要的加锁能力,生产实现是 *redislock.Lock。
//
// 导出是因为 main.go 要用它声明变量 —— 而那个变量必须用**接口类型**装,
// 才能把"没配 Redis"(nil)和"配了但拿不到锁"这两件事分开。
// 理由同 main.go 里 limiter 那段:把一个值为 nil 的具体指针直接赋给接口字段,
// 得到的接口值有类型、无值,不等于 nil。
//
// 定成接口(而且定在 service 包这个**使用方**)是有意的:
// 这样测试塞个假的就能跑,**不需要 Redis** —— 和 staleLister 同一个理由。
//
// ★ Acquire 的三个返回值有两个不同的语义,别混:
//
//   - ok:「要不要拦下这一轮」。false = 锁被别人持有着,这一轮别扫。
//     **Redis 挂了也返回 true**(fail open):回收器是兜底机制,
//     让兜底依赖另一个会挂的组件是反的。
//   - token:「我真的拿到了锁吗」。**token 非空才表示真的持有**。
//     Redis 挂了的时候 token 是空的 —— 那时候根本没有锁,也就没有锁要放。
//
// 所以调用方判断"要不要释放"必须看 token,不能看 ok。
type LeaseLock interface {
	// Acquire 尝试拿锁。返回 (token, ok, err)。
	Acquire(ctx context.Context, key string, ttl time.Duration) (string, bool, error)

	// Release 释放锁。只删自己的那把。
	Release(ctx context.Context, key, token string) error
}

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
//
// ## 多副本
//
// 部署多个副本时,每个副本都会跑这个循环,于是它们会捞同一批任务。
// **靠 cas 兜着不会写错任何数据**,只是白烧几次 SELECT。
//
// lock 是选主锁:一轮开始前先拿一次,拿不到就**跳过这一轮**。
// 这把锁不是正确性依赖(失效的后果只是退化回重复扫),
// 所以用的是单实例 SET NX PX,不是 Redlock —— 理由见 pkg/redislock 的包注释。
type Reconciler struct {
	lister staleLister
	tasks  taskStatusWriter

	// lock 是选主锁。**为 nil 时照常扫**(没配 Redis 的部署)。
	lock LeaseLock

	interval   time.Duration
	staleAfter time.Duration
	batch      int
}

// NewReconciler 组装回收器。cfg.Interval <= 0 时返回 (nil, nil) ——
// 表示这个部署不启用回收,调用方判 nil 就行,不用自己读配置再判一遍。
//
// lock 为 nil 是合法的:没配 Redis 的部署照样跑回收器,
// 只是多副本时没有选主。理由见 pkg/redislock:这把锁不是正确性依赖。
func NewReconciler(lister staleLister, tasks taskStatusWriter, lock LeaseLock, cfg config.Reconcile) (*Reconciler, error) {
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
		lock:       lock,
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
		"batch", r.batch,
		"leader_election", r.lock != nil)

	// "没有选主"这条警告**必须由这里打,不能由接线方打**。
	//
	// 它原来在 main.go 里写成了 `else if reconcileLock == nil { warn }`,
	// 夹在 `if reconciler == nil` 和装着 go Run 的 else 之间 —— 于是
	// REDIS_ADDR 为空(受支持的默认部署)时,分支走到警告就结束了,
	// **go Run 永远到不了**:回收器被构造出来然后静静丢掉。
	// 表现是"任务永远停在 running",而日志里只有一条看起来无害的 Warn。
	//
	// 放在 Run 里就不可能再出现那个形态:警告和"我真的在跑"绑在同一条语句流上,
	// 想打这条警告就必须已经进了 Run。接线方那边只剩一个 if/else。
	if r.lock == nil {
		slog.Warn("回收器没有选主(未配置 REDIS_ADDR),多副本部署时会重复扫同一批任务",
			"提示", "靠 cas 兜底不会写错数据,只是白烧几次查询")
	}

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

	// 选主:拿不到锁就跳过这一轮。
	//
	// **不等待、不重试。** 回收器是兜底机制,晚一分钟完全无所谓;
	// 自旋只会让所有副本挤在一起抢同一把锁。
	//
	// 拿不到锁**不是故障**,不用打 Error —— 每个副本每分钟都会打一条,
	// 那会让真正的错误淹没在噪声里。Info 就够了。
	//
	// lock 为 nil 表示这个部署没配 Redis(没启用选主)。照常扫 ——
	// 和 pkg/redislock 的 fail open 是同一个理由。nil 判断必须在这里:
	// 接口字段为 nil 时调方法是空指针 panic,而不是返回错误。
	//
	// **判据是 token 非空,不是 ok。**
	//
	// Acquire 返回的 ok 表示的是「要不要拦下这一轮」(fail open 时 Redis 挂了
	// 也返回 true),而 token 非空才表示「**我真的拿到了锁**」。
	// 用 ok 判定的话,Redis 挂时会走进释放分支 —— 去释放一把根本不存在的锁。
	// 用 token 判定,两种情况自然分开了。
	var token string
	if r.lock != nil {
		var ok bool
		var err error
		token, ok, err = r.lock.Acquire(runCtx, reconcileLockKey, reconcileLockTTL)
		switch {
		case err != nil:
			// Redis 出问题了。**fail open:照样扫。**
			//
			// 回收器本身就是兜底机制,让兜底依赖另一个会挂的组件是反的。
			// 代价只是退化回"多副本重复扫",不会写错数据。
			//
			// 这条错误要记下来:Redis 挂了这事运维该知道,
			// 只是不该让回收器停摆。
			slog.Warn("回收器加锁失败,本轮照常扫描(多副本可能重复扫)", "error", err)
		case !ok:
			// 别的副本正在扫。直接跳过,不等待也不重试。
			slog.Info("回收器锁已被别的副本持有,本轮跳过")
			return
		}
	}

	// 拿到锁就必须在函数结束时释放。
	//
	// **用 defer,不写在函数末尾。** tick 里有提前 return 的分支,
	// 还有 RunOnce 报错那条。写成末尾手动释放的话,ListStale 报错时
	// 锁要挂满整个 TTL(45s)——而 ListStale 报错恰好是数据库有问题的时候,
	// 也就是最需要下一轮快点重试的时候。
	//
	// defer 保证每一条返回路径都释放。
	//
	// 判据还是 token 非空:Redis 挂的时候压根没有锁,没有锁要放。
	if token != "" {
		defer func() {
			// ★ 释放**不能用 runCtx**,必须另起一个干净的 ctx。
			//
			// runCtx 带着 reconcileTimeout(30s)。RunOnce 把这个预算耗光
			// (库卡住)、或者关闭时父 ctx 被取消,runCtx 这时已经是死的 ——
			// Release 会在本地就失败、碰不到 Redis,锁于是挂满整个 45s TTL。
			// 而这恰恰是上面那段注释声称 defer 能避免的那个场景:
			// 库有病的时候,最需要下一轮快点重试。
			//
			// WithoutCancel 切断父 ctx 的取消信号,再配一个短超时:
			// 释放是一条 EVALSHA,正常亚毫秒级,2 秒够得很。
			relCtx, relCancel := context.WithTimeout(context.WithoutCancel(ctx), reconcileReleaseTimeout)
			defer relCancel()

			// 释放失败只记日志:没别的可做了,而且 TTL 会兜底 ——
			// 45 秒后锁自己就没了。
			if err := r.lock.Release(relCtx, reconcileLockKey, token); err != nil {
				slog.Warn("释放回收器锁失败,等 TTL 自动过期", "error", err)
			}
		}()
	}

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

package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Jepson-Wang/mini_agent_backend/internal/config"
	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
)

// taskCounter 是配额需要的查询能力,生产实现是 *repository.TaskRepo。
//
// 定成接口(而不是直接收 *repository.Repository)是为了这一层能脱库测试 ——
// 配额的逻辑是"数一下、和上限比一比、超了就拒",跟 SQL 没关系。
// 和 reconcile.go 的 staleLister 同一个套路。
type taskCounter interface {
	CountByUserSince(ctx context.Context, userID int64, since time.Time) (int64, error)
}

// TaskQuota 限制单个用户在一个时间窗口内能建多少任务。
//
// 为什么必须有:每个任务都会驱动一整轮 LLM 工具循环,而循环的轮数上限
// (Agent 侧的 IterationBudget)只挡住了单个任务的失控,挡不住"一个账号
// 一秒钟建一千个任务"。没有这一层的话,注册一个号就等于拿到一张无限额度的卡 ——
// 这个项目其它地方(载荷截断、熔断、迭代预算)防的都是同一件事,唯独最直接
// 的这道口子一直开着。
//
// 只在**建任务**时检查,不在取消/查询时检查:花钱的是建。
//
// ★ 这不是一把精确的锁。两个并发请求可能都读到"还差一个就满",于是都放行,
// 结果超一个。想做到精确就得把计数和插入放进同一个事务、再对用户行加锁 ——
// 为一个防滥用的粗粒度阈值付这个代价不划算。配额的作用是挡住"一秒一千个",
// 不是精确计费。
type TaskQuota struct {
	counter taskCounter
	window  time.Duration
	max     int

	// now 可注入,让测试不必真的等一个窗口过去。
	now func() time.Time
}

// NewTaskQuota 组装配额检查器。
//
// cfg.MaxTasks <= 0 时返回 (nil, nil) —— 表示这个部署不限额。
// 调用方**不用**判 nil:Check 是 nil 安全的,见它的说明。
// 和 NewReconciler 同一个约定。
func NewTaskQuota(counter taskCounter, cfg config.Quota) (*TaskQuota, error) {
	if cfg.MaxTasks <= 0 {
		return nil, nil
	}
	if counter == nil {
		return nil, errors.New("配额检查器缺少依赖:counter 不能为 nil")
	}
	// config.Validate 已经挡过一遍;这里再挡一次,因为这个构造函数
	// 在测试里会被直接调用,不经过 config.Load。
	if cfg.Window <= 0 {
		return nil, fmt.Errorf("quota.window 必须大于 0,当前为 %v", cfg.Window)
	}
	return &TaskQuota{
		counter: counter,
		window:  cfg.Window,
		max:     cfg.MaxTasks,
		now:     time.Now,
	}, nil
}

// Check 报告这个用户现在能不能再建一个任务。
//
// 返回 nil 表示放行;超额返回 model.ErrTaskQuotaExceeded。
//
// **接收者为 nil 时一律放行** —— 那表示这个部署没开配额。这样调用方就不用
// 到处写 if s.quota != nil,少一处能忘的地方。
//
// 数不出来时(库报错)**放行**,不拦。这是刻意的取舍:配额是防滥用的软限制,
// 而这条路径上的数据库故障已经会让紧接着的 INSERT 自己失败。在这儿拦下来
// 只会把一个"建任务失败"变成一个更难解释的"配额超限",指向完全错误的方向。
func (q *TaskQuota) Check(ctx context.Context, userID int64) error {
	if q == nil {
		return nil
	}
	since := q.now().Add(-q.window)
	n, err := q.counter.CountByUserSince(ctx, userID, since)
	if err != nil {
		return nil // 见上:数不出来不拦
	}
	if n >= int64(q.max) {
		return fmt.Errorf("%w: %d/%d in %v", model.ErrTaskQuotaExceeded, n, q.max, q.window)
	}
	return nil
}

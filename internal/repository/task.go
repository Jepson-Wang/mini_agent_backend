package repository

import (
	"context"
	"errors"
	"time"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"gorm.io/gorm"
)

type TaskRepo struct{ db *gorm.DB }

// Create 插入一个任务。task_uid 由调用方生成 —— 服务层通常要在开事务之前
// 就拿到这个 ID(见 service.SessionService.CreateWithFirstTask)。
// 写入成功后 t.ID / t.Status / t.CreatedAt 会被回填。
func (r *TaskRepo) Create(ctx context.Context, t *model.Task) error {
	err := r.db.WithContext(ctx).Create(t).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return model.ErrTaskExists // task_uid 撞了
	}
	// 目前 tasks.session_id 上还没建外键(Session 模型里没有 Tasks 关联),
	// 所以这个分支暂时不会触发;等加了外键就能直接生效。
	if errors.Is(err, gorm.ErrForeignKeyViolated) {
		return model.ErrSessionNotFound
	}
	return err
}

func (r *TaskRepo) GetByUID(ctx context.Context, taskUID string) (*model.Task, error) {
	var t model.Task
	err := r.db.WithContext(ctx).Where("task_uid = ?", taskUID).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, model.ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListBySession 按创建时间倒序列出会话内的任务。
// 排序方向跟 idx_tasks_session_created 一致,走索引不用额外排序。
func (r *TaskRepo) ListBySession(ctx context.Context, sessionID string, limit, offset int) ([]model.Task, error) {
	var ts []model.Task
	err := r.db.WithContext(ctx).
		Where("session_id = ?", sessionID).
		Order("created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&ts).Error
	if err != nil {
		return nil, err
	}
	return ts, nil
}

// MarkRunning 把任务从 pending 推到 running。
func (r *TaskRepo) MarkRunning(ctx context.Context, taskUID string) (bool, error) {
	return r.cas(ctx, taskUID, []string{model.TaskPending}, map[string]any{
		"status":     model.TaskRunning,
		"started_at": time.Now(),
	})
}

// MarkSucceeded 把任务标记为成功。output 是最终答复,传 nil 表示没有
// (落库 NULL 而不是空串,和 MarkFailed 的 errMsg 一个道理)。
func (r *TaskRepo) MarkSucceeded(ctx context.Context, taskUID string, output *string) (bool, error) {
	return r.cas(ctx, taskUID, activeStatuses, map[string]any{
		"status":      model.TaskSucceeded,
		"output":      output,
		"finished_at": time.Now(),
	})
}

// MarkFailed 把任务标记为失败。errMsg 传 nil 表示不记原因,
// 落库是 NULL 而不是空串。
func (r *TaskRepo) MarkFailed(ctx context.Context, taskUID string, errMsg *string) (bool, error) {
	return r.cas(ctx, taskUID, activeStatuses, map[string]any{
		"status":      model.TaskFailed,
		"error_msg":   errMsg,
		"finished_at": time.Now(),
	})
}

// MarkCanceled 把任务标记为已取消。reason 传 nil 表示用户没填理由,
// 落库是 NULL 而不是空串 —— 和 MarkFailed 的 errMsg 一个道理。
//
// from 和 MarkSucceeded / MarkFailed 完全一样,这不是巧合:
// 这三个都是「从中间态推到终态」,谁先到谁赢,后到的那个拿到 false。
// 用户点取消和 Agent 报完成撞在一起时,这一条 UPDATE 的行锁就是仲裁者,
// 不需要在上层加锁,也不需要先查再写。
func (r *TaskRepo) MarkCanceled(ctx context.Context, taskUID string, reason *string) (bool, error) {
	return r.cas(ctx, taskUID, activeStatuses, map[string]any{
		"status":        model.TaskCanceled,
		"cancel_reason": reason,
		"finished_at":   time.Now(),
	})
}

// cas 是三个状态流转的公共实现:只有当前状态在 from 里才更新。
// 判断和写入在同一条 UPDATE 里,靠行锁保证并发下只有一个人能推动状态,
// 不需要先查再写。
//
// 返回 false 表示没匹配到行 —— 任务不存在,或者已经是终态。
// 这通常是一个迟到的回调,不是故障,所以不当错误抛。
func (r *TaskRepo) cas(ctx context.Context, taskUID string, from []string, values map[string]any) (bool, error) {
	res := r.db.WithContext(ctx).Model(&model.Task{}).
		Where("task_uid = ? AND status IN ?", taskUID, from).
		Updates(values)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// activeStatuses 是「还能被推动」的状态。
// 单独拎出来是因为 ListStale 也要用它,而它必须和 cas 的 from 一致 ——
// 两边漂了的话,回收器会去捞一些它根本改不动的行,然后每一轮重复捞一次。
var activeStatuses = []string{model.TaskPending, model.TaskRunning}

// ListStale 列出卡住的任务:还在中间态,但 updated_at 已经早于 before。
//
// 这是 idx_tasks_status_updated 的第一个(也是目前唯一的)消费者 ——
// 那个索引本来就是为这条查询建的:status 等值在前、updated_at 范围在后,
// 正好是 B-tree 能一次走完的形状。
//
// 按 updated_at 升序:卡得最久的先处理。一次积压超过 limit 时,
// 这个顺序保证下一轮不会又从头捞同一批(它们已经被改成终态了)。
func (r *TaskRepo) ListStale(ctx context.Context, before time.Time, limit int) ([]model.Task, error) {
	if limit <= 0 {
		// 和 ListBySession 同一个坑:GORM 的 Limit(<=0) 是**不写 LIMIT 子句**,
		// 也就是把整张表捞进内存。回收器是后台循环,真发生了没人会看见。
		return nil, nil
	}

	var ts []model.Task
	err := r.db.WithContext(ctx).
		Where("status IN ? AND updated_at < ?", activeStatuses, before).
		Order("updated_at ASC").
		Limit(limit).
		Find(&ts).Error
	if err != nil {
		return nil, err
	}
	return ts, nil
}

// Touch 给任务续一次 updated_at,表示"它还活着"。
// 返回 false 表示这次没写(任务已是终态,或者离上次续命还不够久)。
//
// notBefore 是**节流阀**:只有 updated_at 早于它的行才会被更新。
// 节流放在 WHERE 里而不是内存里,是刻意的 ——
//
//   - 内存里记"上次 touch 时间"要按 task_uid 存一张表,还得清理;
//   - 多副本部署时每个副本各记各的,节流就失效了;
//   - 而这条 UPDATE 本身就是原子的,不需要额外同步。
//
// 代价是每次都发一条 SQL 出去,但那是一条走唯一索引的 UPDATE,
// 命中不了就是 0 行,比维护一份内存状态便宜得多。
//
// 只续 running,不续 pending:pending 意味着还没交接成功,
// 那正是回收器该来看一眼的状态,给它续命等于把问题永远藏起来。
func (r *TaskRepo) Touch(ctx context.Context, taskUID string, notBefore time.Time) (bool, error) {
	res := r.db.WithContext(ctx).Model(&model.Task{}).
		Where("task_uid = ? AND status = ? AND updated_at < ?",
			taskUID, model.TaskRunning, notBefore).
		Update("updated_at", time.Now())
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// CountByUserSince 统计用户在 since 之后创建的任务数,用于配额。
// 走 idx_tasks_user_created。
func (r *TaskRepo) CountByUserSince(ctx context.Context, userID int64, since time.Time) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&model.Task{}).
		Where("user_id = ? AND created_at >= ?", userID, since).
		Count(&n).Error
	if err != nil {
		return 0, err
	}
	return n, nil
}

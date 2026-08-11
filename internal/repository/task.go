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

// MarkSucceeded 把任务标记为成功。
func (r *TaskRepo) MarkSucceeded(ctx context.Context, taskUID string) (bool, error) {
	return r.cas(ctx, taskUID, []string{model.TaskPending, model.TaskRunning}, map[string]any{
		"status":      model.TaskSucceeded,
		"finished_at": time.Now(),
	})
}

// MarkFailed 把任务标记为失败。errMsg 传 nil 表示不记原因,
// 落库是 NULL 而不是空串。
func (r *TaskRepo) MarkFailed(ctx context.Context, taskUID string, errMsg *string) (bool, error) {
	return r.cas(ctx, taskUID, []string{model.TaskPending, model.TaskRunning}, map[string]any{
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
	return r.cas(ctx, taskUID, []string{model.TaskPending, model.TaskRunning}, map[string]any{
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

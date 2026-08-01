package repository

import (
	"context"
	"time"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"gorm.io/gorm"
)

type TaskRepo struct{ db *gorm.DB }

func (r *TaskRepo) Create(ctx context.Context, taskUID, sessionID string, userID int64, input *string) (*model.Task, error) {
	t := model.Task{TaskUID: taskUID, SessionID: sessionID, UserID: userID, Input: input}
	if err := r.db.WithContext(ctx).Create(&t).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

// MarkSucceeded 把任务标记为成功。返回 false 表示没有匹配到 pending/running 的行,
// 也就是一个迟到的回调 —— 调用方自己决定怎么处理,这不算错误。
func (r *TaskRepo) MarkSucceeded(ctx context.Context, uid string) (bool, error) {
	res := r.db.WithContext(ctx).Model(&model.Task{}).
		Where("task_uid = ? AND status IN ?", uid, []string{"pending", "running"}).
		Updates(map[string]any{"status": "succeeded", "finished_at": time.Now()})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

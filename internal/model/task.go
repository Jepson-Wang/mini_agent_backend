package model

import "time"

// Task 任务
type Task struct {
	ID         int64   `gorm:"primaryKey;autoIncrement"`
	TaskUID    string  `gorm:"type:char(26);not null;uniqueIndex:uk_tasks_uid"`
	SessionID  string  `gorm:"type:char(26);not null;index:idx_tasks_session_created,priority:1"`
	UserID     int64   `gorm:"not null;index:idx_tasks_user_created,priority:1"` // 冗余,鉴权/配额免 join
	Status     string  `gorm:"type:varchar(16);not null;default:pending;index:idx_tasks_status_updated,priority:1"`
	Input      *string `gorm:"type:text"` // 可空
	ErrorMsg   *string `gorm:"type:text"` // 可空:成功的任务这里就是 NULL,不是空串
	StartedAt  *time.Time
	FinishedAt *time.Time
	CreatedAt  time.Time `gorm:"not null;default:now();index:idx_tasks_session_created,priority:2,sort:desc;index:idx_tasks_user_created,priority:2,sort:desc"`
	UpdatedAt  time.Time `gorm:"not null;default:now();index:idx_tasks_status_updated,priority:2"`
}

func (Task) TableName() string { return "tasks" }

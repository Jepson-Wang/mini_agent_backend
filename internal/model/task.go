package model

import "time"

// Task 任务
type Task struct {
	ID        int64   `gorm:"primaryKey;autoIncrement"`
	TaskUID   string  `gorm:"type:char(26);not null;uniqueIndex:uk_tasks_uid"`
	SessionID string  `gorm:"type:char(26);not null;index:idx_tasks_session_created,priority:1"`
	UserID    int64   `gorm:"not null;index:idx_tasks_user_created,priority:1"` // 冗余,鉴权/配额免 join
	Status    string  `gorm:"type:varchar(16);not null;default:pending;index:idx_tasks_status_updated,priority:1"`
	Input     *string `gorm:"type:text"` // 可空
	ErrorMsg  *string `gorm:"type:text"` // 可空:成功的任务这里就是 NULL,不是空串

	// CancelReason 是用户点取消时填的理由,可空(没填就是 NULL,不是空串)。
	// 限长和 dto.CancelTaskReasonReq 的 max 对齐,超了会被 handler 挡在门外。
	CancelReason *string `gorm:"type:varchar(200)"`

	StartedAt  *time.Time
	FinishedAt *time.Time
	CreatedAt  time.Time `gorm:"not null;default:now();index:idx_tasks_session_created,priority:2,sort:desc;index:idx_tasks_user_created,priority:2,sort:desc"`
	UpdatedAt  time.Time `gorm:"not null;default:now();index:idx_tasks_status_updated,priority:2"`
}

func (Task) TableName() string { return "tasks" }

// 任务状态。落库是 varchar(16),别往里加更长的值。
//
// pending / running 是中间态,succeeded / failed / canceled 是终态。
// 「终态不再变」是整个状态机唯一的硬规则 —— repository 那几个 Mark* 方法
// 用一条带 status IN (...) 的 UPDATE 来兑现它,迟到的回调自然就落空了。
const (
	TaskPending   = "pending"
	TaskRunning   = "running"
	TaskSucceeded = "succeeded"
	TaskFailed    = "failed"
	TaskCanceled  = "canceled"
)

// IsTerminal 报告 status 是不是终态。
//
// 未知状态按「非终态」处理:真出现了库里存着没登记的值这种事,
// 让它还能被推动、被取消,好过卡死在那儿谁也动不了。
func IsTerminal(status string) bool {
	switch status {
	case TaskSucceeded, TaskFailed, TaskCanceled:
		return true
	default:
		return false
	}
}

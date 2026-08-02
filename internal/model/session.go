package model

import (
	"time"

	"gorm.io/gorm"
)

// Session 会话
type Session struct {
	SessionID string         `gorm:"type:char(26);primaryKey"`
	UserID    int64          `gorm:"not null;index:idx_sessions_user_updated,priority:1"`
	Status    string         `gorm:"type:varchar(16);not null;default:'active'"`
	Title     *string        `gorm:"type:varchar(128)"` // 可空:首轮对话出来后再回填
	CreatedAt time.Time      `gorm:"not null;default:now()"`
	UpdatedAt time.Time      `gorm:"not null;default:now();index:idx_sessions_user_updated,priority:2,sort:desc"`
	DeletedAt gorm.DeletedAt `gorm:"index"`
}

func (Session) TableName() string { return "sessions" }

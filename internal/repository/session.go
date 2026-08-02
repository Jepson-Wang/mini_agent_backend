package repository

import (
	"context"
	"errors"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ulid"
	"gorm.io/gorm"
)

type SessionRepo struct{ db *gorm.DB }

// Create 新建一个会话,session_id 内部用 ULID 生成。
// title 传 nil 表示还没有标题,首轮对话出来后再回填。
func (r *SessionRepo) Create(ctx context.Context, userID int64, title *string) (*model.Session, error) {
	sessionID, err := ulid.New()
	if err != nil {
		return nil, err
	}
	s := model.Session{SessionID: sessionID, UserID: userID, Title: title}
	err = r.db.WithContext(ctx).Create(&s).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return nil, model.ErrSessionExists
	}
	if errors.Is(err, gorm.ErrForeignKeyViolated) {
		return nil, model.ErrUserNotFound // user_id 指向的用户不存在
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *SessionRepo) ListByUser(ctx context.Context, userID int64, limit, offset int) ([]model.Session, error) {
	var ss []model.Session
	err := r.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("updated_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&ss).Error
	return ss, err
}

func (r *SessionRepo) GetByID(ctx context.Context, SessionID string) (*model.Session, error) {
	var ss model.Session
	err := r.db.WithContext(ctx).Where("session_id = ?", SessionID).First(&ss).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, model.ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ss, nil
}

func (r *SessionRepo) SoftDelete(ctx context.Context, sessionID string) error {
	return r.db.WithContext(ctx).Delete(&model.Session{SessionID: sessionID}).Error
}

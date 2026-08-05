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

// 分页上限。夹在仓库层而不是 handler:所有调用方都从这儿过,
// 放上层的话有一个忘了校验就是一次全表读。
const (
	defaultListLimit = 20
	maxListLimit     = 100
)

// ListByUser 按最近活跃倒序列出某个用户的会话。软删除的行由 GORM 自动排除。
//
// 查不到不是错误:Find 空结果返回的是 nil error 加一个空切片,
// 只有 First/Take/Last 才给 ErrRecordNotFound。别在这儿翻译成
// ErrSessionNotFound —— 空列表是一个合法的结果。
func (r *SessionRepo) ListByUser(ctx context.Context, userID int64, limit, offset int) ([]model.Session, error) {
	// GORM 的 Limit(负数) 会整个不写 LIMIT 子句(clause/limit.go:16),
	// 也就是全表;Limit(0) 则是 LIMIT 0,静默返回空。两个都得挡下来 ——
	// handler 那边迟早是 strconv.Atoi(c.Query("limit")),一个 ?limit=-1
	// 就能把这个用户的全部会话读进内存。
	switch {
	case limit <= 0:
		limit = defaultListLimit
	case limit > maxListLimit:
		limit = maxListLimit
	}
	if offset < 0 {
		offset = 0
	}

	var ss []model.Session
	err := r.db.WithContext(ctx).
		Where("user_id = ?", userID).
		// session_id 是次级排序键。只按 updated_at 排的话,时间相同的几行
		// 顺序不确定,而翻页是分多次查的 —— 两次之间顺序一变就会漏行或重行,
		// 本地基本复现不出来。ULID 字典序即时间序,方向和 updated_at 一致。
		Order("updated_at DESC, session_id DESC").
		Limit(limit).
		Offset(offset).
		Find(&ss).Error
	if err != nil {
		// 出错就返回零值,不把可能半填充的切片一起交出去。
		return nil, err
	}
	return ss, nil
}

// GetByID 按 session_id 取会话。
//
// 注意这里不校验归属:任何人拿到 session_id 都能读到。调用方必须比对
// UserID,否则就是越权读取。session_id 猜不出来,但它会出现在 URL、
// 日志和 Referer 里,不能当密码用。
func (r *SessionRepo) GetByID(ctx context.Context, sessionID string) (*model.Session, error) {
	var ss model.Session
	err := r.db.WithContext(ctx).Where("session_id = ?", sessionID).First(&ss).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, model.ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ss, nil
}

// UpdateTheTitle 改标题。title 传 nil 表示清空,落库是 NULL。
//
// 用 Update("title", ...) 而不是 Updates(结构体):后者会跳过零值字段,
// 想把标题改回 NULL 的话那条 UPDATE 会变成一条什么都不改的语句。
//
// 改完重新查一次再返回。直接把入参拼成结构体返回是不行的 ——
// 那份数据里只有你刚写进去的那一列,session_id / user_id / created_at
// 全是零值,上层拿去序列化就是一个空壳会话。
func (r *SessionRepo) UpdateTheTitle(ctx context.Context, sessionID string, title *string) (*model.Session, error) {
	res := r.db.WithContext(ctx).Model(&model.Session{}).
		Where("session_id = ?", sessionID).
		Update("title", title)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		// 会话不存在,或者已经被软删除(deleted_at IS NULL 是 GORM 自动加的)。
		// 不检查这个的话,改一个不存在的会话会静默"成功"。
		return nil, model.ErrSessionNotFound
	}
	return r.GetByID(ctx, sessionID)
}

// SoftDelete 软删除会话。删一个本来就不存在(或已删)的返回 ErrSessionNotFound。
func (r *SessionRepo) SoftDelete(ctx context.Context, sessionID string) error {
	res := r.db.WithContext(ctx).Delete(&model.Session{SessionID: sessionID})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return model.ErrSessionNotFound
	}
	return nil
}

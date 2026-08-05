package repository

import (
	"context"
	"errors"
	"time"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"gorm.io/gorm"
)

type UserRepo struct{ db *gorm.DB }

// Create 落一个新用户。nickname 传 nil 表示不设置(列可空)。
//
// 调用方负责把 email 规范化(小写、去空白),这里不做 —— 规范化是业务规则,
// 放在仓库层的话,别处直接查库时用的又是另一套,唯一索引照样会被绕过。
func (r *UserRepo) Create(ctx context.Context, email, hash string, nickname *string) (*model.User, error) {
	u := model.User{Email: email, PasswordHash: hash, Nickname: nickname}
	err := r.db.WithContext(ctx).Create(&u).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return nil, model.ErrEmailOccupied
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// TouchLastLogin 记一次登录时间。
func (r *UserRepo) TouchLastLogin(ctx context.Context, id int64, at time.Time) error {
	res := r.db.WithContext(ctx).Model(&model.User{}).
		Where("id = ?", id).
		Update("last_login_at", at)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return model.ErrUserNotFound
	}
	return nil
}

// UpdatePasswordHash 换掉口令散列。
//
// 两处会用到:用户主动改密码,以及登录时发现旧散列的 cost 低于当前配置
// (hash.NeedsRehash)。后者是趁用户手上正好有明文的唯一时机做迁移 ——
// 散列不可逆,离线批量升级是做不到的。
func (r *UserRepo) UpdatePasswordHash(ctx context.Context, id int64, hash string) error {
	if hash == "" {
		// 写空串不会报错,但会把账号变成"任何口令都对不上",而且没人会发现。
		return errors.New("repository: 口令散列不能为空")
	}
	res := r.db.WithContext(ctx).Model(&model.User{}).
		Where("id = ?", id).
		Update("password_hash", hash)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return model.ErrUserNotFound
	}
	return nil
}

func (r *UserRepo) GetByEmail(ctx context.Context, email string) (*model.User, error) {
	var u model.User
	err := r.db.WithContext(ctx).Where("email = ?", email).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, model.ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (r *UserRepo) GetByID(ctx context.Context, id int64) (*model.User, error) {
	var u model.User
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, model.ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

package model

import "time"

// User 用户
type User struct {
	ID           int64   `gorm:"primaryKey"`
	Email        string  `gorm:"type:varchar(255);not null;uniqueIndex:uk_users_email"`
	PasswordHash string  `gorm:"type:varchar(255);not null"`
	Nickname     *string `gorm:"type:varchar(64)"` // 可空
	Status       string  `gorm:"type:varchar(16);not null;default:active"`
	LastLoginAt  *time.Time
	CreatedAt    time.Time // ← 约定字段,自动填充
	UpdatedAt    time.Time // ← 约定字段,自动更新
	Sessions     []Session `gorm:"foreignKey:UserID;constraint:OnDelete:RESTRICT"`
}

func (User) TableName() string { return "users" }

// Equal 比较两个 User 是否是同一条记录的相同快照,
// 典型用法是把新建的返回值和从库里读回来的值对上。
// 因为后者多走了一趟数据库,所以:
//   - 时间用 time.Time.Equal 比瞬时,不受时区和单调钟影响;
//   - 时间先截断到微秒 —— 写入时填的是 Go 的纳秒时间戳,
//     进了 timestamptz 只留微秒,读回来必然掉精度;
//   - 关联字段 Sessions 不参与比较(仓库层都没有 Preload)。
func (u *User) Equal(c *User) bool {
	if u == nil || c == nil {
		return u == c
	}
	return u.ID == c.ID &&
		u.Email == c.Email &&
		u.PasswordHash == c.PasswordHash &&
		equalPtr(u.Nickname, c.Nickname) &&
		u.Status == c.Status &&
		equalTimePtr(u.LastLoginAt, c.LastLoginAt) &&
		equalTime(u.CreatedAt, c.CreatedAt) &&
		equalTime(u.UpdatedAt, c.UpdatedAt)
}

// equalPtr 比较两个可空字段:同为 nil 算相等,一个 nil 一个非 nil 算不等。
func equalPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// equalTime 按 Postgres timestamptz 的精度(微秒)比较两个时间点。
func equalTime(a, b time.Time) bool {
	return a.Truncate(time.Microsecond).Equal(b.Truncate(time.Microsecond))
}

func equalTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return equalTime(*a, *b)
}

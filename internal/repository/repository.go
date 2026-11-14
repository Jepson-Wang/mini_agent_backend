package repository

import (
	"context"

	"gorm.io/gorm"
)

// Repository 聚合所有数据访问入口,上层拿一个它就够了。
type Repository struct {
	db *gorm.DB

	User      *UserRepo
	Session   *SessionRepo
	Task      *TaskRepo
	TaskEvent *TaskEventRepo
}

func New(db *gorm.DB) *Repository {
	return &Repository{
		db:        db,
		User:      &UserRepo{db: db},
		Session:   &SessionRepo{db: db},
		Task:      &TaskRepo{db: db},
		TaskEvent: &TaskEventRepo{db: db},
	}
}

// DB 暴露底层句柄,给 AutoMigrate 和测试用。业务代码不该碰它。
func (r *Repository) DB() *gorm.DB { return r.db }

// WithTx 返回一个绑定在事务上的 Repository 副本。
func (r *Repository) WithTx(tx *gorm.DB) *Repository { return New(tx) }

// Transaction 在一个事务里执行 fn,fn 拿到的 Repository 已经绑定到该事务,
// 所以里面调的每个仓库方法都走同一条连接。fn 返回 error 即回滚。
//
// 如果 r 本身已经绑在事务上,gorm 会开一个 SAVEPOINT 而不是嵌套事务,
// 这样测试可以把整个用例包在一个外层事务里再回滚掉。
func (r *Repository) Transaction(ctx context.Context, fn func(*Repository) error) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(r.WithTx(tx))
	})
}

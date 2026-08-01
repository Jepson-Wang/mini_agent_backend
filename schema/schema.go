package schema

import (
	"context"
	crand "crypto/rand"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
)

// ulidEntropy 是 ULID 的熵源。这里刻意不用 ulid.Make():它内置的熵源是
// math/rand 加时间戳种子(ulid.go:135-138),生成的 ID 可预测。
// session_id / task_uid 会出现在 API 里,必须走 crypto/rand。
var ulidEntropy = &ulid.LockedMonotonicReader{
	MonotonicReader: ulid.Monotonic(crand.Reader, 0),
}

// NewULID 生成一个 26 位 ULID。前 10 位是毫秒时间戳,所以字典序即时间序,
// 拿它当主键时 B-tree 是顺序追加的,不会像 UUIDv4 那样把索引写得到处是空洞。
func NewULID() (string, error) {
	id, err := ulid.New(ulid.Now(), ulidEntropy)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// User 用户
type User struct {
	ID           int64  `gorm:"primaryKey"`
	Email        string `gorm:"type:varchar(255);not null;uniqueIndex:uk_users_email"`
	PasswordHash string `gorm:"type:varchar(255);not null"`
	Nickname     string `gorm:"type:varchar(64)"`
	Status       string `gorm:"type:varchar(16);not null;default:active"`
	LastLoginAt  *time.Time
	CreatedAt    time.Time // ← 约定字段,自动填充
	UpdatedAt    time.Time // ← 约定字段,自动更新
	Sessions     []Session `gorm:"foreignKey:UserID;constraint:OnDelete:RESTRICT"`
}

func (User) TableName() string { return "users" }

// Equal 比较两个 User 是否是同一条记录的相同快照,
// 典型用法是把 CreateUser 的返回值和 GetUserByEmail 读回来的值对上。
// 因为后者多走了一趟数据库,所以:
//   - 时间用 time.Time.Equal 比瞬时,不受时区和单调钟影响;
//   - 时间先截断到微秒 —— Create 填的是 Go 的纳秒时间戳,
//     写进 timestamptz 只留微秒,读回来必然掉精度;
//   - 关联字段 Sessions 不参与比较(这两个函数都没 Preload)。
func (u *User) Equal(c *User) bool {
	if u == nil || c == nil {
		return u == c
	}
	return u.ID == c.ID &&
		u.Email == c.Email &&
		u.PasswordHash == c.PasswordHash &&
		u.Nickname == c.Nickname &&
		u.Status == c.Status &&
		equalTimePtr(u.LastLoginAt, c.LastLoginAt) &&
		equalTime(u.CreatedAt, c.CreatedAt) &&
		equalTime(u.UpdatedAt, c.UpdatedAt)
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

// Session 会话
type Session struct {
	SessionID string         `gorm:"type:char(26);primaryKey"`
	UserID    int64          `gorm:"not null;index:idx_sessions_user_updated,priority:1"`
	Status    string         `gorm:"type:varchar(16);not null;default:'active'"`
	Title     string         `gorm:"type:varchar(128);not null;default:''"`
	CreatedAt time.Time      `gorm:"not null;default:now()"`
	UpdatedAt time.Time      `gorm:"not null;default:now();index:idx_sessions_user_updated,priority:2,sort:desc"`
	DeletedAt gorm.DeletedAt `gorm:"index"`
}

func (Session) TableName() string { return "sessions" }

// Task 任务
type Task struct {
	ID         int64  `gorm:"primaryKey;autoIncrement"`
	TaskUID    string `gorm:"type:char(26);not null;uniqueIndex:uk_tasks_uid"`
	SessionID  string `gorm:"type:char(26);not null;index:idx_tasks_session_created,priority:1"`
	UserID     int64  `gorm:"not null;index:idx_tasks_user_created,priority:1"` // 冗余,鉴权/配额免 join
	Status     string `gorm:"type:varchar(16);not null;default:pending;index:idx_tasks_status_updated,priority:1"`
	Input      string `gorm:"type:text"`
	ErrorMsg   string `gorm:"type:text"`
	StartedAt  *time.Time
	FinishedAt *time.Time
	CreatedAt  time.Time `gorm:"not null;default:now();index:idx_tasks_session_created,priority:2,sort:desc;index:idx_tasks_user_created,priority:2,sort:desc"`
	UpdatedAt  time.Time `gorm:"not null;default:now();index:idx_tasks_status_updated,priority:2"`
}

func (Task) TableName() string { return "tasks" }

var (
	ErrEmailOccupied = errors.New("email or password is not valid")
	ErrUserNotFound  = errors.New("no users found")
	ErrSessionExists = errors.New("session already exists")
)

func CreateUser(ctx context.Context, db *gorm.DB, email, hash string) (*User, error) {
	u := User{Email: email, PasswordHash: hash}
	err := db.WithContext(ctx).Create(&u).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return nil, ErrEmailOccupied
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func GetUserByEmail(ctx context.Context, db *gorm.DB, email string) (*User, error) {
	var user User
	err := db.WithContext(ctx).Where("email = ?", email).First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// CreateSession 新建一个会话,session_id 内部用 ULID 生成。
// title 允许为空 —— 列上有空字符串默认值,首轮对话出来后再回填。
func CreateSession(ctx context.Context, db *gorm.DB, userID int64, title string) (*Session, error) {
	sessionID, err := NewULID()
	if err != nil {
		return nil, err
	}
	s := Session{SessionID: sessionID, UserID: userID, Title: title}
	err = db.WithContext(ctx).Create(&s).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return nil, ErrSessionExists
	}
	if errors.Is(err, gorm.ErrForeignKeyViolated) {
		return nil, ErrUserNotFound // user_id 指向的用户不存在
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func ListSessions(ctx context.Context, db *gorm.DB, userID int64, limit, offset int) ([]Session, error) {
	var ss []Session
	err := db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("updated_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&ss).Error
	return ss, err
}

func CreateTask(ctx context.Context, db *gorm.DB, taskUID, sessionID string, userID int64, input string) (*Task, error) {
	task := Task{TaskUID: taskUID, SessionID: sessionID, UserID: userID, Input: input}
	err := db.WithContext(ctx).Create(&task).Error
	if err != nil {
		return nil, err
	}
	return &task, nil
}

func MarkTaskSucceeded(ctx context.Context, db *gorm.DB, uid string) (bool, error) {
	r := db.WithContext(ctx).Model(&Task{}).
		Where("task_uid = ? AND status IN ?", uid, []string{"pending", "running"}).
		Updates(map[string]any{"status": "succeeded", "finished_at": time.Now()})
	if r.Error != nil {
		return false, r.Error
	}
	return r.RowsAffected > 0, nil // false = 迟到的回调,调用方自己决定怎么处理
}

func CreateSessionWithFirstTask(ctx context.Context, db *gorm.DB,
	sessionID, taskUID string, userID int64, title, input string) (*Session, *Task, error) {

	s := &Session{SessionID: sessionID, UserID: userID, Title: title}
	t := &Task{TaskUID: taskUID, SessionID: sessionID, UserID: userID, Input: input}

	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(s).Error; err != nil {
			return err
		}
		return tx.Create(t).Error
	})
	if err != nil {
		return nil, nil, err
	}
	return s, t, nil
}

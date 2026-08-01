package repository

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"testing"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ptr"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/ulid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// testDB 是整个包共用的连接。没有配 TEST_DSN 时它是 nil,
// withTx 会把用例跳过,这样 CI 上没有 Postgres 也不会红。
var testDB *gorm.DB

// TestMain 连测试库并跑迁移。
//
// 跑法(PowerShell):
//
//	$env:TEST_DSN="host=localhost user=postgres password=123456 dbname=mini_agent_test port=5432 sslmode=disable"
//	go test ./internal/repository/ -v
//
// 请单独建一个测试库,别指向开发库 —— AutoMigrate 会改表结构。
func TestMain(m *testing.M) {
	if dsn := os.Getenv("TEST_DSN"); dsn != "" {
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
			Logger: logger.Default.LogMode(logger.Silent),
			// 下面那些 errors.Is(err, model.ErrXxx) 的断言全靠它,
			// 关掉的话拿到的是裸的 pgx 错误,用例会全挂。
			TranslateError: true,
		})
		if err != nil {
			log.Fatalf("连接测试库失败: %v", err)
		}
		if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.Task{}); err != nil {
			log.Fatalf("测试库迁移失败: %v", err)
		}
		testDB = db
	}
	os.Exit(m.Run())
}

// withTx 把一个用例跑在独立事务里,结束后一律回滚。
// 好处是用例之间完全隔离,也不会在库里留垃圾数据,不需要写清理代码。
func withTx(t *testing.T, fn func(t *testing.T, r *Repository)) {
	t.Helper()
	if testDB == nil {
		t.Skip("未设置 TEST_DSN,跳过需要数据库的用例")
	}
	tx := testDB.Begin()
	if tx.Error != nil {
		t.Fatalf("开启事务失败: %v", tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
	fn(t, New(tx))
}

var emailSeq atomic.Int64

// uniqueEmail 每次返回不同的邮箱。用例本来就跑在会回滚的事务里,
// 但万一将来有人去掉回滚,唯一约束也不会互相打架。
func uniqueEmail() string {
	return fmt.Sprintf("u%d@test.local", emailSeq.Add(1))
}

func mustUser(t *testing.T, r *Repository) *model.User {
	t.Helper()
	u, err := r.User.Create(context.Background(), uniqueEmail(), "hash")
	if err != nil {
		t.Fatalf("准备用户数据失败: %v", err)
	}
	return u
}

// ---- UserRepo ----

// 这条用例真正在验的是 model.User.Equal:写入时的时间戳是 Go 的纳秒精度,
// 读回来的是 timestamptz 的微秒精度,直接用 == 比会挂。
func TestUserRepo_CreateAndGetByEmail(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		email := uniqueEmail()

		created, err := r.User.Create(ctx, email, "hash")
		if err != nil {
			t.Fatalf("创建用户: %v", err)
		}

		got, err := r.User.GetByEmail(ctx, email)
		if err != nil {
			t.Fatalf("按邮箱查询: %v", err)
		}

		if !created.Equal(got) {
			t.Errorf("写入和读回的用户不一致\n写入: %+v\n读回: %+v", created, got)
		}
		if got.Nickname != nil {
			t.Errorf("没传昵称时应该是 nil,实际是 %q", *got.Nickname)
		}
		if got.Status != "active" {
			t.Errorf("Status 默认值应为 active,实际是 %q", got.Status)
		}
	})
}

func TestUserRepo_Create_DuplicateEmail(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		email := uniqueEmail()

		if _, err := r.User.Create(ctx, email, "hash"); err != nil {
			t.Fatalf("首次创建: %v", err)
		}

		// 注意:唯一约束冲突会让整个事务进入 aborted 状态,
		// 之后任何语句都会报 "current transaction is aborted"。
		// 所以这种预期失败的调用必须放在用例最后。
		_, err := r.User.Create(ctx, email, "hash")
		if !errors.Is(err, model.ErrEmailOccupied) {
			t.Fatalf("期望 ErrEmailOccupied,实际是 %v", err)
		}
	})
}

func TestUserRepo_GetByEmail_NotFound(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		_, err := r.User.GetByEmail(context.Background(), "nobody@test.local")
		if !errors.Is(err, model.ErrUserNotFound) {
			t.Fatalf("期望 ErrUserNotFound,实际是 %v", err)
		}
	})
}

// ---- SessionRepo ----

// 验证可空字段的往返:不传 title 读回来必须是 nil,而不是指向空串的指针。
func TestSessionRepo_Create_NilTitleStaysNil(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		u := mustUser(t, r)

		created, err := r.Session.Create(ctx, u.ID, nil)
		if err != nil {
			t.Fatalf("创建会话: %v", err)
		}
		if created.Title != nil {
			t.Errorf("刚创建时 Title 应为 nil,实际是 %q", *created.Title)
		}
		if len(created.SessionID) != 26 {
			t.Errorf("session_id 应为 26 位 ULID,实际是 %q(%d 位)", created.SessionID, len(created.SessionID))
		}

		list, err := r.Session.ListByUser(ctx, u.ID, 10, 0)
		if err != nil {
			t.Fatalf("列出会话: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("期望 1 个会话,实际 %d 个", len(list))
		}
		if list[0].Title != nil {
			t.Errorf("从库里读回的 Title 应为 nil,实际是 %q", *list[0].Title)
		}
		if list[0].Status != "active" {
			t.Errorf("Status 默认值应为 active,实际是 %q", list[0].Status)
		}
	})
}

func TestSessionRepo_Create_TitleRoundTrip(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		u := mustUser(t, r)

		if _, err := r.Session.Create(ctx, u.ID, ptr.To("第一个会话")); err != nil {
			t.Fatalf("创建会话: %v", err)
		}

		list, err := r.Session.ListByUser(ctx, u.ID, 10, 0)
		if err != nil {
			t.Fatalf("列出会话: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("期望 1 个会话,实际 %d 个", len(list))
		}
		if list[0].Title == nil || *list[0].Title != "第一个会话" {
			t.Errorf("Title 没有正确往返,实际是 %v", list[0].Title)
		}
	})
}

// user_id 指向不存在的用户时,外键冲突要被翻译成 ErrUserNotFound。
// 这条用例同时守着 model.User.Sessions 那个关联字段 —— 删掉它外键就没了,
// 插入会静默成功,这条用例会挂。
func TestSessionRepo_Create_UnknownUser(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		_, err := r.Session.Create(context.Background(), 999999999, nil)
		if !errors.Is(err, model.ErrUserNotFound) {
			t.Fatalf("期望 ErrUserNotFound,实际是 %v", err)
		}
	})
}

// ---- TaskRepo ----

// 第二次标记应该返回 false 而不是报错 —— 这是迟到的回调,不是故障。
func TestTaskRepo_MarkSucceeded_LateCallback(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		u := mustUser(t, r)

		sess, err := r.Session.Create(ctx, u.ID, nil)
		if err != nil {
			t.Fatalf("创建会话: %v", err)
		}
		taskUID, err := ulid.New()
		if err != nil {
			t.Fatalf("生成 task_uid: %v", err)
		}
		task, err := r.Task.Create(ctx, taskUID, sess.SessionID, u.ID, ptr.To("你好"))
		if err != nil {
			t.Fatalf("创建任务: %v", err)
		}
		if task.Status != "pending" {
			t.Errorf("新任务状态应为 pending,实际是 %q", task.Status)
		}
		if task.ErrorMsg != nil {
			t.Errorf("新任务 ErrorMsg 应为 nil,实际是 %q", *task.ErrorMsg)
		}

		ok, err := r.Task.MarkSucceeded(ctx, taskUID)
		if err != nil {
			t.Fatalf("首次标记成功: %v", err)
		}
		if !ok {
			t.Fatal("首次标记应该匹配到 1 行")
		}

		ok, err = r.Task.MarkSucceeded(ctx, taskUID)
		if err != nil {
			t.Fatalf("重复标记不该报错: %v", err)
		}
		if ok {
			t.Error("重复标记应该返回 false(迟到的回调)")
		}
	})
}

// ---- 事务 ----

// Repository.Transaction 在已有事务里会开 SAVEPOINT,
// fn 返回错误时只回滚到保存点,外层事务还能继续用。
func TestRepository_Transaction_RollsBackOnError(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		email := uniqueEmail()
		sentinel := errors.New("故意失败")

		err := r.Transaction(ctx, func(tr *Repository) error {
			if _, err := tr.User.Create(ctx, email, "hash"); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("期望拿回 fn 的错误,实际是 %v", err)
		}

		// 回滚生效的话,刚才那个用户不该存在
		if _, err := r.User.GetByEmail(ctx, email); !errors.Is(err, model.ErrUserNotFound) {
			t.Fatalf("事务应该已回滚,但用户还在(err=%v)", err)
		}
	})
}

func TestRepository_Transaction_CommitsOnSuccess(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		email := uniqueEmail()

		err := r.Transaction(ctx, func(tr *Repository) error {
			_, err := tr.User.Create(ctx, email, "hash")
			return err
		})
		if err != nil {
			t.Fatalf("事务不该失败: %v", err)
		}

		if _, err := r.User.GetByEmail(ctx, email); err != nil {
			t.Fatalf("提交后应该能查到用户: %v", err)
		}
	})
}

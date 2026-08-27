package repository

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

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
			fatal("连接测试库失败", err)
		}
		if err := migrateTestDB(db); err != nil {
			fatal("测试库迁移失败", err)
		}
		testDB = db
	}
	requireDBOrExplicitSkip("repository")
	os.Exit(m.Run())
}

// allowSkipDBEnv 是"我知道这次不跑数据库用例"的显式承认。
const allowSkipDBEnv = "ALLOW_SKIP_DB"

// requireDBOrExplicitSkip 在既没有 TEST_DSN、也没有显式承认时直接让本包失败。
//
// 这条看着挺凶,但它修的是一个真实的坑:本包大多数用例都要库,没有 TEST_DSN
// 时它们全被 t.Skip 掉 —— 而 go test 不带 -v 时**不打印 skip,也不打印通过的
// 包的任何输出**。也就是说"全都跑过了"和"一条都没跑"在终端上都是一行
//
//	ok  .../internal/service  0.3s
//
// 完全分不出来。想靠打一条醒目的横幅提醒是没用的,那条横幅同样会被吞掉
// (试过了)。go test 在非 -v 模式下唯一保证会显示的东西,就是失败。
//
// 而被静默跳掉的里面有 CLAUDE.md 点名"删了就会出问题"的安全不变量用例:
// 登录失败不可区分、被禁用账号的校验顺序、取消时别人的任务要看起来像不存在、
// 迟到的回调不能复活已取消的任务、回调不许写 canceled。这些恰恰是最不该在
// "看起来是绿的"状态下被跳过的。
//
// 所以取向和这个项目里其它地方一致:宁可响亮地拒绝,也不要"能跑但没防护"。
// 不跑库用例仍然是完全正当的选择,只是得说出来 —— make test 会替你说。
func requireDBOrExplicitSkip(pkg string) {
	if testDB != nil || os.Getenv(allowSkipDBEnv) != "" {
		return
	}
	fatal(fmt.Sprintf(`%s 包:没有 TEST_DSN,需要数据库的用例一条都不会跑。

这里拒绝直接给一个 "ok" —— 那和"全部通过"在终端上长得一模一样,
而被跳掉的用例里包含 CLAUDE.md 点名的安全不变量。

三选一:
  make test-db                真跑一遍(推荐)
  make test                   承认跳过,它会替你设 %s=1
  %s=1 go test ./...   同上,手动版`,
		pkg, allowSkipDBEnv, allowSkipDBEnv), errors.New("缺少 TEST_DSN"))
}

// fatal 打一条 ERROR 然后退出。
//
// TestMain 里没有 *testing.T,t.Fatalf 用不了;而这几处失败都发生在
// m.Run() 之前,一条用例都还没跑起来,只能直接退。
func fatal(msg string, err error) {
	slog.Error(msg, "error", err)
	os.Exit(1)
}

// migrateTestDB 在一把 advisory 锁里跑 AutoMigrate。
//
// repository 和 service 两个包的 TestMain 各跑一次迁移,而 go test ./...
// 是并行跑包的。平时两边都是空操作看不出问题,一旦有新列,两条
// ALTER TABLE ... ADD COLUMN 撞在一起就会有一个拿到
// 42701(column already exists),整个包直接 log.Fatal。
//
// 用 pg_advisory_xact_lock 而不是 pg_advisory_lock:后者是会话级的,
// 连接被还回池里之后锁还在,得自己配对解锁;前者在事务结束时自动释放。
// 常量是随手取的,只要两个包用的是同一个就行。
func migrateTestDB(db *gorm.DB) error {
	const migrateLockKey = 0x6D696E69 // "mini"

	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", migrateLockKey).Error; err != nil {
			return err
		}
		return tx.AutoMigrate(&model.User{}, &model.Session{}, &model.Task{})
	})
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
	u, err := r.User.Create(context.Background(), uniqueEmail(), "hash", nil)
	if err != nil {
		t.Fatalf("准备用户数据失败: %v", err)
	}
	return u
}

func mustSession(t *testing.T, r *Repository, userID int64) *model.Session {
	t.Helper()
	s, err := r.Session.Create(context.Background(), userID, nil)
	if err != nil {
		t.Fatalf("准备会话数据失败: %v", err)
	}
	return s
}

// mustTask 造一个任务。createdAt 传零值就用当前时间。
// 需要断言排序的用例请显式传时间 —— time.Now() 在 Windows 上分辨率不够,
// 连着建两条可能拿到同一个微秒值,顺序就不确定了。
func mustTask(t *testing.T, r *Repository, sess *model.Session, userID int64, createdAt time.Time) *model.Task {
	t.Helper()
	uid, err := ulid.New()
	if err != nil {
		t.Fatalf("生成 task_uid: %v", err)
	}
	task := &model.Task{TaskUID: uid, SessionID: sess.SessionID, UserID: userID, CreatedAt: createdAt}
	if err := r.Task.Create(context.Background(), task); err != nil {
		t.Fatalf("准备任务数据失败: %v", err)
	}
	return task
}

// ---- UserRepo ----

// 这条用例真正在验的是 model.User.Equal:写入时的时间戳是 Go 的纳秒精度,
// 读回来的是 timestamptz 的微秒精度,直接用 == 比会挂。
func TestUserRepo_CreateAndGetByEmail(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		email := uniqueEmail()

		created, err := r.User.Create(ctx, email, "hash", nil)
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

		if _, err := r.User.Create(ctx, email, "hash", nil); err != nil {
			t.Fatalf("首次创建: %v", err)
		}

		// 注意:唯一约束冲突会让整个事务进入 aborted 状态,
		// 之后任何语句都会报 "current transaction is aborted"。
		// 所以这种预期失败的调用必须放在用例最后。
		_, err := r.User.Create(ctx, email, "hash", nil)
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
		task := &model.Task{TaskUID: taskUID, SessionID: sess.SessionID, UserID: u.ID, Input: ptr.To("你好")}
		if err := r.Task.Create(ctx, task); err != nil {
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

// MarkRunning 只认 pending,已经在跑的任务不该被重复推进。
func TestTaskRepo_MarkRunning_OnlyFromPending(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		u := mustUser(t, r)
		task := mustTask(t, r, mustSession(t, r, u.ID), u.ID, time.Time{})

		ok, err := r.Task.MarkRunning(ctx, task.TaskUID)
		if err != nil {
			t.Fatalf("首次标记运行中: %v", err)
		}
		if !ok {
			t.Fatal("pending 的任务应该能推到 running")
		}

		got, err := r.Task.GetByUID(ctx, task.TaskUID)
		if err != nil {
			t.Fatalf("查询任务: %v", err)
		}
		if got.Status != "running" {
			t.Errorf("状态应为 running,实际是 %q", got.Status)
		}
		if got.StartedAt == nil {
			t.Error("StartedAt 应该被填上")
		}
		if got.FinishedAt != nil {
			t.Error("还没结束,FinishedAt 应为 nil")
		}

		ok, err = r.Task.MarkRunning(ctx, task.TaskUID)
		if err != nil {
			t.Fatalf("重复标记不该报错: %v", err)
		}
		if ok {
			t.Error("已经是 running 了,应该返回 false")
		}
	})
}

// running -> failed,顺带验证 ErrorMsg 这个可空字段的往返。
func TestTaskRepo_MarkFailed(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		u := mustUser(t, r)
		task := mustTask(t, r, mustSession(t, r, u.ID), u.ID, time.Time{})

		if _, err := r.Task.MarkRunning(ctx, task.TaskUID); err != nil {
			t.Fatalf("标记运行中: %v", err)
		}

		ok, err := r.Task.MarkFailed(ctx, task.TaskUID, ptr.To("模型超时"))
		if err != nil {
			t.Fatalf("标记失败: %v", err)
		}
		if !ok {
			t.Fatal("running 的任务应该能推到 failed")
		}

		got, err := r.Task.GetByUID(ctx, task.TaskUID)
		if err != nil {
			t.Fatalf("查询任务: %v", err)
		}
		if got.Status != "failed" {
			t.Errorf("状态应为 failed,实际是 %q", got.Status)
		}
		if got.ErrorMsg == nil || *got.ErrorMsg != "模型超时" {
			t.Errorf("ErrorMsg 没有正确往返,实际是 %v", got.ErrorMsg)
		}
		if got.FinishedAt == nil {
			t.Error("FinishedAt 应该被填上")
		}

		// failed 是终态,不能再被标记成功
		if ok, err := r.Task.MarkSucceeded(ctx, task.TaskUID); err != nil || ok {
			t.Errorf("终态不该被推翻,ok=%v err=%v", ok, err)
		}
	})
}

func TestTaskRepo_GetByUID_NotFound(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		_, err := r.Task.GetByUID(context.Background(), "00000000000000000000000000")
		if !errors.Is(err, model.ErrTaskNotFound) {
			t.Fatalf("期望 ErrTaskNotFound,实际是 %v", err)
		}
	})
}

func TestTaskRepo_ListBySession(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		u := mustUser(t, r)
		sess := mustSession(t, r, u.ID)
		other := mustSession(t, r, u.ID)

		now := time.Now()
		oldest := mustTask(t, r, sess, u.ID, now.Add(-2*time.Minute))
		middle := mustTask(t, r, sess, u.ID, now.Add(-time.Minute))
		newest := mustTask(t, r, sess, u.ID, now)
		mustTask(t, r, other, u.ID, now) // 别的会话,不该被捞出来

		list, err := r.Task.ListBySession(ctx, sess.SessionID, 10, 0)
		if err != nil {
			t.Fatalf("列出任务: %v", err)
		}
		if len(list) != 3 {
			t.Fatalf("期望 3 个任务,实际 %d 个", len(list))
		}
		want := []string{newest.TaskUID, middle.TaskUID, oldest.TaskUID}
		for i, uid := range want {
			if list[i].TaskUID != uid {
				t.Errorf("第 %d 条应为 %s,实际是 %s(排序应按 created_at 倒序)", i, uid, list[i].TaskUID)
			}
		}

		page, err := r.Task.ListBySession(ctx, sess.SessionID, 2, 1)
		if err != nil {
			t.Fatalf("分页列出任务: %v", err)
		}
		if len(page) != 2 || page[0].TaskUID != middle.TaskUID || page[1].TaskUID != oldest.TaskUID {
			t.Errorf("limit=2 offset=1 的结果不对: %+v", page)
		}
	})
}

func TestTaskRepo_CountByUserSince(t *testing.T) {
	withTx(t, func(t *testing.T, r *Repository) {
		ctx := context.Background()
		u := mustUser(t, r)
		sess := mustSession(t, r, u.ID)

		now := time.Now()
		mustTask(t, r, sess, u.ID, now.Add(-2*time.Hour))
		mustTask(t, r, sess, u.ID, now.Add(-30*time.Minute))
		mustTask(t, r, sess, u.ID, now)

		// 另一个用户的任务不该算进来
		other := mustUser(t, r)
		mustTask(t, r, mustSession(t, r, other.ID), other.ID, now)

		n, err := r.Task.CountByUserSince(ctx, u.ID, now.Add(-time.Hour))
		if err != nil {
			t.Fatalf("统计任务数: %v", err)
		}
		if n != 2 {
			t.Errorf("最近一小时应有 2 个任务,实际 %d 个", n)
		}

		n, err = r.Task.CountByUserSince(ctx, u.ID, now.Add(time.Minute))
		if err != nil {
			t.Fatalf("统计任务数: %v", err)
		}
		if n != 0 {
			t.Errorf("未来时间点应统计到 0 个,实际 %d 个", n)
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
			if _, err := tr.User.Create(ctx, email, "hash", nil); err != nil {
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
			_, err := tr.User.Create(ctx, email, "hash", nil)
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

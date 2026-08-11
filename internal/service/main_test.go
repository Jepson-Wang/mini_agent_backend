// main_test.go 只放进程级的东西:测试库连接和 TestMain。
//
// 单独一个文件是因为 TestMain 整个测试二进制只能有一个 —— 哪怕内部包
// (package service)和外部包(package service_test)各写一个,也会报
// multiple definitions of TestMain。放在一个有名字的固定位置,
// 拆测试文件时就不会有人顺手又写一个。
package service

import (
	"log/slog"
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/hash"
)

// testDB 是本包共用的连接,没配 TEST_DSN 时为 nil,用例会被跳过。
//
// 别直接拿它建服务 —— 走 withTx,每个用例一个事务、结束回滚。
// 直接用这个连接的话用例之间会互相看见对方的数据,在 t.Parallel() 下偶发失败。
//
// 跑法(PowerShell):
//
//	$env:TEST_DSN="host=localhost user=postgres password=... dbname=mini_agent_test port=5432 sslmode=disable"
//	go test ./internal/service/
//
// TEST_DSN 必须指向单独的测试库,不能指向开发库 —— 下面会跑 AutoMigrate 改表结构。
var testDB *gorm.DB

func TestMain(m *testing.M) {
	// bcrypt 的 cost 12 每次要 160ms,这个包里要散列几十次。
	// 调到最低只影响耗时,不影响任何被测行为。
	//
	// 注意这是**进程级**副作用,所有用例共享。想临时改 cost 的用例
	// (见 TestLogin_RehashesWhenCostIncreases)必须自己 t.Cleanup 恢复。
	if err := hash.SetCost(bcrypt.MinCost); err != nil {
		fatal("SetCost 失败", err)
	}

	if dsn := os.Getenv("TEST_DSN"); dsn != "" {
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
			Logger: logger.Default.LogMode(logger.Silent),
			// ErrEmailOccupied 的断言全靠它。
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
	os.Exit(m.Run())
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
// 本包和 repository 包的 TestMain 各跑一次迁移,而 go test ./... 是并行跑包的。
// 平时两边都是空操作看不出问题,一旦有新列(比如 tasks.cancel_reason),
// 两条 ALTER TABLE ... ADD COLUMN 撞在一起就会有一个拿到
// 42701(column already exists),整个包直接 log.Fatal ——
// 表现是"第一次跑挂,第二次就好了",很容易被当成偶发放过去。
//
// 用 pg_advisory_xact_lock 而不是 pg_advisory_lock:后者是会话级的,
// 连接被还回池里之后锁还在,得自己配对解锁;前者在事务结束时自动释放。
// 锁常量必须和 repository 那边的一致。
func migrateTestDB(db *gorm.DB) error {
	const migrateLockKey = 0x6D696E69 // "mini"

	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", migrateLockKey).Error; err != nil {
			return err
		}
		return tx.AutoMigrate(&model.User{}, &model.Session{}, &model.Task{})
	})
}

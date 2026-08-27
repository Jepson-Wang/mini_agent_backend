// main_test.go 只放进程级的东西:测试库连接和 TestMain。
//
// 单独一个文件是因为 TestMain 整个测试二进制只能有一个 —— 哪怕内部包
// (package service)和外部包(package service_test)各写一个,也会报
// multiple definitions of TestMain。放在一个有名字的固定位置,
// 拆测试文件时就不会有人顺手又写一个。
package service

import (
	"errors"
	"fmt"
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
	requireDBOrExplicitSkip("service")
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

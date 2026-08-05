// main_test.go 只放进程级的东西:测试库连接和 TestMain。
//
// 单独一个文件是因为 TestMain 整个测试二进制只能有一个 —— 哪怕内部包
// (package service)和外部包(package service_test)各写一个,也会报
// multiple definitions of TestMain。放在一个有名字的固定位置,
// 拆测试文件时就不会有人顺手又写一个。
package service

import (
	"log"
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
		log.Fatalf("SetCost: %v", err)
	}

	if dsn := os.Getenv("TEST_DSN"); dsn != "" {
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
			Logger: logger.Default.LogMode(logger.Silent),
			// ErrEmailOccupied 的断言全靠它。
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

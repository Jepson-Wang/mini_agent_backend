package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/Jepson-Wang/mini_agent_backend/internal/model"
	"github.com/Jepson-Wang/mini_agent_backend/internal/pkg/hash"
	"github.com/Jepson-Wang/mini_agent_backend/internal/repository"
)

// testDB 是本包共用的连接,没配 TEST_DSN 时为 nil,用例会被跳过。
// 跑法和 internal/repository 的用例一样(PowerShell):
//
//	$env:TEST_DSN="host=localhost user=postgres password=... dbname=mini_agent_test port=5432 sslmode=disable"
//	go test ./internal/service/
var testDB *gorm.DB

func TestMain(m *testing.M) {
	// bcrypt 的 cost 12 每次要 160ms,这个包里要散列几十次。
	// 调到最低只影响耗时,不影响任何被测行为。
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

// fakeIssuer 记下拿到的 userID,顺便省掉真造一个 32 字节密钥。
type fakeIssuer struct {
	lastUID int64
	err     error
}

func (f *fakeIssuer) Issue(userID int64) (string, time.Time, error) {
	f.lastUID = userID
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	return "tok-" + strconv.FormatInt(userID, 10), time.Now().Add(time.Hour), nil
}

// withSvc 把用例跑在独立事务里,结束一律回滚,用例之间互不干扰。
func withSvc(t *testing.T, fn func(t *testing.T, s *AuthService, r *repository.Repository, iss *fakeIssuer)) {
	t.Helper()
	if testDB == nil {
		t.Skip("未设置 TEST_DSN,跳过需要数据库的用例")
	}
	tx := testDB.Begin()
	if tx.Error != nil {
		t.Fatalf("开启事务失败: %v", tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })

	repo := repository.New(tx)
	iss := &fakeIssuer{}
	fn(t, NewAuthService(repo, iss), repo, iss)
}

var emailSeq atomic.Int64

func uniqueEmail() string {
	return fmt.Sprintf("svc%d@test.local", emailSeq.Add(1))
}

const testPassword = "correct horse battery"

func TestRegister_OK(t *testing.T) {
	withSvc(t, func(t *testing.T, s *AuthService, r *repository.Repository, iss *fakeIssuer) {
		ctx := context.Background()
		email := uniqueEmail()

		cred, err := s.Register(ctx, email, testPassword, "爱丽丝")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}

		if cred.User.ID == 0 {
			t.Error("用户 id 为 0")
		}
		if cred.Token != "tok-"+strconv.FormatInt(cred.User.ID, 10) {
			t.Errorf("Token = %q", cred.Token)
		}
		if iss.lastUID != cred.User.ID {
			t.Errorf("签发用的 uid = %d, 期望 %d", iss.lastUID, cred.User.ID)
		}
		if cred.User.Nickname == nil || *cred.User.Nickname != "爱丽丝" {
			t.Errorf("Nickname = %v", cred.User.Nickname)
		}

		// 库里存的必须是散列,不是明文。
		saved, err := r.User.GetByEmail(ctx, email)
		if err != nil {
			t.Fatalf("GetByEmail: %v", err)
		}
		if saved.PasswordHash == testPassword {
			t.Fatal("库里存的是明文口令")
		}
		if !strings.HasPrefix(saved.PasswordHash, "$2") {
			t.Errorf("PasswordHash 不像 bcrypt: %q", saved.PasswordHash)
		}
		if err := hash.Verify(saved.PasswordHash, testPassword); err != nil {
			t.Errorf("存下来的散列对不上原口令: %v", err)
		}
	})
}

// 昵称没填要存成 NULL,不是空串 —— 否则库里会有两种"没有昵称"。
func TestRegister_BlankNicknameIsNull(t *testing.T) {
	withSvc(t, func(t *testing.T, s *AuthService, r *repository.Repository, _ *fakeIssuer) {
		cred, err := s.Register(context.Background(), uniqueEmail(), testPassword, "   ")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		if cred.User.Nickname != nil {
			t.Errorf("Nickname = %q, 期望 nil", *cred.User.Nickname)
		}
	})
}

// 邮箱大小写不同不能算两个账号。唯一索引比的是字节,不管大小写,
// 所以规范化必须在业务层做,而且注册和登录得用同一套规则。
func TestRegister_EmailIsCaseInsensitive(t *testing.T) {
	withSvc(t, func(t *testing.T, s *AuthService, _ *repository.Repository, _ *fakeIssuer) {
		ctx := context.Background()
		lower := uniqueEmail()
		upper := strings.ToUpper(lower)

		if _, err := s.Register(ctx, upper, testPassword, ""); err != nil {
			t.Fatalf("Register: %v", err)
		}

		// 大写注册,小写也要能登进来。
		if _, err := s.Login(ctx, lower, testPassword); err != nil {
			t.Fatalf("小写登录失败: %v —— 注册和登录的规范化规则不一致", err)
		}

		// 同一个邮箱换个写法,应当被判定为已占用。
		// 这条放最后:唯一约束一旦触发,外层事务就进入 aborted 状态
		// (SQLSTATE 25P02),后面所有语句都会被拒。
		if _, err := s.Register(ctx, lower, testPassword, ""); !errors.Is(err, model.ErrEmailOccupied) {
			t.Fatalf("err = %v, 期望 ErrEmailOccupied —— 大小写不同注册出了两个账号", err)
		}
	})
}

func TestLogin_OK(t *testing.T) {
	withSvc(t, func(t *testing.T, s *AuthService, _ *repository.Repository, iss *fakeIssuer) {
		ctx := context.Background()
		email := uniqueEmail()

		reg, err := s.Register(ctx, email, testPassword, "")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}

		cred, err := s.Login(ctx, email, testPassword)
		if err != nil {
			t.Fatalf("Login: %v", err)
		}
		if cred.User.ID != reg.User.ID {
			t.Errorf("登录拿到的用户 id = %d, 期望 %d", cred.User.ID, reg.User.ID)
		}
		if iss.lastUID != reg.User.ID {
			t.Errorf("签发用的 uid = %d", iss.lastUID)
		}
		if cred.ExpiresAt.Before(time.Now()) {
			t.Error("签出来就已经过期了")
		}
	})
}

// 这条是登录接口最重要的一条不变式:
// "邮箱不存在"和"口令不对"必须是同一个错误,否则接口就是账号枚举器。
func TestLogin_FailuresAreIndistinguishable(t *testing.T) {
	withSvc(t, func(t *testing.T, s *AuthService, _ *repository.Repository, _ *fakeIssuer) {
		ctx := context.Background()
		email := uniqueEmail()
		if _, err := s.Register(ctx, email, testPassword, ""); err != nil {
			t.Fatalf("Register: %v", err)
		}

		_, wrongPwd := s.Login(ctx, email, "definitely-not-it")
		_, noSuchUser := s.Login(ctx, uniqueEmail(), testPassword)

		for name, err := range map[string]error{"口令不对": wrongPwd, "邮箱不存在": noSuchUser} {
			if !errors.Is(err, model.ErrInvalidCredentials) {
				t.Errorf("%s 返回的是 %v, 期望 ErrInvalidCredentials", name, err)
			}
		}
		// 错误原文也会进日志,别把邮箱和口令带进去。
		for name, err := range map[string]error{"口令不对": wrongPwd, "邮箱不存在": noSuchUser} {
			if err != nil && strings.Contains(err.Error(), testPassword) {
				t.Errorf("%s 的错误里出现了口令: %v", name, err)
			}
		}
	})
}

// 禁用的账号不能登录。注意 Login 是先验口令再看状态 ——
// 反过来的话,不知道口令也能问出某个邮箱是否注册过。
func TestLogin_DisabledUser(t *testing.T) {
	withSvc(t, func(t *testing.T, s *AuthService, r *repository.Repository, _ *fakeIssuer) {
		ctx := context.Background()
		email := uniqueEmail()

		reg, err := s.Register(ctx, email, testPassword, "")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		if err := r.DB().Model(&model.User{}).Where("id = ?", reg.User.ID).
			Update("status", "banned").Error; err != nil {
			t.Fatalf("改状态失败: %v", err)
		}

		// 口令正确 → 才轮得到状态检查。
		if _, err := s.Login(ctx, email, testPassword); !errors.Is(err, model.ErrUserDisabled) {
			t.Fatalf("err = %v, 期望 ErrUserDisabled", err)
		}

		// 口令不对 → 仍然只说"凭据无效",不能透露这个账号存在且被禁用。
		if _, err := s.Login(ctx, email, "wrong-password"); !errors.Is(err, model.ErrInvalidCredentials) {
			t.Fatalf("err = %v, 期望 ErrInvalidCredentials —— 状态检查跑到口令校验前面了", err)
		}
	})
}

func TestLogin_RecordsLastLoginAt(t *testing.T) {
	withSvc(t, func(t *testing.T, s *AuthService, r *repository.Repository, _ *fakeIssuer) {
		ctx := context.Background()
		email := uniqueEmail()

		reg, err := s.Register(ctx, email, testPassword, "")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		if reg.User.LastLoginAt != nil {
			t.Error("刚注册的用户不该有最后登录时间")
		}

		if _, err := s.Login(ctx, email, testPassword); err != nil {
			t.Fatalf("Login: %v", err)
		}

		saved, err := r.User.GetByID(ctx, reg.User.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if saved.LastLoginAt == nil {
			t.Fatal("last_login_at 没写进去")
		}
		if time.Since(*saved.LastLoginAt) > time.Minute {
			t.Errorf("last_login_at = %v, 不像刚刚", saved.LastLoginAt)
		}
	})
}

// cost 调高之后,老账号的散列要在下次登录时顺手升级 ——
// 散列不可逆,用户手上有明文的这一刻是唯一的迁移时机。
func TestLogin_RehashesWhenCostIncreases(t *testing.T) {
	withSvc(t, func(t *testing.T, s *AuthService, r *repository.Repository, _ *fakeIssuer) {
		ctx := context.Background()
		email := uniqueEmail()

		reg, err := s.Register(ctx, email, testPassword, "")
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		oldHash := reg.User.PasswordHash

		// 提高 cost 并在用例结束后恢复,别影响同包的其他用例。
		newCost := hash.Cost() + 1
		if err := hash.SetCost(newCost); err != nil {
			t.Fatalf("SetCost: %v", err)
		}
		t.Cleanup(func() { _ = hash.SetCost(bcrypt.MinCost) })

		if _, err := s.Login(ctx, email, testPassword); err != nil {
			t.Fatalf("Login: %v", err)
		}

		saved, err := r.User.GetByID(ctx, reg.User.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if saved.PasswordHash == oldHash {
			t.Fatal("散列没有被重算")
		}
		if got, err := bcrypt.Cost([]byte(saved.PasswordHash)); err != nil || got != newCost {
			t.Fatalf("新散列的 cost = %d (err=%v), 期望 %d", got, err, newCost)
		}
		// 最要紧的:重算之后原口令还得能登进来。
		if _, err := s.Login(ctx, email, testPassword); err != nil {
			t.Fatalf("重算后登录失败: %v", err)
		}
	})
}

// 签发失败不能悄悄返回一个没有令牌的登录态。
func TestIssueFailurePropagates(t *testing.T) {
	withSvc(t, func(t *testing.T, s *AuthService, _ *repository.Repository, iss *fakeIssuer) {
		iss.err = errors.New("签发器坏了")

		if _, err := s.Register(context.Background(), uniqueEmail(), testPassword, ""); err == nil {
			t.Fatal("签发失败时 Register 应当报错")
		}
	})
}

// 没注入签发器是接线错误,要报错而不是 panic。
func TestIssue_NilIssuer(t *testing.T) {
	s := NewAuthService(nil, nil)
	if _, err := s.issue(&model.User{ID: 1}); err == nil {
		t.Fatal("未注入 TokenIssuer 时应当报错")
	}
}

func TestNormalizeEmail(t *testing.T) {
	for in, want := range map[string]string{
		"  Alice@Example.COM  ": "alice@example.com",
		"bob@test.local":        "bob@test.local",
		"":                      "",
	} {
		if got := normalizeEmail(in); got != want {
			t.Errorf("normalizeEmail(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

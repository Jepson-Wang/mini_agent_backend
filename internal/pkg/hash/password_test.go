package hash

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// TestMain 把 cost 压到最低。默认的 12 每次运算 160ms,
// 这个文件跑一轮要十几秒,没必要。
func TestMain(m *testing.M) {
	if err := SetCost(bcrypt.MinCost); err != nil {
		panic(err)
	}
	m.Run()
}

func TestHash_SaltIsRandom(t *testing.T) {
	a, err := Hash("hunter2")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	b, err := Hash("hunter2")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	// 同一个口令两次结果必须不同,否则说明 salt 没起作用,
	// 一张彩虹表就能把整个用户表撞开。
	if a == b {
		t.Fatal("同一口令两次 hash 结果相同,salt 没生效")
	}
}

func TestHash_Length(t *testing.T) {
	h, err := Hash("hunter2")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	// bcrypt 输出恒定 60 字符,users.password_hash 是 varchar(255),够用。
	if len(h) != 60 {
		t.Fatalf("hash 长度 = %d, 期望 60", len(h))
	}
}

func TestVerify_Correct(t *testing.T) {
	h, err := Hash("hunter2")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := Verify(h, "hunter2"); err != nil {
		t.Fatalf("正确口令校验失败: %v", err)
	}
}

func TestVerify_Wrong(t *testing.T) {
	h, err := Hash("hunter2")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := Verify(h, "hunter3"); !errors.Is(err, ErrMismatchedPassword) {
		t.Fatalf("错误口令返回 %v, 期望 ErrMismatchedPassword", err)
	}
}

// 脏 hash 必须报 ErrInvalidHash 而不是"密码错误"。
// 混淆的话,字段被截断的用户会永远登不上,而日志里只有一堆正常的 401。
func TestVerify_BrokenHash(t *testing.T) {
	cases := map[string]string{
		"空串":       "",
		"明文":       "hunter2",
		"截断的 hash": "$2a$04$abcdef",
		"前缀不对":     "$9x$04$" + strings.Repeat("a", 53),
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			err := Verify(bad, "hunter2")
			if !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("err = %v, 期望 ErrInvalidHash", err)
			}
			if errors.Is(err, ErrMismatchedPassword) {
				t.Fatal("脏 hash 被误判成了口令不匹配")
			}
		})
	}
}

func TestHash_LengthLimit(t *testing.T) {
	// 边界:正好 MaxPasswordBytes 要过。
	if _, err := Hash(strings.Repeat("a", MaxPasswordBytes)); err != nil {
		t.Fatalf("%d 字节应当通过, 得到 %v", MaxPasswordBytes, err)
	}
	// 多一个字节要被拦下,而且必须是 ErrPasswordTooLong,
	// 不能是 bcrypt 那个笼统的生成失败。
	if _, err := Hash(strings.Repeat("a", MaxPasswordBytes+1)); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("%d 字节返回 %v, 期望 ErrPasswordTooLong", MaxPasswordBytes+1, err)
	}
	// 按字节不按字符:汉字 3 字节,超限的汉字串同样要被拦。
	if _, err := Hash(strings.Repeat("密", MaxPasswordBytes/3+1)); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("超长汉字口令返回 %v, 期望 ErrPasswordTooLong", err)
	}
}

// 超长口令在校验侧判不匹配,不能因为 bcrypt 内部截断到 72 字节而误判成正确。
func TestVerify_LengthLimit(t *testing.T) {
	h, err := Hash(strings.Repeat("a", MaxPasswordBytes))
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := Verify(h, strings.Repeat("a", MaxPasswordBytes+8)); !errors.Is(err, ErrMismatchedPassword) {
		t.Fatalf("超长口令返回 %v, 期望 ErrMismatchedPassword", err)
	}
}

func TestNeedsRehash(t *testing.T) {
	// 当前 cost 生成的,不用重算。
	h, err := Hash("hunter2")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if NeedsRehash(h) {
		t.Fatal("当前 cost 的 hash 不该需要重算")
	}

	// 模拟一条历史数据:用更低的 cost 生成,提 cost 后应当被识别出来。
	old, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	defer func(prev int) { cost = prev }(cost)
	cost = bcrypt.MinCost + 2

	if !NeedsRehash(string(old)) {
		t.Fatal("低 cost 的 hash 应当需要重算")
	}
	// 脏数据不该触发重算。
	if NeedsRehash("not a bcrypt hash") {
		t.Fatal("非法 hash 不该触发重算")
	}
}

func TestSetCost(t *testing.T) {
	defer func(prev int) { cost = prev }(cost)

	if err := SetCost(bcrypt.MinCost + 1); err != nil {
		t.Fatalf("合法 cost 不该报错: %v", err)
	}
	if Cost() != bcrypt.MinCost+1 {
		t.Fatalf("Cost() = %d, 期望 %d", Cost(), bcrypt.MinCost+1)
	}

	// 配置漏配读到 0 时,必须回退到 DefaultCost 并报错,
	// 绝不能把强度悄悄降下去。
	if err := SetCost(0); err == nil {
		t.Fatal("越界 cost 应当返回 error")
	}
	if Cost() != DefaultCost {
		t.Fatalf("越界后 Cost() = %d, 期望回退到 %d", Cost(), DefaultCost)
	}

	if err := SetCost(bcrypt.MaxCost + 1); err == nil {
		t.Fatal("超过 MaxCost 应当返回 error")
	}
}

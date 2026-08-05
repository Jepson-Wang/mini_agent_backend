// Package hash 负责口令的单向散列与校验。
//
// 这一层只做密码学,不认业务:它抛出的是 ErrMismatchedPassword 这种
// "口令对不上"的事实,翻译成哪个业务错误由 service 层决定。
// 这样 hash 包不依赖 model,单测里也不用拖起整个领域层。
package hash

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

const (
	// DefaultCost 是 bcrypt 的计算强度。每 +1 耗时翻倍。
	DefaultCost = 12

	// MaxPasswordBytes 允许的口令最大字节数。
	MaxPasswordBytes = 72
)

var (
	// ErrPasswordTooLong 口令超过 MaxPasswordBytes。
	// 这是参数问题,不是内部故障,可以原样告诉用户。
	ErrPasswordTooLong = fmt.Errorf("password exceeds %d bytes", MaxPasswordBytes)

	// ErrMismatchedPassword 口令不匹配。
	// 上层必须翻译成和"用户不存在"完全一样的对外响应,否则接口就成了账号探测器。
	ErrMismatchedPassword = errors.New("password mismatch")

	// ErrInvalidHash 库里存的不是合法的 bcrypt 串 —— 字段被截断、迁移写错、
	// 或者有人往 password_hash 里塞了明文。
	//
	// 这不是"密码错了",绝对不能翻译成登录失败:那样用户永远登不上,
	// 而你在日志里只看到一堆正常的 401。要当内部故障报警。
	ErrInvalidHash = errors.New("invalid password hash")
)

// cost 只在进程启动时通过 SetCost 改一次,之后全程只读,所以不加锁。
var cost = DefaultCost

// SetCost 覆盖计算强度,只应在启动阶段调用一次,且要在任何 Hash/Verify 之前。
//
// 越界的值会回退到 DefaultCost 并返回 error —— 宁可慢一点,
// 也不能因为配置写错(比如漏配读到 0)把强度悄悄降没了。
// 测试里应该设成 bcrypt.MinCost(4),否则每个用例要花 160ms。
func SetCost(c int) error {
	if c < bcrypt.MinCost || c > bcrypt.MaxCost {
		cost = DefaultCost
		return fmt.Errorf("bcrypt cost %d 越界(合法范围 %d-%d),已回退到 %d",
			c, bcrypt.MinCost, bcrypt.MaxCost, DefaultCost)
	}
	cost = c
	return nil
}

// Cost 返回当前生效的强度,给启动日志用。
func Cost() int { return cost }

// Hash 计算口令散列。salt 由 bcrypt 自己生成并嵌在结果里,不用额外存。
// 返回值恒为 60 个字符。
func Hash(password string) (string, error) {
	if len(password) > MaxPasswordBytes {
		return "", ErrPasswordTooLong
	}

	h, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		// 长度上面拦过了,走到这儿只可能是 cost 非法,属于配置事故。
		// 包一层保留原因 —— 但注意错误里绝不能出现 password 本身。
		return "", fmt.Errorf("bcrypt 生成失败: %w", err)
	}
	return string(h), nil
}

// Verify 校验口令。匹配返回 nil,不匹配返回 ErrMismatchedPassword,
// hash 本身有问题返回 ErrInvalidHash。
//
// 比较走 bcrypt.CompareHashAndPassword,内部是 subtle.ConstantTimeCompare;
// 千万不要自己把两个 hash 拿去 == 或 bytes.Equal。
func Verify(hash, password string) error {
	// CompareHashAndPassword 内部不做长度检查,超长会被 blowfish 静默截断到
	// 72 字节再比较。既然 Hash 那边卡了 MaxPasswordBytes,超长的口令不可能
	// 存在于库里,直接判定不匹配。
	if len(password) > MaxPasswordBytes {
		return ErrMismatchedPassword
	}

	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
		return ErrMismatchedPassword
	default:
		// ErrHashTooShort / InvalidHashPrefixError / HashVersionTooNewError /
		// InvalidCostError 都归到这儿。
		return fmt.Errorf("%w: %v", ErrInvalidHash, err)
	}
}

// NeedsRehash 判断一个已存的 hash 是不是用比当前更低的 cost 算的。
//
// 用法:登录成功后如果返回 true,就用刚拿到的明文重新 Hash 并更新库里的字段。
// 这是唯一能拿到明文的时机,错过就只能等用户下次登录。
//
// hash 非法时返回 false —— 这种情况 Verify 已经报 ErrInvalidHash 了,
// 轮不到这儿处理,更不该拿一个坏 hash 去触发重算。
func NeedsRehash(hash string) bool {
	oldCost, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		return false
	}
	return oldCost < cost
}

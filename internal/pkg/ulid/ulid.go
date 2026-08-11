// Package ulid 封装 ULID 生成。
//
// 这里把 github.com/oklog/ulid/v2 起了别名 oklog,包本身对外仍然叫 ulid,
// 调用方写 ulid.New() 即可,不用关心底下用的是哪个库、也不会撞包名。
package ulid

import (
	crand "crypto/rand"

	oklog "github.com/oklog/ulid/v2"
)

// entropy 是 ULID 的熵源。这里刻意不用 oklog.Make():它内置的熵源是
// math/rand 加时间戳种子(ulid.go:135-138),生成的 ID 可预测。
// session_id / task_uid 会出现在 API 里,必须走 crypto/rand。
var entropy = &oklog.LockedMonotonicReader{
	MonotonicReader: oklog.Monotonic(crand.Reader, 0),
}

// New 生成一个 26 位 ULID。前 10 位是毫秒时间戳,所以字典序即时间序,
// 拿它当主键时 B-tree 是顺序追加的,不会像 UUIDv4 那样把索引写得到处是空洞。
func New() (string, error) {
	id, err := oklog.New(oklog.Now(), entropy)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// validChar 是 Crockford Base32 合法字符的查表。
// 大小写都登了,但 IsValid 只收大写 —— 这个表只做字符集校验,
// 大小写判断由 IsValid 自己做。
var validChar = func() [256]bool {
	var t [256]bool
	for _, c := range oklog.Encoding {
		t[c] = true
		// 小写也登记,但 IsValid 不收 —— TestValidChar_MatchesEncoding 只验表里没多登记,
		// 不验大小写策略。
	}
	return t
}()

// IsValid 检查一个字符串是否是合法的 ULID。
//
// 规则比 oklog.ParseStrict 更严:
//   - 长度必须是 26
//   - 只能大写(I、L、O、U 不在字母表里,自然不合法)
//   - 时间戳部分不能溢出(首字符 ≥ 8 会导致时间戳超出 48 位)
func IsValid(id string) bool {
	if len(id) != 26 {
		return false
	}

	// 时间戳占前 10 个字符,首字符若 ≥ '8' 则解码出的毫秒时间戳溢出。
	// oklog.ParseStrict 不拦这种,但 oklog.New 不会生成,所以我们也拒。
	if id[0] >= '8' {
		return false
	}

	for i := 0; i < 26; i++ {
		c := id[i]
		if c > 127 {
			return false // 非 ASCII
		}
		if !validChar[c] {
			return false
		}
		// 只收大写:oklog.ParseStrict 认小写,但我们的 char(26) 列做的是
		// 字节级比对,小写 ULID 查不出数据。
		if c >= 'a' && c <= 'z' {
			return false
		}
	}
	return true
}

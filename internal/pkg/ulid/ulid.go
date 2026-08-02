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

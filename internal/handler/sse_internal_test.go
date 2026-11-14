package handler

import "testing"

// 续传游标的解析。
//
// 单独一个文件是因为 parseLastEventID 是**未导出**的函数,
// 而 handler 包的其它测试都在包外(package handler_test),够不着它。
// 同 middleware/trace_test.go 的做法。
//
// ## 为什么值得单独测
//
// parseLastEventID 的契约是「解析失败或者为负一律当 0」。
// 而下游的 resolveLastID 里还有一道 `lastID <= 0` 的兜底 ——
// 所以在 handler 这一层,负数和不带 header 的表现**完全一样**,
// 从外面测不出区别。
//
// 也就是说:哪天有人把这里的 `|| n < 0` 删掉,handler 层的用例一条都不会红。
// 而这个函数的契约会悄悄变成「负数原样返回」——
// 现在没人用它,但下一个用它的人会踩到。
//
// 这条用例锁的就是那个契约本身。
func TestParseLastEventID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int64
	}{
		{"空字符串", "", 0},
		{"只有空白", "   ", 0},
		{"不是数字", "abc", 0},
		{"浮点数", "1.5", 0},
		{"负数", "-5", 0},
		{"负零", "-0", 0},
		{"零", "0", 0},
		{"正常值", "42", 42},
		{"带空白", "  42  ", 42},
		{"超大数溢出", "99999999999999999999999", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseLastEventID(c.in); got != c.want {
				t.Errorf("parseLastEventID(%q) = %d, 期望 %d", c.in, got, c.want)
			}
		})
	}
}

// Last-Event-ID 是一个游标,不是凭据。
//
// 它只决定「读本会话的哪一段」,读不出别的会话 ——
// 越权那道防线在 MustOwnSession,不在这里。
//
// 所以格式不对的游标不该拒绝整个请求:为一个格式不对的游标拒绝,
// 只会让客户端连「重头开始」都做不到。
func TestParseLastEventID_NeverRejects(t *testing.T) {
	for _, in := range []string{"", "  ", "abc", "-1", "-99999", "1.5", "0x10"} {
		if got := parseLastEventID(in); got < 0 {
			t.Errorf("parseLastEventID(%q) = %d, 期望 >= 0(负游标一律当 0)", in, got)
		}
	}
}

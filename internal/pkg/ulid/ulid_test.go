package ulid

import (
	"strings"
	"testing"

	oklog "github.com/oklog/ulid/v2"
)

// TestIsValid_AcceptsGenerated 是最重要的一条:自家生成的 ID 必须过自家的校验。
// 这两个函数分开写,哪天有人改了其中一个,这里会先炸。
func TestIsValid_AcceptsGenerated(t *testing.T) {
	for i := 0; i < 200; i++ {
		id, err := New()
		if err != nil {
			t.Fatalf("生成 ULID: %v", err)
		}
		if !IsValid(id) {
			t.Fatalf("New() 生成的 %q 没通过 IsValid", id)
		}
	}
}

func TestIsValid(t *testing.T) {
	// 一个真实的 26 位 ULID,下面的用例都在它身上改。
	const ok = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

	cases := []struct {
		name string
		id   string
		want bool
	}{
		{"合法", ok, true},
		{"全 0", "00000000000000000000000000", true},
		{"最大值", "7ZZZZZZZZZZZZZZZZZZZZZZZZZ", true},

		// 长度
		{"空串", "", false},
		{"少一位", ok[:25], false},
		{"多一位", ok + "A", false},
		{"前后带空格", " " + ok[:24] + " ", false},
		{"末尾换行", ok[:25] + "\n", false},

		// 字符集:Crockford 挖掉的四个字母
		{"含 I", "I" + ok[1:], false},
		{"含 L", "0L" + ok[2:], false},
		{"含 O", "0O" + ok[2:], false},
		{"含 U", "0U" + ok[2:], false},
		{"含连字符", "0-" + ok[2:], false},
		{"含空格", "0 " + ok[2:], false},

		// 大小写:oklog.ParseStrict 收小写,我们不收。
		{"全小写", strings.ToLower(ok), false},
		{"混一个小写", ok[:25] + "v", false},

		// 首字符溢出:字符集和长度都对,但解不出 ULID。
		{"首字符 8", "8" + ok[1:], false},
		{"首字符 Z", "Z" + ok[1:], false},

		// 非 ASCII:三字节汉字,长度和查表两道都拦得住。
		{"含汉字", "中" + ok[3:], false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsValid(c.id); got != c.want {
				t.Errorf("IsValid(%q) = %v, 期望 %v", c.id, got, c.want)
			}
		})
	}
}

// TestIsValid_StricterThanOklog 把我们和库的两处差异钉死,免得哪天有人
// 图省事把 IsValid 换成 oklog.ParseStrict 的包装 —— 那两种输入会被放行,
// 然后在查库时变成一条查不到任何行的 SQL。
func TestIsValid_StricterThanOklog(t *testing.T) {
	const ok = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

	for _, id := range []string{
		strings.ToLower(ok), // 小写:ParseStrict 认,数据库比不上
		ok[:25] + "v",       // 混一个小写
	} {
		if _, err := oklog.ParseStrict(id); err != nil {
			t.Fatalf("前提变了:oklog.ParseStrict(%q) 现在会报错 %v", id, err)
		}
		if IsValid(id) {
			t.Errorf("IsValid(%q) 放行了小写,应当拒绝", id)
		}
	}
}

// TestValidChar_MatchesEncoding 保证查表和字母表没有漂移。
func TestValidChar_MatchesEncoding(t *testing.T) {
	if len(oklog.Encoding) != 32 {
		t.Fatalf("Crockford Base32 应当是 32 个字符,现在是 %d", len(oklog.Encoding))
	}
	var n int
	for c := 0; c < 256; c++ {
		if validChar[c] {
			n++
			if !strings.ContainsRune(oklog.Encoding, rune(c)) {
				t.Errorf("表里有 %q,但字母表没有", rune(c))
			}
		}
	}
	if n != len(oklog.Encoding) {
		t.Errorf("表里登记了 %d 个字符,字母表有 %d 个", n, len(oklog.Encoding))
	}
	for _, c := range "ILOUilou" {
		if validChar[c] {
			t.Errorf("%q 不该是合法字符", c)
		}
	}
}

func BenchmarkIsValid(b *testing.B) {
	const id = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !IsValid(id) {
			b.Fatal("应当合法")
		}
	}
}

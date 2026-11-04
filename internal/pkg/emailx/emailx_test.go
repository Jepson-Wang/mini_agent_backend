package emailx

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"混合大小写", "Alice@Example.COM", "alice@example.com"},
		{"首尾空格", "  alice@example.com  ", "alice@example.com"},
		{"首尾制表符和换行", "\t\nalice@example.com\r\n", "alice@example.com"},
		{"已经规范", "alice@example.com", "alice@example.com"},
		{"空串", "", ""},
		{"纯空白", " \t\n ", ""},
		{"空白加大写", "  Alice@Example.com\t", "alice@example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.in); got != tc.want {
				t.Fatalf("Normalize(%q) = %q, 想要 %q", tc.in, got, tc.want)
			}
		})
	}
}

// 本地部分只差大小写的两个写法必须落到同一个结果 —— 这是限流桶键
// 和查库能对上的前提。
func TestNormalize_LocalPartCaseCollapses(t *testing.T) {
	a := Normalize("Alice@example.com")
	b := Normalize(" aLICE@example.com ")
	if a != b {
		t.Fatalf("只差大小写的本地部分规范化后不一致: %q vs %q", a, b)
	}
}

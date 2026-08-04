package validate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gin-gonic/gin/binding"
)

// 用本包内的结构体做样本,不引 handler/dto —— 这个包不该反向依赖上层。
type sample struct {
	Email    string `json:"email"    binding:"required,email,max=255"`
	Password string `json:"password" binding:"required,min=8,bmax=72"`
	Nickname string `json:"nickname" binding:"omitempty,max=4"`
	Age      int    `json:"age"      binding:"omitempty,gte=18,lte=120"`
	Role     string `json:"role"     binding:"omitempty,oneof=admin member"`
	Secret   string `json:"-"        binding:"omitempty,len=6"`
}

// check 跑一次结构体校验并翻译。
func check(t *testing.T, v any) map[string]string {
	t.Helper()
	err := binding.Validator.ValidateStruct(v)
	if err == nil {
		t.Fatal("期望校验失败,实际通过了")
	}
	fields := Translate(err)
	if len(fields) == 0 {
		t.Fatalf("Translate 没翻出任何字段: %v", err)
	}
	return fields
}

// 核心诉求:错误里的键必须是 json 名,不是 Go 字段名。
// 靠的是 init 里注册的 RegisterTagNameFunc。
func TestTranslate_UsesJSONFieldNames(t *testing.T) {
	fields := check(t, &sample{})

	for _, want := range []string{"email", "password"} {
		if _, ok := fields[want]; !ok {
			t.Errorf("缺少字段 %q,实际拿到 %v", want, fields)
		}
	}
	for _, notWant := range []string{"Email", "Password"} {
		if _, ok := fields[notWant]; ok {
			t.Errorf("出现了 Go 字段名 %q —— RegisterTagNameFunc 没生效", notWant)
		}
	}
}

// json:"-" 的字段退回 Go 名,而不是变成 "-" 这种没法用的键。
func TestTranslate_DashTagFallsBackToGoName(t *testing.T) {
	fields := check(t, &sample{Email: "a@b.com", Password: "12345678", Secret: "xx"})

	if _, bad := fields["-"]; bad {
		t.Errorf("json:\"-\" 的字段被当成了字段名 \"-\": %v", fields)
	}
	if _, ok := fields["Secret"]; !ok {
		t.Errorf("期望退回 Go 名 Secret,实际 %v", fields)
	}
}

func TestTranslate_Messages(t *testing.T) {
	cases := []struct {
		name  string
		in    sample
		field string
		want  string
	}{
		{"必填", sample{Password: "12345678"}, "email", "邮箱不能为空"},
		{"邮箱格式", sample{Email: "not-an-email", Password: "12345678"}, "email", "请填写正确的邮箱地址"},
		{"太短", sample{Email: "a@b.com", Password: "1234"}, "password", "密码至少 8 个字符"},
		{"太长", sample{Email: "a@b.com", Password: "12345678", Nickname: "12345"}, "nickname", "昵称最多 4 个字符"},
		{"数字下限", sample{Email: "a@b.com", Password: "12345678", Age: 10}, "age", "age必须大于或等于 18"},
		{"枚举", sample{Email: "a@b.com", Password: "12345678", Role: "root"}, "role", "role只能是 admin、member 之一"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.in
			got := check(t, &in)[c.field]
			if got != c.want {
				t.Errorf("%s 的提示 = %q, 期望 %q", c.field, got, c.want)
			}
		})
	}
}

// bmax 按字节算。内置的 max 按字符算,拦不住 72 个汉字(216 字节),
// 那种输入会被 bcrypt 截断到 72 字节。
func TestByteMax(t *testing.T) {
	ascii72 := strings.Repeat("a", 72)
	if err := binding.Validator.ValidateStruct(&sample{Email: "a@b.com", Password: ascii72}); err != nil {
		t.Errorf("72 个 ASCII 字符正好 72 字节,不该被拒: %v", err)
	}

	cn30 := strings.Repeat("口", 30) // 30 个字符,90 字节
	if len(cn30) <= 72 {
		t.Fatalf("样本构造错了,只有 %d 字节", len(cn30))
	}
	fields := check(t, &sample{Email: "a@b.com", Password: cn30})
	if !strings.Contains(fields["password"], "字节") {
		t.Errorf("期望按字节数报错,实际 %q", fields["password"])
	}
}

// 不是校验错误就返回 nil,让调用方回一句笼统的话。
// 直接吐 json 包的原文会带出 Go 结构体名和字段路径。
func TestTranslate_NonValidationError(t *testing.T) {
	var target sample
	jsonErr := json.Unmarshal([]byte(`{"age":"十八"}`), &target)
	if jsonErr == nil {
		t.Fatal("样本应当触发 json 类型错误")
	}

	for name, err := range map[string]error{
		"普通错误":   errors.New("boom"),
		"json 错": jsonErr,
		"nil":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			if got := Translate(err); got != nil {
				t.Errorf("期望 nil,实际 %v", got)
			}
		})
	}
}

// 提示语是要回给客户端的,里面不能有内部命名,更不能有用户填的值。
func TestTranslate_LeaksNothing(t *testing.T) {
	const password = "hunter2" // 太短,会触发 min

	fields := check(t, &sample{Email: "bad", Password: password, Role: "root"})

	for field, msg := range fields {
		if strings.Contains(msg, password) {
			t.Errorf("字段 %s 的提示回显了用户填的值: %q", field, msg)
		}
		for _, leak := range []string{"sample", "Error:Field validation", "tag", "binding", "validator"} {
			if strings.Contains(msg, leak) {
				t.Errorf("字段 %s 的提示泄露了内部细节 %q: %s", field, leak, msg)
			}
		}
	}
}

// 没登记的 tag 走兜底文案,而不是把 validator 的原文漏出去。
func TestMessage_UnknownTagFallsBack(t *testing.T) {
	type odd struct {
		Token string `json:"token" binding:"required,hexadecimal"`
	}
	fields := check(t, &odd{Token: "zzz"})
	if got := fields["token"]; got != "token格式不正确" {
		t.Errorf("兜底文案 = %q", got)
	}
}

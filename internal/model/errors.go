package model

import "errors"

// 业务错误。仓库层负责把 gorm / 驱动的错误翻译成这里的值,
// 上层只认这些,不需要知道底下用的是哪个数据库。
var (
	ErrEmailOccupied   = errors.New("email or password is not valid")
	ErrUserNotFound    = errors.New("no users found")
	ErrSessionExists   = errors.New("session already exists")
	ErrSessionNotFound = errors.New("session not found")
	ErrTaskExists      = errors.New("task already exists")
	ErrTaskNotFound    = errors.New("task not found")

	// ErrTaskNotCancelable 任务已经到终态,取消不了了。
	//
	// 它和 ErrTaskNotFound 分得开是安全的:能走到这一步说明调用方
	// 已经证明了这个任务是他自己的(见 service.TaskService.mustOwn),
	// 告诉他「已经跑完了」不多泄露任何东西。
	ErrTaskNotCancelable = errors.New("task is not cancelable")

	// ErrTaskStatusInvalid 回调报上来的状态不在我们认识的那几个里。
	ErrTaskStatusInvalid = errors.New("invalid task status")

	// ErrInvalidCredentials 登录失败。
	// "用户不存在"和"密码错误"都必须归到这一个错误上,对外的 code 和文案
	// 也必须完全一致,否则登录接口就成了账号探测器。
	ErrInvalidCredentials = errors.New("invalid credentials")

	// ErrUserDisabled 账号不可登录(封禁、注销等)。
	//
	// 它和 ErrInvalidCredentials 分开是安全的,前提是**先校验口令再看状态**:
	// 能走到这一步说明对方已经拿着正确口令了,告诉他"账号被禁用"不多泄露什么,
	// 反倒省掉一轮客服。顺序反过来就成了枚举接口 —— 不知道口令也能问出
	// 某个邮箱是否注册过。见 service.AuthService.Login。
	ErrUserDisabled = errors.New("user disabled")
)

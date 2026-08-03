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

	// ErrInvalidCredentials 登录失败。
	// "用户不存在"和"密码错误"都必须归到这一个错误上,对外的 code 和文案
	// 也必须完全一致,否则登录接口就成了账号探测器。
	ErrInvalidCredentials = errors.New("invalid credentials")
)

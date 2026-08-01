// Package ptr 提供可空字段取址的辅助函数。
package ptr

// To 取字面量的地址,用来给可空字段传值:ptr.To("张三")。
// Go 不允许直接对字面量取址,没有它每次都要先声明一个临时变量。
func To[T any](v T) *T { return &v }

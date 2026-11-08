// Package console 传输无关的控制台操作层：web HTTP handler 与 desktop wails
// service 共享同一实现——操作校验、文件读写、错误分类只写一遍，两端各做
// 传输适配（web: Kind → HTTP 状态码；desktop: Kind → ApiError）。
// 响应 JSON 形状与 docs/web-api.md 及桌面既有绑定保持一致，前端零改动。
package console

import "net/http"

// Kind 错误类别（与 desktop ApiError 的 kind 值一一对应）
type Kind string

const (
	KindInvalid     Kind = "invalid"
	KindNotFound    Kind = "notfound"
	KindBusy        Kind = "busy"
	KindReadonly    Kind = "readonly"
	KindUnavailable Kind = "unavailable"
	KindError       Kind = "error"
)

// Error 传输无关的操作错误
type Error struct {
	Kind    Kind   `json:"kind"`
	Message string `json:"message"`
}

// Error 透出错误文本
func (e *Error) Error() string { return e.Message }

// HTTPStatus 类别 → HTTP 状态码（web 传输层适配用）
func (k Kind) HTTPStatus() int {
	switch k {
	case KindInvalid:
		return http.StatusBadRequest
	case KindNotFound:
		return http.StatusNotFound
	case KindBusy:
		return http.StatusConflict
	case KindReadonly:
		return http.StatusForbidden
	case KindUnavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// 各类别构造
func Invalid(msg string) *Error     { return &Error{Kind: KindInvalid, Message: msg} }
func NotFound(msg string) *Error    { return &Error{Kind: KindNotFound, Message: msg} }
func Busy(msg string) *Error        { return &Error{Kind: KindBusy, Message: msg} }
func ReadOnly(msg string) *Error    { return &Error{Kind: KindReadonly, Message: msg} }
func Unavailable(msg string) *Error { return &Error{Kind: KindUnavailable, Message: msg} }
func Wrap(err error) *Error         { return &Error{Kind: KindError, Message: err.Error()} }

// Package services 桌面端 typed Service 层：wails 绑定直供前端调用，
// 复用 web.Backend 与 coding/mcp/mesh 的同一套实现，无 HTTP 面。
// 响应结构直接沿用 web 包导出类型，JSON 形状与 docs/web-api.md 一致；
// 错误统一为 ApiError（Kind 供前端分支，替代 HTTP 状态码）。
package services

import (
	"encoding/json"
	"errors"

	"doge/internal/app"
	"doge/internal/coding"
	"doge/internal/coding/uploads"
	"doge/internal/web"
)

type (
	// ApiError 服务层错误
	ApiError struct {
		Kind    string `json:"kind"`    // 错误类别 1:invalid,2:notfound,3:busy,4:readonly,5:unavailable,6:error
		Message string `json:"message"` // 错误描述
	}

	// Services 全部服务的集合，注册进 wails 的 Services
	Services struct {
		Session *SessionService
		Config  *ConfigService
		Mcp     *McpService
		Skills  *SkillsService
		Mesh    *MeshService
		Uploads *UploadsService
	}

	// Deps 各服务的公共依赖
	Deps struct {
		Backend  web.Backend        // 会话后端（单窗口：恒可写，切换委托 AgentSession）
		Session  *coding.AgentSession // 活动会话实例（MCP 工具重发等直操作）
		Mcp      web.McpView        // nil = MCP 未初始化
		Mesh     web.MeshView       // 初始 mesh 视图（运行时以 App.MeshView 为准）
		Uploads  *uploads.Store     // nil = 上传不可用
		Console  *Console           // 事件桥（session_reset 等帧由此发出）
		App      *app.App           // 装配产物（mesh 运行时启停入口）
		Version  string             // 构建版本（mesh profile 上报）
	}
)

// Error 透出错误文本
func (e *ApiError) Error() string { return e.Message }

// MarshalError 序列化 error 返回给前端（注册进 ServiceOptions.MarshalError）
func MarshalError(err error) []byte {
	var apiErr *ApiError
	if !errors.As(err, &apiErr) {
		apiErr = &ApiError{Kind: "error", Message: err.Error()}
	}
	data, _ := json.Marshal(apiErr)
	return data
}

// 各类别错误构造
func errInvalid(msg string) *ApiError   { return &ApiError{Kind: "invalid", Message: msg} }
func errNotFound(msg string) *ApiError  { return &ApiError{Kind: "notfound", Message: msg} }
func errBusy(msg string) *ApiError      { return &ApiError{Kind: "busy", Message: msg} }
func errReadOnly(msg string) *ApiError  { return &ApiError{Kind: "readonly", Message: msg} }
func errUnavailable(msg string) *ApiError {
	return &ApiError{Kind: "unavailable", Message: msg}
}
func errInternal(err error) *ApiError {
	return &ApiError{Kind: "error", Message: err.Error()}
}

// New 装配全部服务
func New(d Deps) *Services {
	return &Services{
		Session: &SessionService{d: d},
		Config:  &ConfigService{d: d},
		Mcp:     &McpService{d: d},
		Skills:  &SkillsService{d: d},
		Mesh:    &MeshService{d: d},
		Uploads: &UploadsService{d: d},
	}
}

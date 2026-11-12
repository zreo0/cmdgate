// Package api 定义客户端和服务端之间传输的数据结构
//
// 本机 unix socket 和远端 HTTPS 使用同一套结构
package api

import (
	"time"

	"cmdgate/internal/action"
	"cmdgate/internal/store"
)

// ActionView 是返回给调用者的操作说明，不含脚本内容和凭证
type ActionView struct {
	Name        string                  `json:"name"`
	Description string                  `json:"description,omitempty"`
	Params      map[string]action.Param `json:"params,omitempty"`
	Risk        action.Risk             `json:"risk"`
}

// NewActionView 从已加载的操作生成说明
func NewActionView(a *action.Action) ActionView {
	return ActionView{Name: a.Name, Description: a.Description, Params: a.Params, Risk: a.Risk}
}

// SubmitRequest 是发起请求的内容
type SubmitRequest struct {
	Action string            `json:"action"`
	Params map[string]string `json:"params,omitempty"`
	// 文件参数的内容，JSON 里是 base64
	Files map[string][]byte `json:"files,omitempty"`
}

// RequestView 是返回给调用者的请求状态
type RequestView struct {
	ID        string            `json:"id"`
	Action    string            `json:"action"`
	Params    map[string]string `json:"params,omitempty"`
	Files     []store.FileInfo  `json:"files,omitempty"`
	Requester string            `json:"requester"`
	Risk      string            `json:"risk"`
	State     string            `json:"state"`
	Approver  string            `json:"approver,omitempty"`
	Reason    string            `json:"reason,omitempty"`
	Preflight string            `json:"preflight,omitempty"`
	ExitCode  *int              `json:"exit_code,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	ExpiresAt *time.Time        `json:"expires_at,omitempty"`
	// 脱敏后的执行输出，只在请求结束后返回
	Output string `json:"output,omitempty"`
}

// Decision 是管理员应急入口提交的决定
type Decision struct {
	Approve bool `json:"approve"`
}

// WhoAmI 是服务端识别出的调用者身份
type WhoAmI struct {
	Name  string `json:"name"`
	Admin bool   `json:"admin"`
	Mode  string `json:"mode"`
}

// Error 是出错时的响应体
type Error struct {
	Error string `json:"error"`
}

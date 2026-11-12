// Package channel 定义审批渠道的约定
//
// 渠道只负责收发：把审批请求渲染成平台消息、接收回调并验证来源
// 随机数校验、审批人授权、过期和状态流转都在核心里，不在渠道里
package channel

import (
	"context"
	"fmt"
	"sort"
	"time"

	"cmdgate/internal/store"
)

// KV 是一个要展示的参数
type KV struct {
	Key   string
	Value string
}

// File 是一个要展示的文件参数
type File struct {
	store.FileInfo
	Content []byte
}

// Request 是发给审批人的内容，渠道必须完整展示，不能悄悄截断参数
type Request struct {
	ID          string
	Nonce       string
	Action      string
	Description string
	Requester   string
	// 操作目录的 hash
	Hash      string
	Params    []KV
	Files     []File
	Preflight string
	ExpiresAt time.Time
	// 接收审批消息的平台用户 ID
	Recipients []string
}

// Decision 是渠道收到的一次批准或拒绝
type Decision struct {
	RequestID string
	Nonce     string
	Approve   bool
	// 平台验证过的用户 ID，核心用它查审批人别名
	ApproverID string
}

// Channel 是一个已配置好的审批渠道
type Channel interface {
	// Send 发送审批请求，返回每个接收人对应的消息句柄
	Send(ctx context.Context, req Request) ([]store.Handle, error)
	// Decisions 持续接收决定，ctx 结束时关闭返回的通道
	Decisions(ctx context.Context) (<-chan Decision, error)
	// Update 在已发出的消息上标记最新状态并移除按钮
	Update(ctx context.Context, h store.Handle, status string) error
	// Ping 检查渠道是否连通，供 doctor 使用
	Ping(ctx context.Context) error
}

// Prompter 是配置向导与管理员交互的方式
type Prompter interface {
	// Say 显示一行信息
	Say(format string, args ...any)
	// Ask 询问一个值，def 是直接回车时的默认值
	Ask(prompt, def string) (string, error)
	// AskSecret 询问一个不回显的值
	AskSecret(prompt string) (string, error)
}

// SetupResult 是配置向导的产出
type SetupResult struct {
	Config store.ChannelConfig
	// 需要写入凭证库的值，键是凭证名
	Secrets map[string]string
}

// Driver 是一种渠道类型的实现入口
type Driver struct {
	// 交互式配置向导，name 是这个渠道实例在配置里的名字
	Setup func(ctx context.Context, name string, p Prompter) (*SetupResult, error)
	// 按配置创建渠道实例，secret 用来读取渠道自己的凭证
	Open func(name string, cfg store.ChannelConfig, secret func(string) (string, error)) (Channel, error)
}

var drivers = map[string]Driver{}

// Register 登记一种渠道类型，由各渠道包在 init 里调用
func Register(typ string, d Driver) {
	drivers[typ] = d
}

// Lookup 按类型名查找渠道实现
func Lookup(typ string) (Driver, error) {
	d, ok := drivers[typ]
	if !ok {
		return Driver{}, fmt.Errorf("unknown channel type %q (available: %v)", typ, Types())
	}
	return d, nil
}

// Types 返回已登记的渠道类型
func Types() []string {
	var out []string
	for t := range drivers {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

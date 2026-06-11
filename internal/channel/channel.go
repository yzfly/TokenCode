// Package channel 实现 IM 通道体系——团队模式的脊柱：成员用自己的 IM 账号
// 远程驱动自己的工作空间。三块拼图：
//
//   - Adapter：一种 IM 平台的收/发抽象（飞书在 internal/channel/feishu）；
//   - Store：成员绑定与配对码的持久化（~/.config/tokencode/team.json）；
//   - Router：入站消息 → 查绑定 → 在该成员的工作空间里跑一个 turn → 回复。
//
// v0 取舍：纯文本消息（卡片流式后置）、只处理单聊、会话历史驻内存
// （进程生命周期）、进度回报一句「收到」+ 最终文本（不逐条转发工具调用）。
package channel

import "context"

// Inbound 是一条入站消息（平台无关）。
type Inbound struct {
	Channel   string // 通道名（"feishu"…）
	AccountID string // 通道侧的应用/账号标识（飞书=app_id）
	UserID    string // 发送者在该通道的稳定标识（飞书=open_id）
	UserName  string // 发送者显示名（平台不给则为空）
	ChatID    string // 会话标识（回复用）
	Text      string // 文本内容
}

// Outbound 是一条出站消息。v0 纯文本/markdown，卡片后置。
type Outbound struct {
	ChatID string
	Text   string
}

// Adapter 是一种 IM 平台的接入实现。
type Adapter interface {
	Name() string
	// Start 建立长连接并阻塞直到 ctx 取消；重连由实现自管。
	// 收到消息须立即向平台 ack、异步投 sink（sink 可能跑一整个 turn）。
	Start(ctx context.Context, sink func(Inbound)) error
	// Send 发一条出站消息。
	Send(ctx context.Context, out Outbound) error
}

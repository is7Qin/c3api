// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"

	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/is7Qin/codex-sdk/redisstore"
	"github.com/redis/go-redis/v9"
)

// Book 是 Codex 会话簿在网关侧的**唯一门面**：整个仓库除本包外没有任何地方能见到
// `*codexsdk.ConversationBook`。
//
// 为什么是具体类型而不是接口：会话簿没有第二个实现，也没有替身需求——既有全部用例
// 都用真 Book（miniredis 或自造 store），没有一个需要注入假 Book。为不存在的替换点
// 引入接口，代价是每请求一次间接调用加一层要长期维护的契约，与「控制面可抽象、
// 数据面必须具体」的判据相悖。真要替身时用 `export_test.go`，SDK 自身就是这个惯例。
//
// 归属：Redis 键布局、会话/轮次的合并与推进、写入队列与预算、熔断 —— 全部由 SDK
// 定义，本类型只做构造与转发，不复制任何一条算法。
type Book struct{ b *codexsdk.ConversationBook }

// NewBook 在 rdb 上构造会话簿。
//
// rdb 只要求 redis.Scripter（生产是 redisx 的连接池，测试是 miniredis 支撑的
// *redis.Client）：连接池生命周期归调用方，本包不 Close、不持有第二份所有权。
// 注意客户端必须开 ContextTimeoutEnabled（redisstore 要求，否则每批预算不生效并在
// 构造时 panic）——那是装配点的责任，见 cmd/server 的 redisx 构造。
func NewBook(rdb redis.Scripter) *Book {
	return &Book{b: codexsdk.NewConversationBook(redisstore.New(rdb))}
}

// BookRequest 是宿主能提供的**全部**原始输入。
//
// 刻意只放原料：三元组 → ConversationKey 的派生（含分隔与转义规则）、payload 的
// 形状判定（首轮/续轮、指令与工具是否变化）、owner 与条目的绑定，全部留在这里。
// 宿主既不该知道会话键长什么样，也不该能自己拼出一个"看起来合法"的请求。
type BookRequest struct {
	CacheDomain    string // 缓存域（配置代际；跨域即跨键空间）
	APIKeyID       int64  // API key 行号
	ConversationID string // 客户端对话标识（只进 ConversationKey 的散列）
	InstallationID string // 账号级安装号
	Owner          [16]byte
}

// request 把宿主输入归一成 SDK 请求。唯一调用点在本包内。
func (r BookRequest) request(payload []byte) codexsdk.ConversationRequest {
	return codexsdk.ConversationRequest{
		Key:            codexsdk.NewConversationKey(r.CacheDomain, r.APIKeyID, r.ConversationID),
		InstallationID: r.InstallationID,
		Owner:          r.Owner,
		Input:          codexsdk.AnalyzeConversationInput(payload),
	}
}

// Identity 会话路径的一次身份（HTTP 面：每次尝试调用一次，跨账号故障转移时按当次
// 选择重算）。payload 必须是**实际发给上游的体**——它决定本条是不是一轮的第一个
// 请求（Turn.First），与 SDK 内部的判定同源。
//
// 返回值是 SDK 的值类型，宿主只准透传给调用面、不得读取或构造字段：dial 参数包的
// 聚合（CallRequest）在下一个提交完成，届时本方法返回不透明句柄。
func (b *Book) Identity(ctx context.Context, req BookRequest, payload []byte) codexsdk.Identity {
	return b.b.Prepare(ctx, req.request(payload))
}

// Option WS 面的逐帧身份（等价 SDK 的 WithConversation）：身份随首帧逐帧合成，
// turn-state 由会话簿按轮次管理。frame 同时是 Turn.First 的判据与连接上的首帧。
func (b *Book) Option(req BookRequest, frame []byte) codexsdk.Option {
	r := req.request(frame)
	return codexsdk.WithConversation(b.b, r, frame)
}

// Flush 同步排空写入队列一次（批大小与预算归 SDK；同一时刻只允许一个 Flush）。
func (b *Book) Flush(ctx context.Context) error { return b.b.Flush(ctx) }

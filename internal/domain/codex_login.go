// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import "time"

// —— codex 设备码登录（ChatGPT OAuth device authorization grant）——
//
// 协议由 codex-sdk（DeviceAuthClient）实现；本文件的类型是**跨层契约**：
// sdkbridge 包装 SDK 后只暴露这里的类型，service/handler 不直引 codexsdk
// （分层纪律同 sdkbridge/codex.go）。**网关不保存待授权会话**：发起响应把
// device_auth_id/user_code 交给发起者，完成调用时当场轮询一次并走既有 codex
// 凭据导入核心（单行 upsert），不新增落库路径。
//
// **信任边界（如实表述）**：device_auth_id + user_code 是一对短期 bearer 凭据，
// 持有二者的**任一已鉴权调用者**都能完成这次登录——网关不做「发起人绑定」
// （那需要签名/加密句柄或服务端会话，本设计刻意不要）。因此它们只在发起者
// 自己的浏览器上下文里流转、不落日志；落库归属由**完成调用者**的作用域决定
// （供应商面写本人，管理面写平台）。

// CodexDeviceCode 一次待授权的设备码会话（协议侧字段）。
//
// AuthID（device_auth_id）是轮询凭据：只存 Redis，不回显给最终用户（用户只需
// UserCode + VerificationURL）。
type CodexDeviceCode struct {
	AuthID          string
	UserCode        string
	VerificationURL string
	Interval        time.Duration // 轮询间隔（服务端下发；缺省 5s）
	ExpiresIn       time.Duration // 有效期；0 = 服务端未下发
}

// CodexLoginTokens 设备码登录换到的凭据束（完成导入用；id_token 只用于取
// 邮箱/账号标识，不落库）。
type CodexLoginTokens struct {
	AccessToken  string
	RefreshToken string
	AccountID    string // chatgpt_account_id（上游账号/空间标识）
	Email        string // 登录邮箱（组合幂等键之一）
	ExpiresAt    *time.Time
}

// CodexLoginStatus 设备码轮询状态（service → handler 契约，落响应体 status）。
type CodexLoginStatus string

const (
	// CodexLoginPending 用户尚未完成授权（会话继续保留，调用方稍后再轮询）。
	CodexLoginPending CodexLoginStatus = "pending"
	// CodexLoginAuthorized 授权完成（凭据已导入，会话已消费）。
	CodexLoginAuthorized CodexLoginStatus = "authorized"
	// CodexLoginExpired 设备码过期（会话已删除，需重新发起）。
	CodexLoginExpired CodexLoginStatus = "expired"
	// CodexLoginDenied 用户拒绝授权（会话已删除）。
	CodexLoginDenied CodexLoginStatus = "denied"
)

// —— 会话不入库 ——
//
// 设备码登录**没有服务端会话**：发起响应把 device_auth_id/user_code 交给发起者
// （已鉴权的管理台/供应商），用户浏览器完成授权后由前端触发一次「完成」调用，
// 服务端当场轮询一次并落库。生命周期完全由上游设备码有效期 + 浏览器决定，网关
// 不持有 15 分钟的中转状态——因此无 Redis 依赖、无 TTL 语义、无跨实例原子消费
// 问题。device_auth_id 是短期轮询凭据：只在发起者自己的浏览器上下文里流转，
// 响应/请求都不落日志（等同批量导入时管理员粘贴的凭据，暴露面更低）。

// CodexLoginResult 完成调用的结果（status 之外只在 authorized 填充）。
type CodexLoginResult struct {
	Status         CodexLoginStatus
	AccountID      int64 // 平台账号行 id（并发赢家之外为 0）
	AccountName    string
	Email          string
	CodexAccountID string
	Imported       bool // true = 新建账号；false + AccountID>0 = 更新既有账号凭据
}

// CodexLoginChallenge 发起登录的响应面：前端据此展示一次性码，并在用户完成
// 授权后把 DeviceAuthID/UserCode 回传「完成」端点（无服务端会话可查）。
type CodexLoginChallenge struct {
	DeviceAuthID     string
	UserCode         string
	VerificationURL  string
	IntervalSeconds  int
	ExpiresInSeconds int
}

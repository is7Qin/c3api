// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package domain 定义网关的核心领域类型；业务层（scheduler/proxy/service）只依赖本包。
package domain

import (
	"slices"
	"time"

	"github.com/is7qin/c3api/internal/credential"
)

type RequestFormat string

const (
	FormatOpenAIChat        RequestFormat = "openai-chat"
	FormatOpenAIResponses   RequestFormat = "openai-responses"
	FormatOpenAIResponsesWS RequestFormat = "openai-responses-ws" // Responses WS（Codex 客户端形态）
	FormatAnthropic         RequestFormat = "anthropic"
	// FormatOpenAIImages 图片生成（/v1/images/generations|edits，spec §4.3）：
	// JSON + multipart 双协议；预检查 image_price 表（GetImagePrice，跳过 chat
	// 价预检——P1-1 预检按格式切换）。落库 format = openai-images——usage_logs.format
	// 无 DB enum（varchar），ent 生成 FormatValidator 客户端面校验（COPY 逐行
	// 校验前置——不扩展则图片行 COPY 恒失败回灌）。
	FormatOpenAIImages RequestFormat = "openai-images"
	// FormatOpenAISearch codex search 端点格式（usage_logs 统一计费模型 spec
	// 2026-08-13）：为 search 按次计费铺路——search = 1 次功能调用（call_count=1）
	// + price_per_call_millis 按次价快照。**本 task 只扩枚举**（Valid/ent 枚举/
	// COPY FormatValidator——不扩展则 search 行 COPY 恒失败回灌，对齐 openai-
	// images 先例）；search 端点接入为独立 task，消费本枚举与 call_count 落账。
	FormatOpenAISearch RequestFormat = "openai-search"
)

func (f RequestFormat) Valid() bool {
	switch f {
	case FormatOpenAIChat, FormatOpenAIResponses, FormatOpenAIResponsesWS, FormatAnthropic, FormatOpenAIImages, FormatOpenAISearch:
		return true
	}
	return false
}

type AccountStatus string

const (
	StatusActive    AccountStatus = "active"
	StatusUnhealthy AccountStatus = "unhealthy"
	Status429       AccountStatus = "429"
	StatusDisabled  AccountStatus = "disabled"
)

// Role 用户角色（两级：platform_admin | user）。
type Role string

const (
	RolePlatformAdmin Role = "platform_admin"
	RoleUser          Role = "user"
)

func (r Role) Valid() bool {
	switch r {
	case RolePlatformAdmin, RoleUser:
		return true
	}
	return false
}

// UserStatus 用户状态。
type UserStatus string

const (
	UserStatusActive   UserStatus = "active"
	UserStatusDisabled UserStatus = "disabled"
)

func (s UserStatus) Valid() bool {
	switch s {
	case UserStatusActive, UserStatusDisabled:
		return true
	}
	return false
}

// UserSnapshot 用户快照条目（Auth 内存表元素：RequireJWT/adminAuth 共用，
// 单次查找同时取 status+role——降权即时生效的 role 数据源，adminAuth 不再
// 单独信任 claims.Role）。
type UserSnapshot struct {
	Status UserStatus
	Role   Role
}

// KeyStatus 客户端 key 状态。
type KeyStatus string

const (
	KeyStatusActive   KeyStatus = "active"
	KeyStatusDisabled KeyStatus = "disabled"
)

func (s KeyStatus) Valid() bool {
	switch s {
	case KeyStatusActive, KeyStatusDisabled:
		return true
	}
	return false
}

// GroupVisibility 组可见性：public 全部用户可选；private 仅授予用户。
type GroupVisibility string

const (
	GroupVisibilityPublic  GroupVisibility = "public"
	GroupVisibilityPrivate GroupVisibility = "private"
)

func (v GroupVisibility) Valid() bool {
	switch v {
	case GroupVisibilityPublic, GroupVisibilityPrivate:
		return true
	}
	return false
}

// SettingType settings 值类型。
type SettingType string

const (
	SettingTypeSwitch SettingType = "switch"
	SettingTypeNumber SettingType = "number"
	SettingTypeString SettingType = "string"
)

func (t SettingType) Valid() bool {
	switch t {
	case SettingTypeSwitch, SettingTypeNumber, SettingTypeString:
		return true
	}
	return false
}

type ErrorType string

const (
	ErrNone      ErrorType = "none"
	Err429       ErrorType = "429"
	Err4xx       ErrorType = "4xx"
	Err5xx       ErrorType = "5xx"
	ErrNetwork   ErrorType = "network"
	ErrAuth      ErrorType = "auth"
	ErrNoAccount ErrorType = "no_account"
	ErrAbort     ErrorType = "abort"
	ErrBilling   ErrorType = "billing" // 计费拒绝（缺价/余额不足 402）
)

// ErrMsgMaxLen 错误文本域内截断上限（usagelog.error_message varchar(500)
// 与 accounts.last_error 共用；部署故障修复：错误文本留痕但列长度有界）。
const ErrMsgMaxLen = 500

// TruncateErrMsg 把错误文本截断到 ErrMsgMaxLen 字符（按 rune 截断，不拆断
// 多字节 UTF-8）。仅错误分支调用（成功路径不经过），热路径零成本——短文本
// （≤500 字符，含全部 ASCII 错误文案）直接返回不分配。
func TruncateErrMsg(s string) string {
	if len(s) <= ErrMsgMaxLen {
		return s
	}
	r := []rune(s)
	if len(r) <= ErrMsgMaxLen {
		return s
	}
	return string(r[:ErrMsgMaxLen])
}

type Template struct {
	ID               int64
	Name             string
	BaseURL          string
	CredentialType   credential.Type            // 模板级：默认 api_key（DB 默认；号池生态类型后续）
	SupportedFormats []RequestFormat            // 模板支持的格式（非空、去重）
	Models           []string                   // 可服务模型集合
	FormatModels     map[RequestFormat][]string // 格式 → 该格式支持的模型列表；未配置 = 全部 Models
	ModelMapping     map[string]string
	// StripImageTools 模板级图像 tool 剥离开关（template_ext.strip_image_tools
	// 快照合并，W4 消费；三类型 responses-special/codex-oauth/codex-pat 公共
	// 能力）：true = response.create 帧出口剥离图像工具（tools 数组 +
	// tool_choice 悬挂；input 内嵌 v1 图像内容不做）。热路径快照布尔读 + 分支
	// 零开销；false = 未配置/关闭（nil 与 false 同语义，快照收敛为 bool）。
	StripImageTools bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
	DeletedAt       *time.Time // 软删除时间戳；nil = 存活（列表/消费路径过滤；GET 单个可查已删）
}

// FormatsFor 模板支持的格式列表。
func (t *Template) FormatsFor() []RequestFormat { return t.SupportedFormats }

// FormatSupports 格式 f 是否支持模型 m（模型级限制）：
// f 不在 SupportedFormats → false；FormatModels[f] 配置了 → m ∈ 列表；未配置 → true。
func (t *Template) FormatSupports(f RequestFormat, m string) bool {
	if !slices.Contains(t.SupportedFormats, f) {
		return false
	}
	if list, ok := t.FormatModels[f]; ok {
		return slices.Contains(list, m)
	}
	return true
}

// Serves 模型是否在可服务集合（models ∪ format_models 全部列表 ∪ mapping keys）内。
func (t *Template) Serves(m string) bool {
	if slices.Contains(t.Models, m) {
		return true
	}
	for _, list := range t.FormatModels {
		if slices.Contains(list, m) {
			return true
		}
	}
	if _, ok := t.ModelMapping[m]; ok {
		return true
	}
	return false
}

// HasModelSpace 模型空间是否已配置（Models/FormatModels/ModelMapping 任一非空）。
// 空 = 未配置 = 全模型支持（硬白名单语义下归 tier2 兜底桶）。
func (t *Template) HasModelSpace() bool {
	return len(t.Models) > 0 || len(t.FormatModels) > 0 || len(t.ModelMapping) > 0
}

type Account struct {
	ID         int64
	Name       string
	TemplateID int64
	Template   *Template
	// BaseURL 账号级 base_url 覆盖（路由属性，列位于 upstream_key 前——用户
	// 裁决 2026-08-14）：nil = 继承模板 base_url；非空 = 覆盖模板值（api_key/
	// responses-special 静态透传的兜底——模板留空则账号级可补）。DB 恒
	// nil|非空两种形态（create 路径空串归一 nil、批量 "" 落 NULL）。
	BaseURL        *string
	UpstreamKey    string
	Status         AccountStatus
	CooldownUntil  *time.Time
	Weight         int
	MaxConcurrency int
	LastError      *string
	LastUsedAt     *time.Time
	// FailedAt SDK 上报的运行时失效时刻（account.failed_at 列，SDK 接入 T1——
	// 用户裁决 2026-08-13：仅此一列；失效原因复用既有 LastError，两原因字段
	// 并存会漂移）：nil = 未失效；非 nil = 账号级终止（凭据永久失效/上游封禁/
	// 判死）的上报时刻。与 Status=disabled 语义分离：disabled = 管理面手动禁用；
	// failed_at = 运行时失效，两者可并存（失效后管理员仍可手动处理；恢复 =
	// 清 failed_at + last_error + 恢复调度，T5 细化）。调度器选号不读本字段
	//（pickFrom 只跳 disabled——摘除必须落库 status）。
	FailedAt  *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time // 软删除时间戳；nil = 存活（列表/消费路径过滤；GET 单个可查已删）
	// GroupIDs 写路径（创建/更新）专用：nil = 不设置/不变；非 nil = 替换账号
	// 全部分组（含空数组 = 清空）。读路径忽略——编辑回显走 GetAccountGroups
	// 独立查询（toDomainAccount 不填充该字段）。
	GroupIDs *[]int64
	// Ext 账号类型化鉴权扩展（account_ext 1:1 边；调度器快照加载
	// LoadGroupsAccounts/LoadGroupAccounts 合并——与 Template.StripImageTools
	// 同款快照合并先例，sdk-wiring T4 P3-4 定死路线；T2 起 codex 路由按 Ext
	// 派生 AccountCredential）。其余路径（管理面账号 CRUD 等）无 ext 边 → nil。
	Ext *AccountExt
}

// TemplateExt 模板类型化扩展配置（template_ext 子表，1:1）：credential_type
// ∈ {responses-special, codex-oauth, codex-pat} 的模板才有 ext 行（api_key 主列
// 类型无 ext 行）。模板是共享配置面：只承载类型声明 + StripImageTools 公共
// 能力开关（三类型通用）——凭据列组（oauth/pat）一律在账号级 account_ext
// （账号私有）。nil = 未配置（NULL 落库）。
type TemplateExt struct {
	TemplateID      int64
	CredentialType  credential.Type
	StripImageTools *bool // 三类型公共能力开关：模板级图像 tool 剥离（W4 消费）
}

// CodexIdentity codex 账号身份四元组（对齐真实客户端语义：installation_id
// 安装级永久；session/thread 会话级恒等；window = {thread_id}:{n} 起始 :0）——
// account_ext.codex_identity jsonb 序列化契约类型（json tag 即落库形态）。
// 空字段 = 未提供（service 归一：全空 → 自动生成/沿用存量；identity 无清空
// 路径——账号存在期间稳定）。
type CodexIdentity struct {
	InstallationID string `json:"installation_id"`
	SessionID      string `json:"session_id"`
	ThreadID       string `json:"thread_id"`
	WindowID       string `json:"window_id"`
}

// AccountExt 账号类型化鉴权扩展（account_ext 子表，1:1）：credential_type
// ∈ {codex-oauth, codex-pat}（账号只两种 codex 类型）。字段按作用分组：
// 标识 → 身份（codex_identity jsonb 单列）→ 凭据 → 管理标识。身份四元组
// （对齐真实 codex 客户端语义，导入时 service NewCodexIdentity() 自动生成并
// 持久化、账号存在期间稳定）：InstallationID 必存（UUIDv4 安装级永久）；
// SessionID/ThreadID UUIDv7 会话级（恒等 thread==session）；WindowID =
// {thread_id}:0（导入时生成后恒定不变——零递增零状态）。nil = 未配置。
// 凭据列组按类型约束（service 校验）：oauth 只允许 CodexOAuth* 列组；pat
// 只允许 CodexPATKey。CodexAccountID 为上游账号/空间标识（Task B 批量导入
// 必填；本 task 仅建结构——管理面写入能力 Task B 接线）。
type AccountExt struct {
	AccountID              int64
	CredentialType         credential.Type
	CodexIdentity          *CodexIdentity `json:"codex_identity"` // 身份四元组（jsonb；nil = 未配置/异常）
	CodexOAuthToken        *string        // 凭据：oauth 访问令牌
	CodexOAuthRefreshToken *string        // 凭据：oauth 刷新令牌
	CodexOAuthExpiresAt    *time.Time     // 凭据：oauth 访问令牌过期时间
	CodexPATKey            *string        // 凭据：pat
	CodexEmail             *string        // 管理标识：账号登录邮箱（导入时人工/上游提供，非自动生成，可空）
	CodexAccountID         *string        // 上游账号/空间标识（Task B 导入必填；可空）
}

// CodexOAuthImportItem 批量导入 codex-oauth 单行（Task B——组合幂等键
// codex_email + codex_account_id；token+refresh 成对必填；expires_at 可选
// 原始 RFC3339 字符串（service 逐行解析——格式错误 → 行级 failed 非整批
// 400；nil = 过期未知 → 401 自愈）；max_concurrency/weight 配置面——nil =
// 缺省 25/100（导入面裁决覆盖账号表默认 8））。
type CodexOAuthImportItem struct {
	CodexEmail             string
	CodexAccountID         string
	CodexOAuthToken        string
	CodexOAuthRefreshToken string
	CodexOAuthExpiresAt    *string
	MaxConcurrency         *int
	Weight                 *int
}

// CodexPATImportItem 批量导入 codex-pat 单行（组合键同上；pat_key 必填）。
type CodexPATImportItem struct {
	CodexEmail     string
	CodexAccountID string
	CodexPATKey    string
	MaxConcurrency *int
	Weight         *int
}

// ImportFailedItem 行级失败条目（index = items 原始下标——行级定位契约；
// error 为该行失败原因文案）。
type ImportFailedItem struct {
	Index int
	Error string
}

// ImportResult 批量导入结果（imported 新建 / updated 键存在更新凭据 /
// failed 行级失败——单行失败不毁整批，整批不原子）。
type ImportResult struct {
	Imported int
	Updated  int
	Failed   []ImportFailedItem
}

// CodexUsageSnapshot 账号 codex 额度快照（白名单收敛契约——spec 2026-08-18
// Task 3：SDK UsageStatus 五块砍四留四——RateLimitReachedType（与 allowed=false
// 重复的派生状态）与瞬时布尔（Allowed/LimitReached/HasCredits/Unlimited/
// OverageLimitReached——5min 缓存下已过时）与 ApproxLocalMessages/
// ApproxCloudMessages（[]any 未定型数组——不进契约）一律不出现）。每块 nil →
// omitempty（上游没返回就不出字段——个人订阅账号可能无 SpendControl、team
// 账号可能无 Credits.Balance；上游返回什么透什么，零填充）。金额字段为字符串
// 不解析（SDK 有意保精度）。快照为尽力视图（展示面参考，非计费依据——真实
// 账本 = usage_logs）。
type CodexUsageSnapshot struct {
	PlanType     string             `json:"plan_type,omitempty"`
	RateLimit    *CodexRateLimit    `json:"rate_limit,omitempty"`    // 窗口用量
	Credits      *CodexCredits      `json:"credits,omitempty"`       // 充值余额
	SpendControl *CodexSpendControl `json:"spend_control,omitempty"` // 消费限额（花了多少）
}

// CodexRateLimit 主窗口用量（ResetAt = SDK Unix 秒 → time.Time，JSON RFC3339；
// 指针 + omitempty——上游主窗口省略 reset_at（Unix 0）→ nil → 不出字段，零填充
// 语义：虚假 0001-01-01T00:00:00Z 不外泄）。
type CodexRateLimit struct {
	UsedPercent int        `json:"used_percent"`
	ResetAt     *time.Time `json:"reset_at,omitempty"`
}

// CodexCredits 充值余额（Balance 金额字符串——如 "12.50"，不解析；指针 +
// omitempty——上游空串 → nil → 不出字段，零填充语义）。
type CodexCredits struct {
	Balance *string `json:"balance,omitempty"`
}

// CodexSpendControl 消费控制额度（Limit/Used/Remaining 金额字符串）。
type CodexSpendControl struct {
	Limit            string `json:"limit"`
	Used             string `json:"used"`
	Remaining        string `json:"remaining"`
	UsedPercent      int    `json:"used_percent"`
	RemainingPercent int    `json:"remaining_percent"`
}

// UsageAgg 账号 usage_logs 区间聚合行（ScanUsageAgg 行形态——统一 usage API
// 查询面 spec 2026-08-18）：毫分 int64 原样聚合（USD 换算在 handler 展示边界
// /1e5，temp-balances 先例），不做逐行事后计算。
type UsageAgg struct {
	AccountID   int64
	Requests    int64 // COUNT(*)
	Cost        int64 // SUM(cost)，毫分（计费成本）
	RawCost     int64 // SUM(raw_cost)，毫分（乘倍率前原始成本）
	TotalTokens int64 // SUM(total_tokens)
}

// AccountUsageUpstreamError 上游快照失败分类标记（upstream_error 枚举——
// task 3 sdkbridge 错误分类即映射输入：ErrAuthExpired → auth_expired、
// ErrUpstream → upstream_unavailable；api-key 无凭据/"缺失"恒 nil，不进
// auth_expired）。
type AccountUsageUpstreamError string

const (
	UpstreamErrorAuthExpired         AccountUsageUpstreamError = "auth_expired"         // 凭据失效（sdkbridge fatal 类）
	UpstreamErrorUpstreamUnavailable AccountUsageUpstreamError = "upstream_unavailable" // 网络/5xx 等上游错误（ErrUpstream/其余）
)

// AccountUsage 账号 usage 视图 item（/api/admin/accounts/usage 统一 usage API 查询
// 面）：Gateway 恒全量（无记录全 0——前端免补零）；Upstream 为 codex 额度快照
// （task 3 sdkbridge；api-key/无凭据账号恒 nil）；UpstreamError 为快照失败标记
// （成功/nil 上游恒 nil——"无上游能力"与"快照挂了"区分）。
type AccountUsage struct {
	AccountID     int64
	Gateway       UsageAgg
	Upstream      *CodexUsageSnapshot
	UpstreamError *AccountUsageUpstreamError
}

type Group struct {
	ID         int64
	Name       string
	Visibility GroupVisibility
	// PriceMultiplier 万分数（T3.5 价格倍率）：组默认 10000 = ×1；0 = 免费。
	// 写路径语义：Create 缺省（nil，service 归一为 10000）恒写入；Update 恒写入
	// （PUT 全量替换）。API 边界（handler/convert.go）与正常值 float64 换算
	// （1.5 ↔ 15000）。
	PriceMultiplier int
	// ProtocolConverts 分组级协议转换方向集合（只补差，W5 消费；多方向并存按
	// 客户端格式命中——chat 请求走 chat_to_*、anthropic 请求走 mess_to_resp、
	// resp 请求走 resp_to_mess）：空集合 = off = 不转换（off 不进数组）。
	// 写路径语义：Create 缺省（handler 归一为空数组）恒写入；Update 恒写入
	// （service 校验集合——非法方向/重复方向/同客户端格式多方向 400；显式
	// 空数组 = 清空）。
	ProtocolConverts []ProtocolConvert
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time // 软删除时间戳；nil = 存活（列表/消费路径过滤；GET 单个可查已删）
}

// ProtocolConvert 分组级协议转换方向枚举（补差语义：模板已支持客户端协议 →
// 直接转发零转换；转换仅在协议不匹配时发生）。方向 = 客户端协议 → 模板协议；
// off（不转换）不在枚举内——空集合表达（数组元素 ∈ 4 方向；集合校验在
// service 层——非法方向/重复/同客户端格式多方向 400）。
type ProtocolConvert string

const (
	// ProtocolConvertOff 不转换（空数组语义；不进方向数组，service 归一剔除）。
	ProtocolConvertOff ProtocolConvert = "off"
	// ProtocolConvertChatToResp 客户端 chat → 模板 resp 协议。
	ProtocolConvertChatToResp ProtocolConvert = "chat_to_resp"
	// ProtocolConvertMessToResp 客户端 messages（anthropic）→ 模板 resp 协议。
	ProtocolConvertMessToResp ProtocolConvert = "mess_to_resp"
	// ProtocolConvertRespToMess 客户端 resp → 模板 messages（anthropic）协议。
	ProtocolConvertRespToMess ProtocolConvert = "resp_to_mess"
	// ProtocolConvertChatToMess 客户端 chat → 模板 messages（anthropic）协议。
	ProtocolConvertChatToMess ProtocolConvert = "chat_to_mess"
)

func (p ProtocolConvert) Valid() bool {
	switch p {
	case ProtocolConvertChatToResp, ProtocolConvertMessToResp,
		ProtocolConvertRespToMess, ProtocolConvertChatToMess:
		return true
	}
	return false
}

// User 用户（顶层实体，无租户）。标识 = 邮箱；PasswordHash 为 bcrypt
// DefaultCost(10)（与 sub2api 同参数，存量 hash 可迁移验证）。
// Balance 最小单位（毫分；1 USD = 100,000 毫分，Phase 5 计费统一单位，
// 管理面 API 展示/输入换算 USD）。
// 价格倍率按组（T3.5 修正）：挂在 group_assignment 上（GroupAssignment.
// PriceMultiplier），用户不同组可有不同倍率——User 无倍率字段。
type User struct {
	ID             int64
	Email          string
	PasswordHash   string
	Role           Role
	Status         UserStatus
	MaxConcurrency int
	Balance        int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Key 客户端 API key（独立表，重建 group 内嵌 key 语义）。
type Key struct {
	ID             int64
	UserID         int64
	GroupID        int64
	Name           string
	KeyRaw         string // 明文常驻（长期可查看/复制；DB 泄露即明文暴露——自托管权衡）
	Status         KeyStatus
	MaxConcurrency int
	Quota          int64 // 累计 token 上限；0 = 不限
	QuotaUsed      int64 // 已消耗（后扣；无额度 key 恒 0）
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeletedAt      *time.Time // 软删除时间戳；nil = 存活（鉴权/列表过滤；GET 单个可查已删）
}

// HasQuota 是否有额度上限（quota > 0）。热路径门禁/扣减短路标志。
func (k *Key) HasQuota() bool { return k.Quota > 0 }

// GroupAssignment private 组的授予记录（用户 ↔ 组多对多）。
// PriceMultiplier 该用户在该组的专属价格倍率（万分数，T3.5 修正：按组——
// 用户在不同组可有不同倍率）；nil = 未设置 → 用组倍率；0 = 免费。
type GroupAssignment struct {
	ID              int64
	GroupID         int64
	UserID          int64
	PriceMultiplier *int
	CreatedAt       time.Time
}

// Setting 类型化配置（key/type/value；signup_enabled 注册开关等）。
// Min/Max 数值值域（含边界；nil = 无限制）、PolicyValues 字符串枚举值域
// （空 = 不限）——仅注册表条目携带（管理面 UpdateSetting 校验用，A-P2-11 护栏
// 前置）；DB 行不落库（读路径由注册表默认兜底合并，见 SettingRepo.GetAll）。
type Setting struct {
	ID           int64
	Key          string
	Type         SettingType
	Value        string
	Min          *int64   // 数值最小值（含）；nil = 无下限
	Max          *int64   // 数值最大值（含）；nil = 无上限
	PolicyValues []string // 字符串枚举值域（如 service_tier 策略 key）；空 = 不限
	UpdatedAt    time.Time
}

// KeyMeta 鉴权快照条目：key + 归属用户的关键门禁字段（Auth 内存表元素，
// repository.LoadKeys 构建；热路径零 DB 读取）。
type KeyMeta struct {
	KeyID       int64
	UserID      int64
	GroupID     int64
	KeyStatus   KeyStatus
	KeyMaxConc  int
	UserStatus  UserStatus
	UserMaxConc int
	HasQuota    bool
	Quota       int64
	QuotaUsed   int64 // 快照值（reload 时从 DB 读）；在途扣减走内存计数
	// ProtocolConverts 组级协议转换方向集合快照值（W5）：空 = 不转换（热路径
	// 分支零开销）；元素 = 客户端协议 → 模板协议（补差语义，转换器
	// internal/protoconv；多方向按客户端格式命中，同客户端格式多方向已被
	// 创建/更新校验拒绝）。
	ProtocolConverts []ProtocolConvert
}

// UsageLog 用量日志：user_id/key_id 为鉴权归属（context 传递，0 = 无）。
// 计费列（Phase 5）：Cost 毫分（1 USD = 100,000 毫分）；BillingTier 请求
// service_tier 归一化值（priority/flex/fast/auto，空 = 未计费路径）；AboveHit
// 任一分量超 above 阈值命中分段；Overdraft 本次扣费透支（负余额）。
// ErrorMessage 错误文本（部署故障修复）：连接级 err.Error() / 4xx+ 上游 body，
// 域内截断 500 字符（TruncateErrMsg）；nil = 无错误文本（成功路径恒空）。
// 时间/价格快照（字段序对齐 ent schema）：TTFTMS 首 token 时间毫秒（流式首
// chunk 采集；非流式/失败/无首 token 路径 = nil）；Price*Millis 每 M token 毫分
// 单价快照（1 USD = 100,000 毫分，pricing 同款单位；applyBilling 填充，零额外
// 查找）——nil = 未计费路径（no_price 防御）；缓存价 nil = 该请求无缓存读或
// 无缓存价。
// 统一计费模型（spec 2026-08-13）：功能调用分量 = CallCount（图片生成 = 张数
// data 长/completed 事件数、search = 1）+ PricePerCallMillis 按单元价快照
// （毫分/单元——search 每次 / 图片每张）。**call_count 不入 TotalTokens**
// （功能调用非 token——对齐原 image_count 语义；离线聚合 sum(call_count) = 功能
// 调用量，进 StatBucket.CallCount——spec 2026-08-14 用户裁决按次调用入桶与展示）。
// 图片生成 image token 已并入 InputTokens/OutputTokens（原 image_input/output_tokens 六
// 列删除——image token 价快照列随之删除，cost 口径不变：ImageCost 仍按
// 张数 × 每张价 + image token 价计算）。
type UsageLog struct {
	ID                       int64
	RequestID                string
	ClientIP                 string // 客户端 IP（供应商头按序识别 + RemoteAddr 兜底——proxy.behind_cdn 门控；审计/排障标识，非安全边界；空 = 理论无（提取恒有兜底））
	GroupID                  int64  // 0 = 无
	AccountID                int64  // 0 = 无
	TemplateID               int64  // 0 = 无
	UserID                   int64  // 0 = 无（鉴权失败/无 key）
	KeyID                    int64  // 0 = 无
	Model                    string
	MappedModel              string // 空 = 未映射
	Format                   RequestFormat
	StatusCode               int
	ErrorType                ErrorType
	ErrorMessage             *string // nil = 无错误文本（NULL 落库）
	LatencyMS                int64
	TTFTMS                   *int64 // 首 token 时间毫秒；非流式/失败/无首 token 路径 = nil
	InputTokens              int64  // 输入 token（图片生成含 image input tokens——已并入）
	PriceInputMillis         *int64 // 输入单价快照（每 M token 毫分）
	OutputTokens             int64  // 输出 token（图片生成含 image output tokens——已并入）
	PriceOutputMillis        *int64 // 输出单价快照（每 M token 毫分）
	TotalTokens              int64  // 含 image tokens、不含 call_count（口径不变）
	CacheReadTokens          int64  // 缓存读取 token（跨协议归一化，sub2api 计费语义）
	PriceCacheReadMillis     *int64 // 缓存读单价快照；nil = 无缓存读或无缓存价
	CacheCreationTokens      int64  // 缓存写入 token（OpenAI ephemeral 5m/1h 聚合）
	PriceCacheCreationMillis *int64 // 缓存写单价快照；nil = 无缓存写或无缓存价
	CallCount                int64  // 功能调用计数：图片生成 = 张数（data 长/completed 事件数）、search = 1；不入 TotalTokens（功能调用非 token）
	PricePerCallMillis       *int64 // 按单元价快照（**毫分/单元**——search 每次 / 图片每张；例外单位，例外于上文"毫分/1M"口径——per-call 计费不走 /1e6 除法）；nil = 无按单元分量
	Cost                     int64  // 毫分；错误请求（402/4xx）为 0
	// RawCost 乘倍率前的原始成本（毫分；免费组 cost=0 但 raw 有值——"实际消耗"
	// 只有 raw 能看）。回显面投影归 Task 2（本字段无 json tag，对齐 domain 全
	// 结构惯例）。
	RawCost     int64  // 毫分；bill 未装配/无价防御路径恒 0
	BillingTier string // priority/flex/fast/auto；空 = 未计费路径
	AboveHit    bool
	Overdraft   bool
	// Billed 扣费收敛标记（F2 ledger-cursor，spec 2026-08-23）：false=待对账
	// 消费者扣减；true=扣费事务已完成（或出生吸收态——计费关闭/匿名行）。
	// 出生标记由 proxy.routeLog 按 NOT BillingCapture OR UserID<=0 盖章；
	// 翻转为 true 只发生在对账事务内（与 FEFO 扣减同事务原子）。
	Billed     bool
	CreatedAt  time.Time
}

// LedgerRow 计费游标消费行（usage_logs 未扣子集的瘦身投影；spec-f2-ledger-cursor
// ABI-1 冻结契约）：FetchUnbilledBatch 返回、按 UserID 分组进 DeductOnlyAndMark。
type LedgerRow struct {
	ID          int64
	UserID      int64
	Cost        int64
	Model       string
	BillingTier string
	CallCount   int64
	Format      string
}

// StatBucket 小时统计桶（usage_stats 行；离线聚合 worker 的 INSERT 行形态）。
// 请求路径零统计计算（spec 2026-08-14）：usage_stats 只由离线聚合 worker 写入
// （DELETE+INSERT 覆盖语义）——total_latency_ms 已删除（延迟列出局）；TTFT 由
// ttft_total_ms/ttft_count/ttft_max_ms/ttft_hist 四列承载（avg 在查询侧 Go 除；
// ttft_hist 10 档直方图见 stat_repo.go ttftHistBounds）。ttft_hist 为 PG bigint[]
// 数组列——ent 无数组类型（field.Ints 是 JSON 语义），不进 ent schema（carve-out，
// 评审 P1-1；统计读取面走 pgx 直查扫描）。v2 瘦身（spec 2026-08-23）：维度
// 7→3——account_id/template_id/user_id/is_error 四维删除（实体视角由
// EntityStatBucket/usage_entity_stats 承载；is_error 降为 error_count 测量列
// 语义），唯一键 = (bucket_time, group_id, model)。
type StatBucket struct {
	BucketTime          time.Time // 对齐到小时（UTC）
	GroupID             int64     // 0 = 无
	Model               string
	RequestCount        int64
	ErrorCount          int64
	InputTokens         int64
	OutputTokens        int64
	TotalTokens         int64
	CacheReadTokens     int64   // 缓存读取 token
	CacheCreationTokens int64   // 缓存写入 token
	Cost                int64   // 毫分（计费预聚合，花费统计不扫明细）
	RawCost             int64   // 毫分（乘倍率前原始成本——免费组 cost=0 但 raw 有值）
	CallCount           int64   // 按次调用：图片生成 = 张数、search = 1（离线聚合 sum(call_count) 直取）
	TTFTTotalMS         int64   // TTFT sum（avg = 查询侧 Go 除 TTFTCount）
	TTFTCount           int64   // TTFT 样本数（仅首 token 流式请求；abort 行含 TTFT 也计入其桶）
	TTFTMaxMS           int64   // TTFT max
	TTFTHist            []int64 // TTFT 直方图 10 档（len = 10；SQL 侧 count(*) FILTER 生成）
}

// EntityStatBucket 实体小时卷积桶（usage_entity_stats 行语义）。
type EntityStatBucket struct {
	BucketTime          time.Time
	EntityType          string // 'account' | 'user' | 'key'
	EntityID            int64
	Model               string
	RequestCount        int64
	ErrorCount          int64
	CallCount           int64
	InputTokens         int64
	OutputTokens        int64
	TotalTokens         int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	Cost                int64
	RawCost             int64
	TTFTTotalMS         int64
	TTFTCount           int64
	TTFTMaxMS           int64
}

// TTFTSummary TTFT 卡片聚合（/stats/ttft 与 /api/user/stats/ttft 响应内核）。
// Source 恒标分支身份："exact"（usage_logs percentile_cont 精确）或 "sketch"
// （cube hist 服务端合并插值）；空窗 = 零值结构（Count:0），Source 仍标分支
// ——调用方以 Count 判空，不以 Source 判空。
type TTFTSummary struct {
	Count  int64
	AvgMS  int64
	P50MS  int64
	P95MS  int64
	P99MS  int64
	MaxMS  int64
	Source string
}

// —— 规则引擎（可编排状态管理） ——
type RuleWhen struct {
	Kind                   *string  `json:"kind,omitempty"`
	HTTPStatus             *int     `json:"http_status,omitempty"`
	ErrorMessageContains   *string  `json:"error_message_contains,omitempty"`
	AccountID              *int64   `json:"account_id,omitempty"`
	TemplateID             *int64   `json:"template_id,omitempty"`
	GroupID                *int64   `json:"group_id,omitempty"`
	Model                  *string  `json:"model,omitempty"`
	HTTPStatusIn           []int    `json:"http_status_in,omitempty"`            // 与 HTTPStatus 互斥，同字段 OR
	ModelIn                []string `json:"model_in,omitempty"`                  // 与 Model 互斥，同字段 OR，语义=最终请求模型 sel.Model/mapped
	ErrorMessageContainsIn []string `json:"error_message_contains_in,omitempty"` // 与 ErrorMessageContains 互斥，任一子串命中
	WindowSeconds          *int     `json:"window_seconds,omitempty"`
	Count429GE             *int     `json:"count_429_ge,omitempty"`
	// CountFailureGE 语义 = "失败事件桶"（非 ok 非 429 事件计数——5xx/network/4xx
	// 并入）。
	CountFailureGE *int     `json:"count_failure_ge,omitempty"`
	CountOKGE      *int     `json:"count_ok_ge,omitempty"`
	CountTotalGE   *int     `json:"count_total_ge,omitempty"`
	Ratio429GE     *float64 `json:"ratio_429_ge,omitempty"`
	// RatioFailureGE 同 CountFailureGE：分母为失败事件桶（5xx/network/4xx 并入）。
	RatioFailureGE *float64 `json:"ratio_failure_ge,omitempty"`
}

type RuleThen struct {
	Status   *AccountStatus `json:"status,omitempty"`
	Cooldown *string        `json:"cooldown,omitempty"` // time.ParseDuration 可解析的时长，如 "30s"、"5h"
	Weight   *int           `json:"weight,omitempty"`   // 0-100
	// ResponseCode nil=透传上游码，non-nil=覆写为指定码（400-599）；指针即意图（fresh setup，无旧 Transmit 兼容）。
	ResponseCode *int `json:"response_code,omitempty"`
	// CustomMessage nil=透传上游文，non-nil=覆写为固定文案（禁止空串）；指针即意图。
	CustomMessage *string `json:"custom_message,omitempty"`
	// 启动 guard 检测旧列 Transmit：fresh setup 哲学，用户裁决；旧列存在则 fail-fast 需重建（本 Task 仅注释占位，DB 检测由后续迁移承载）。
}

type Rule struct {
	ID        int64
	Name      string
	Enabled   bool
	Priority  int
	When      RuleWhen
	Then      RuleThen
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time // 软删除时间戳；nil = 存活（列表/规则引擎重载过滤）
}

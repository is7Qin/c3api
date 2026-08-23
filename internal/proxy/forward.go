// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package proxy 是 AI 请求热路径：分组 key 鉴权 → 调度器选号 → SDK 转发 → 用量采集。
// 规格 §6/§9。不变量：热路径零 DB、零 per-request 锁。
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
	"github.com/is7qin/c3api/pkg/logx"
)

type Config struct {
	MaxBodySize           int64
	MaxInflight           int64
	UpstreamTimeout       time.Duration // codex 非流式上游超时（resp/images 各自包 ctx——B-P2-7；HTTPClient.Timeout 不可用：流式/非流式四方法共享，覆盖整响应体读取会切断长流式 SSE）。同源同值 cfg.Proxy.UpstreamTimeout（aiclient.Config.UpstreamTimeout 管 typed 面）
	UpstreamStreamTimeout time.Duration // 流式 backstop（非流式超时在 aiclient.Config/cfg.Proxy.UpstreamTimeout）
	FailoverAttempts      int
	GroupKeyRPM           int
	UsageCapture          bool
	BillingCapture        bool // 计费开关（config.Billing.Enabled 映射；余额预检门控 + billable 行 Billed 出生标记取反——F2 单写点后不再路由分流）
	// BehindCDN 客户端 IP 识别开关（config.proxy.behind_cdn 映射；clientIP
	// 提取门控——false 完全不读供应商头直取 RemoteAddr，true 按序采信三头）。
	// 部署前提见 config.go 注释与 clientip.go：源站只对 CDN 暴露。
	BehindCDN bool
}

type Proxy struct {
	cfg     Config
	sched   *scheduler.Scheduler
	creds   *credential.Registry
	rec     *usage.Recorder
	clients *aiclient.Factory
	auth    *Auth
	limit   *fixedWindowLimiter
	log     *logx.Logger
	bill    *BillingHooks // 计费钩子；nil = 计费全关
	// errlog 错误明细落盘 worker（分表设计；nil = 未装配——拒绝/异常路径只聚
	// 合统计不落 err_logs 明细，测试/未装配形态）。与计费 flusher 完全解耦。
	errlog   *usage.ErrLogWorker
	inflight atomic.Int64
	callers  map[domain.RequestFormat]UpstreamCaller // 格式 → 上游调用器（New 构造，零查找 per-request 只一次 map 读）
	// imageGenerations/imageEdits images 端点调用器（Task B：同一格式
	// openai-images 两个端点，上游子路径不同——handleFormat 按请求路径选
	// 调用器，New 一次性构造免 per-request 分配）。
	imageGenerations *imagesCaller
	imageEdits       *imagesCaller
	// codexImagesGenerations/codexImagesEdits codex 类型 images 端点调用器
	//（T2 §2：SDK GenerateImage 非流式；同一格式两端点——按请求路径选，
	// New 一次性构造免 per-request 分配）。
	codexImagesGenerations *codexImagesCaller
	codexImagesEdits       *codexImagesCaller
	// convCallers 协议转换路径调用器（W5）：方向 → convertedCaller（请求体已
	// 按方向转换，响应反向转换回客户端协议）。仅协议不匹配时才使用；off 组
	// 恒不触达（handleFormat 分支）。
	convCallers map[domain.ProtocolConvert]UpstreamCaller
	// codex SDK 适配层（T2 §1——cred → Auth 缓存 / GenerateImage / 信封 /
	// fatal 统一回调全在适配层；main 装配 SetCodex 注入，nil = 未装配 → codex
	// 类型 501 显式拒绝——防 nil 误走凭据缺失 502）。
	codex *sdkbridge.Codex
	// wsHeartbeatInterval resp-ws 心跳间隔 seam（T4：测试缩短 200ms 验证心跳节
	// 奏；默认 responsesWSHeartbeatInterval——New 构造，生产路径不变）。
	wsHeartbeatInterval time.Duration
	// failover 骨架的单例 attempt/sink（D3 管线骨架化）：无状态（per-request
	// 差异经 attemptState 按值流入——热路径零新增分配，同 callers map 惯例），
	// New 一次性构造。
	chatAttempt   upstreamAttempt
	searchAttempt upstreamAttempt
	wsAttempt     upstreamAttempt
	httpSink      pipelineSink
	wsSink        pipelineSink
}

// New 构造代理。creds 为凭据注册表（评审 M2：直接参数注入，编译期强制；
// 不用 Config 字段——避免 nil 运行时才炸）。bill 为计费钩子（Phase 5；
// nil = 计费全关——现有调用点/测试兼容）。errlog 为错误明细落盘 worker
// （分表设计；nil = 未装配——拒绝/异常路径只聚统计不落 err_logs 明细）。
func New(cfg Config, sched *scheduler.Scheduler, creds *credential.Registry, rec *usage.Recorder, clients *aiclient.Factory, auth *Auth, log *logx.Logger, bill *BillingHooks, errlog *usage.ErrLogWorker) *Proxy {
	p := &Proxy{
		cfg: cfg, sched: sched, creds: creds, rec: rec, clients: clients, auth: auth,
		limit: newFixedWindowLimiter(cfg.GroupKeyRPM), log: log, bill: bill, errlog: errlog,
		wsHeartbeatInterval: responsesWSHeartbeatInterval,
	}
	// 注册表：每格式一 caller，New 时一次性构造（per-request 零分配）。
	// 新格式（Gemini/Grok/ollama 等）= 1 个 caller 文件 + 此处一行注册。
	// images 格式两端点调用器分列（上游子路径不同），map 占位 + handleFormat
	// 按请求路径覆盖为具体调用器。
	p.callers = map[domain.RequestFormat]UpstreamCaller{
		domain.FormatOpenAIChat:      &chatCaller{p: p},
		domain.FormatOpenAIResponses: &responsesCaller{p: p},
		domain.FormatAnthropic:       &anthropicCaller{p: p},
	}
	p.imageGenerations = &imagesCaller{p: p, path: "images/generations"}
	p.imageEdits = &imagesCaller{p: p, path: "images/edits"}
	p.callers[domain.FormatOpenAIImages] = p.imageGenerations
	// 协议转换路径（W5）：每方向一 convertedCaller（构造期一次性建好；
	// 热路径分支只读 map，off 组不触达）。
	p.convCallers = map[domain.ProtocolConvert]UpstreamCaller{
		domain.ProtocolConvertChatToResp: &convertedCaller{p: p, dir: domain.ProtocolConvertChatToResp},
		domain.ProtocolConvertMessToResp: &convertedCaller{p: p, dir: domain.ProtocolConvertMessToResp},
		domain.ProtocolConvertRespToMess: &convertedCaller{p: p, dir: domain.ProtocolConvertRespToMess},
		domain.ProtocolConvertChatToMess: &convertedCaller{p: p, dir: domain.ProtocolConvertChatToMess},
	}
	p.codexImagesGenerations = &codexImagesCaller{p: p}
	p.codexImagesEdits = &codexImagesCaller{p: p}
	// failover 骨架单例（D3）：attempt/sink 无状态（差异状态按值经 attemptState
	// 流入）——init 期一次性分配，per-request 零新增分配。
	p.chatAttempt = &chatAttempt{p: p}
	p.searchAttempt = &searchAttempt{p: p}
	p.wsAttempt = &wsAttempt{p: p}
	p.httpSink = &httpSink{}
	p.wsSink = &wsSink{}
	return p
}

// SetCodex 注入 codex SDK 适配层（T2 §3 装配点——main 构造
// sdkbridge.NewCodex(统一失效回调) 后注入；nil = 未装配 → codex 类型请求
// 501 显式拒绝）。
func (p *Proxy) SetCodex(c *sdkbridge.Codex) { p.codex = c }

func (p *Proxy) Inflight() int64 { return p.inflight.Load() }

// SetInstancesProvider 注入集群实例数 N 提供者（#14 多实例预算分摊；svc 构造
// 后调用——main 装配点，T3a 接线：px.SetInstancesProvider(svc)）。转发给
// auth（gate 预算 ceil(剩余/N)）与 limit（RPM ceil(rpm/N)）；N 变更
// （settings NOTIFY）后再次调用即触发预算即时重算（§3.4）。
func (p *Proxy) SetInstancesProvider(inst InstancesProvider) {
	p.auth.SetInstancesProvider(inst)
	p.limit.SetInstancesProvider(inst)
}

// finish 收尾：释放并发槽 + 额度扣减（后扣模型，usage 已知）+ 计费计算 +
// 记录用量（凡持有并发槽的路径必调）。无额度 key 无内存计数器 → 扣减
// no-op（恒 0）。落库路由统一走 routeLog（分表原则：不计费不入 usage_logs）。
func (p *Proxy) finish(accountID int64, l *domain.UsageLog) {
	p.sched.Release(accountID)
	if l != nil {
		p.applyBilling(l)
		p.auth.DeductQuota(l.KeyID, l.TotalTokens)
	}
	if p.cfg.UsageCapture && l != nil {
		p.routeLog(l)
	}
}

// applyBilling 计费计算（T2 中间态：只算不扣，扣费落库在 T3）：价格快照读 →
// billing.Cost 填 l.Cost/l.AboveHit。计费模型 = MappedModel ?? Model。价格缺失
// （预检后快照被删竞态）→ Warn + BillingTier="no_price" + cost 0（运行时
// 防御，审计留痕不按 0 计价；非兼容分支）。
func (p *Proxy) applyBilling(l *domain.UsageLog) {
	if p.bill == nil || p.bill.Prices == nil {
		return
	}
	// images 格式走 image 价计费（spec §4.2：Cost 总量含 image 分量；images
	// 请求只计 image 分量——文本 token 分量不做，chat 价路径对 images 恒不适用）。
	if l.Format == domain.FormatOpenAIImages {
		p.applyImageBilling(l)
		return
	}
	// search 格式走按单元价计费（spec 2026-08-13：search 独立按次计费——
	// codex-search 固定功能标识，GetFunctionPrice 表行价或默认 1000 毫分兜底；
	// **不查 GetPrice/GetImagePrice**——codex 模型多半无价，chat 价路径会落
	// no_price 静默 0 计费，恰是用户裁决要杜绝的）。
	if l.Format == domain.FormatOpenAISearch {
		if p.bill.FunctionPrices == nil {
			// 按单元价查找器未装配（评审 M-3）：Warn 防御性留痕——P2-1"杜绝静默
			// 0 计费"精神（生产装配正常不可达，仅防装配缺失静默掉价）。
			if p.log != nil {
				p.log.Warn("billing function price lookup unavailable", logx.String("model", domain.CodexSearchModel))
			}
			return
		}
		p.applyFunctionBilling(l)
		return
	}
	model := l.MappedModel
	if model == "" {
		model = l.Model
	}
	if model == "" {
		return // 未选号失败路径（无模型可计费；cost 恒 0）
	}
	pr, err := p.bill.Prices.GetPrice(model)
	if err != nil || pr == nil {
		if p.log != nil {
			p.log.Warn("billing price lookup failed", logx.String("model", model), logx.Error(err))
		}
		l.BillingTier = "no_price"
		return
	}
	// 价格快照（每 M token 毫分，1 USD = 100,000 毫分；pricing 同款单位）：
	// 请求时点生效基础单价直接读入日志——GetPrice 已取价，零额外查找零额外
	// DB 读。priority/flex 档位替换价与 above 分段价/倍率不展开（快照语义 =
	// 基础单价）。缓存价仅在有该分量（tokens > 0）且模型有价（非 nil）时
	// 落快照；否则保持 nil（NULL，无该分量语义）。
	l.PriceInputMillis = &pr.PromptPricePerMillion
	l.PriceOutputMillis = &pr.CompletionPricePerMillion
	if l.CacheReadTokens > 0 && pr.CacheReadPricePerMillion != nil {
		l.PriceCacheReadMillis = pr.CacheReadPricePerMillion
	}
	if l.CacheCreationTokens > 0 && pr.CacheCreationPricePerMillion != nil {
		l.PriceCacheCreationMillis = pr.CacheCreationPricePerMillion
	}
	l.Cost, l.AboveHit = billing.Cost(pr, billing.NormalizeTier(l.BillingTier), l.InputTokens, l.OutputTokens, l.CacheReadTokens, l.CacheCreationTokens)
	// resp/resp-ws 响应检测 image 计费旁路（spec §6；检测挂点产出 CallCount）：
	// count > 0 → 按 model 查 GetImagePrice——缺价 → no_price 标记整单不计费
	// （响应已产生无法 402，对齐上方 chat 缺价同语义：审计留痕不按 0 计价）；
	// 有价 → ImageCost 聚合进 Cost（仅 per-image 分量——responses 路径无
	// image_tokens，image token 恒 0）+ per-image 价快照落 PricePerCallMillis
	// （统一计费模型 spec 2026-08-13：call_count × price_per_call_millis → cost）。
	// P3-9：行存在但 per-image 价 nil → 图分量 0（出图免费，chat 分量照常——
	// ImageCost nil 分量回退 0 语义）。
	if l.CallCount > 0 {
		ip, err := p.bill.Prices.GetImagePrice(model)
		if err != nil || ip == nil {
			if p.log != nil {
				p.log.Warn("image price lookup failed", logx.String("model", model), logx.Error(err))
			}
			l.BillingTier = "no_price"
			l.Cost, l.AboveHit = 0, false // no_price 整单不计费（图像未计费不按 0 计价）
			return
		}
		if ip.OutputCostPerImageMilli != nil {
			l.PricePerCallMillis = ip.OutputCostPerImageMilli
		}
		l.Cost += billing.ImageCost(ip, 0, 0, l.CallCount) // image token 恒 0（responses 路径无 image_tokens，V1-V3 实证）
	}
	// 价格倍率（T3.5，用户拍板）：整单 × 有效倍率（万分数；用户覆盖组——
	// 用户已设置 → 用户值，否则组倍率，均缺 ×1）。m==10000 恒等短路零开销；
	// m==0 免费（cost 0，仍记日志不扣费）。倍率不改变 aboveHit 语义。
	p.applyMultiplierLog(l, l.Cost) // l.Cost 已含 chat 分量累计（倍率前原始成本）
}

// applyMultiplierLog 倍率施加单点（spec 2026-08-18 raw_cost 列）：raw 显式传
// 参——三处倍率参数形态不同，读 l.Cost 作 raw 在 search 路径（该时刻 l.Cost
// 恒 0——buildLog 不设 Cost）恒失真（gate Major 1 修订）。cost 语义零变化
// （applyMultiplier 纯函数不动）。热路径一次 int64 赋值零额外开销。
func (p *Proxy) applyMultiplierLog(l *domain.UsageLog, raw int64) {
	l.RawCost = raw
	l.Cost = applyMultiplier(raw, p.bill.Balances.EffectiveMultiplier(l.UserID, l.GroupID))
}

// applyImageBilling 生图计费（images 格式专用——只计 image 分量）：价格快照
// 读（GetImagePrice，零 DB）→ ImageCost 填 l.Cost。计费模型 = MappedModel ??
// Model（与 chat 同语义）。价格缺失（预检后快照被删竞态）→ Warn +
// BillingTier="no_price" + cost 0（运行时防御，审计留痕不按 0 计价）。价格
// 快照列仅在分量 >0 且模型有价（非 nil）时落（nil = 无该分量语义）；倍率
// 同 chat 路径整单施加（fast/组倍率——ImageCost 本身不含倍率）。
// 统一计费模型（spec 2026-08-13）落账口径：image token 并入 Input/Output
// Tokens（images 格式 text 分量恒 0 → 两列即 image tokens——ImageCost 口径不
// 变）；per-image 价快照即 PricePerCallMillis（毫分/张）。
func (p *Proxy) applyImageBilling(l *domain.UsageLog) {
	model := l.MappedModel
	if model == "" {
		model = l.Model
	}
	if model == "" {
		return // 未选号失败路径（无模型可计费；cost 恒 0）
	}
	ip, err := p.bill.Prices.GetImagePrice(model)
	if err != nil || ip == nil {
		if p.log != nil {
			p.log.Warn("billing image price lookup failed", logx.String("model", model), logx.Error(err))
		}
		l.BillingTier = "no_price"
		return
	}
	if l.CallCount > 0 && ip.OutputCostPerImageMilli != nil {
		l.PricePerCallMillis = ip.OutputCostPerImageMilli
	}
	l.Cost = billing.ImageCost(ip, l.InputTokens, l.OutputTokens, l.CallCount)
	p.applyMultiplierLog(l, l.Cost) // :291 已赋值 ImageCost——raw 取倍率前累计
}

// applyFunctionBilling 按单元计费（openai-search 格式专用——只计按次分量）：
// GetFunctionPrice("codex-search") 快照读（零 DB）→ PricePerCallMillis 按次价
// 快照（仅 call_count > 0 落——对齐 images 先例，错误行不落按次价快照，评审
// M-2）+ Cost = 按次价 × call_count（search 成功恒 1——无 token 分量，响应
// 无 usage）。计费模型 = codex-search 固定标识（不查 GetPrice/GetImagePrice
// ——codex 模型多半无价）。价格缺失（GetFunctionPrice 对 codex-search 有默认
// 行兜底，正常不可达）→ Warn + no_price + cost 0（运行时防御，审计留痕不按
// 0 计价）。倍率同 chat 路径整单施加。
func (p *Proxy) applyFunctionBilling(l *domain.UsageLog) {
	fp, err := p.bill.FunctionPrices.GetFunctionPrice(domain.CodexSearchModel)
	if err != nil || fp == nil || fp.PricePerCall == nil {
		if p.log != nil {
			p.log.Warn("billing function price lookup failed", logx.String("model", domain.CodexSearchModel), logx.Error(err))
		}
		l.BillingTier = "no_price"
		return
	}
	if l.CallCount > 0 {
		l.PricePerCallMillis = fp.PricePerCall
	}
	// raw = 显式表达式（gate Major 1）：该时刻 l.Cost 恒 0，读字段作 raw 会
	// 静默丢 search 全量原始成本（测试红绿断言兜底）。
	p.applyMultiplierLog(l, *fp.PricePerCall*l.CallCount)
}

// buildLog 组装 UsageLog（record 与 finish 共用）。语义（评审 I-1 确认）：
// Model = 客户端请求模型（reqModel），MappedModel = 映射后实际模型
// （usedModel 与请求模型不同才写入，否则空 = 未映射）。u 传值（GC 削减 P6：
// 指针逃逸 1 alloc；零值 = 无用量）。
// 统一计费模型（spec 2026-08-13）：image token 分量（ii/io，images 格式专用，
// resp 路径恒 0）并入 Input/OutputTokens（TotalTokens 口径不变）；功能调用
// 计数（calls = 图片张数：resp 检测旁路计数 Task D 先行 / images 格式直连与
// codex 路径 data 数组长 / 流式 completed 事件数）落 CallCount（不入 TotalTokens）。
func (p *Proxy) buildLog(reqID string, groupID, accountID int64, reqModel, usedModel string, format domain.RequestFormat, status int, et domain.ErrorType, u usageTuple, start time.Time) *domain.UsageLog {
	return &domain.UsageLog{
		RequestID: reqID, GroupID: groupID, AccountID: accountID,
		Model: reqModel, MappedModel: mappedFor(reqModel, usedModel), Format: format, StatusCode: status, ErrorType: et,
		LatencyMS:   time.Since(start).Milliseconds(),
		InputTokens: u.it + u.ii, OutputTokens: u.ot + u.io, TotalTokens: u.tt,
		CacheReadTokens: u.cr, CacheCreationTokens: u.cc,
		CallCount: u.calls,
		CreatedAt: time.Now(),
	}
}

// mappedFor 判定映射关系：实际使用的模型（used）非空且与请求模型（req）不同
// → 返回映射后模型；无映射/失败路径（used 为空或与请求相同）→ 空。精确比较
// 足够——ModelMapping 匹配语义即大小写敏感等值（selection.go）。
func mappedFor(req, used string) string {
	if used != "" && used != req {
		return used
	}
	return ""
}

// record 记录一条用量日志（无并发槽的失败路径；有槽路径走 finish）。
// ctx 提供鉴权 KeyMeta（user_id/key_id 归属；401 等鉴权失败路径无 KeyMeta）。
// 落库路由与 finish 同走 routeLog 单写点。**本地
// 预用量拒绝（429/402 等，见 recordRejected）不在此路径**——无用量可记的
// 拒绝不产生明细，避免拒绝风暴打爆 pending。
func (p *Proxy) record(ctx context.Context, reqID string, groupID, accountID int64, reqModel, usedModel string, format domain.RequestFormat, status int, et domain.ErrorType, latencyMS int64, u usageTuple, start time.Time) {
	p.recordLog(logWithCtx(ctx, p.buildLog(reqID, groupID, accountID, reqModel, usedModel, format, status, et, u, start)))
}

// recordRejected 记录一条**本地预用量拒绝**（401 鉴权失败/额度耗尽/并发超限/
// 余额 402/tier reject/缺价/无账号：请求未接触上游、未消费任何 token、cost 恒
// 0）。双轨（用户裁决分表设计）：①统计聚合（usagestat 请求/错误计数语义不变）
// ②错误明细投递 errlog worker 落 err_logs——**不产生 usage_logs 明细**、不进
// billed/非 billed pending。拒绝风暴（P2a 压测 2026-08-11：单 key 限流
// 161k req/s → 60s 冲至 9.8M pending 行 / RSS 7.5GB，usage_logs 表 120.7M→
// 144.5M 行膨胀）每请求一条 usage_logs 明细即无界积压与写放大源头；err_logs
// 为独立瘦表 + 有界队列背压（队列满丢弃采样），风暴不淹没 DB 不爆内存——
// 审计明细补回但不回到 usage_logs 主链路。msg 为拒绝文案（error_message 审计
// 字段，域内截断 500）。
func (p *Proxy) recordRejected(ctx context.Context, reqID string, groupID, accountID int64, reqModel, usedModel string, format domain.RequestFormat, status int, et domain.ErrorType, latencyMS int64, u usageTuple, start time.Time, msg string) {
	if !p.cfg.UsageCapture {
		return
	}
	l := logWithCtx(ctx, p.buildLog(reqID, groupID, accountID, reqModel, usedModel, format, status, et, u, start))
	if msg != "" {
		m := domain.TruncateErrMsg(msg)
		l.ErrorMessage = &m
	}
	p.enqueueRejectedErr(l) // 明细（err_logs 普通队列：风暴采样丢弃面）
}

// enqueueRejectedErr 拒绝行投递（架构审查 B2：拒绝类行走普通队列——风暴采样
// 丢弃；与双轨行豁免通道分离）。nil worker（未装配）→ no-op。
func (p *Proxy) enqueueRejectedErr(l *domain.UsageLog) {
	if p.errlog != nil {
		p.errlog.EnqueueRejected(l)
	}
}

// recordLog 用量落库路由入口（record 无并发槽失败路径与 failover 耗尽路径共用；
// 有槽路径 finish 直调 routeLog）。调用方须已填 ErrorMessage（错误文本落盘；
// 成功路径 nil 恒空）。
func (p *Proxy) recordLog(l *domain.UsageLog) {
	if !p.cfg.UsageCapture {
		return
	}
	p.routeLog(l)
}

// routeLog 分表统一路由（用户裁决修正 2026-08-11：usage_logs 成员资格按**放行
// 路径语义（error_type）**判定，与 cost 无关——cost>0 判定会漏掉免费分组
// （倍率 0 的成功行）与 0 token 成功行（空响应））：
//   - usage_logs = 放行路径明细：error_type ∈ {none（成功，含 cost=0 免费组/
//     空响应）, abort（半异常计费）}——F2 单写点（spec §一）：billable 行一律
//     经 rec.Record 入队，入队前盖 Billed 出生标记；扣费由 billing worker 从
//     账本游标消费（T3）。4xx/5xx/network（上游透传/耗尽失败行）不写
//     usage_logs（失败明细归 err_logs，P2a 拒绝风暴教训同族）
//   - err_logs = 全部错误明细（error_type != none）：4xx/5xx（上游透传/耗尽）
//   - abort 双轨（豁免队列恒落盘——架构审查 B2）；拒绝行走 recordRejected
//     的采样队列，不经本路由
//   - usage_stats = 离线聚合（spec 2026-08-14）：请求路径零统计计算/投递——
//     放行行（none/abort）由离线 worker 从 usage_logs 重建（全字段含 TTFT/
//     call_count）；纯错误行（4xx/5xx/network）从 err_logs 重建（count 语义）；
//     拒绝行随 err_logs 采样丢样（口径注释见 recordRejected）
func (p *Proxy) routeLog(l *domain.UsageLog) {
	if l.ErrorType == domain.ErrNone || l.ErrorType == domain.ErrAbort { // 放行路径
		// F2 出生标记盖章（spec §一）：Billed = !(计费捕获开 && 有用户归属)。
		// true = 出生即结算吸收态（计费关闭/匿名行本就不扣，游标零消费顺带
		// 省一次循环）；false = 待对账，billing worker 游标消费（T3）。
		l.Billed = !(p.cfg.BillingCapture && l.UserID > 0)
		p.rec.Record(l) // 唯一持久化入口（usage_logs 落库 + quota 累加）
	}
	if l.ErrorType != domain.ErrNone {
		p.enqueueErrLog(l) // 全部错误明细 → err_logs（豁免通道）
	}
}

// enqueueErrLog 错误明细投递（架构审查 B2：上游错误/双轨行走豁免队列——不参与
// 拒绝风暴采样丢弃，恒落盘；与拒绝行采样通道分离）。nil worker（未装配）→
// no-op。仅错误行调用（成功路径零开销）。
func (p *Proxy) enqueueErrLog(l *domain.UsageLog) {
	if p.errlog != nil {
		p.errlog.EnqueueError(l)
	}
}

// ctxKeyReqMeta 是请求元数据的 context 键（handleFormat 写入；日志归属读取）。
// 单键单值（GC 削减 P6：原 meta/tier 两次 WithValue+WithContext 合并为一次）：
// 携带鉴权 KeyMeta 与归一化 service_tier；hasTier 保持非计费路径 BillingTier
// 空语义（计费全关不写入 hasTier → 日志 BillingTier 恒空）。
type ctxKeyReqMeta struct{}

// ctxKeyTTFT 是 TTFT 采集的 context 键（caller 流式首 chunk 写入；日志读取——
// 先例 ctxKeyReqMeta）。值 *int64（首 token 时间毫秒）；仅流式首帧到达后写入，
// 非流式/失败/无首 token 路径无值 → 日志 TTFTMS 恒 nil（NULL 落库）。
type ctxKeyTTFT struct{}

type reqMeta struct {
	meta     domain.KeyMeta
	tier     billing.Tier
	hasTier  bool
	clientIP string // 客户端 IP（guardPipeline 入口鉴权前提取；401 及全部拒绝路径带）
}

// logWithCtx 从 ctx 读请求元数据填日志归属（user_id/key_id；context 传递
// ——不改变 Call/buildLog 签名；无 KeyMeta 的路径保持 0）+ 客户端 IP
// （client_ip——guardPipeline 入口提取，全部日志路径恒带）+ service_tier
// 归一化值填 BillingTier（计费启用路径；无 tier 的路径保持空）+ TTFT（流式
// 首 chunk 采集；非流式/失败路径无值 → nil）。rm 为指针（handleFormat 原地补
// tier，单次 context 写入；全程请求 goroutine 内同步访问）。
func logWithCtx(ctx context.Context, l *domain.UsageLog) *domain.UsageLog {
	if rm, ok := ctx.Value(ctxKeyReqMeta{}).(*reqMeta); ok {
		l.UserID = rm.meta.UserID
		l.KeyID = rm.meta.KeyID
		l.ClientIP = rm.clientIP
		if rm.hasTier {
			l.BillingTier = rm.tier.String()
		}
	}
	if ttft, ok := ctx.Value(ctxKeyTTFT{}).(*int64); ok {
		l.TTFTMS = ttft
	}
	return l
}

type formatError struct {
	status int
	msg    string
}

func (e *formatError) Error() string { return e.msg }

var (
	errInvalidKey     = &formatError{status: http.StatusUnauthorized, msg: "invalid gateway key"}
	errTooMany        = &formatError{status: http.StatusTooManyRequests, msg: "no available account"}
	errConcurrency    = &formatError{status: http.StatusTooManyRequests, msg: "concurrency limit exceeded"}
	errQuotaExhausted = &formatError{status: http.StatusTooManyRequests, msg: "key quota exhausted"}
	errRateLimit      = &formatError{status: http.StatusTooManyRequests, msg: "group rate limited"}
	errBody           = &formatError{status: http.StatusRequestEntityTooLarge, msg: "request body too large"}
	// errUpgradeRequired 400：resp-ws 端点收到非升级请求（本地拒绝，无记录）。
	errUpgradeRequired = &formatError{status: http.StatusBadRequest, msg: "websocket upgrade required"}
	// errGroupNotFound 404：组不存在/快照未加载（GET /v1/models——鉴权已过但
	// 组失效；对齐 Select 的 ErrGroupNotFound 语义）。
	errGroupNotFound = &formatError{status: http.StatusNotFound, msg: "group not found"}
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// encodedError 预编码错误响应（静态错误体 init 时编码一次，热路径零反射）。
type encodedError struct {
	status int
	body   []byte
}

// errBodies 静态本地拒绝错误的预编码响应体（与 writeJSON 逐字节同构：
// json.NewEncoder(map) + 尾随换行）。动态 body 的 4xx 透传与 handleSelectError
// 的临时 formatError 仍走 writeJSON 反射路径（错误路径，非热路径）。
var errBodies = func() map[*formatError]encodedError {
	enc := func(e *formatError) encodedError {
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(map[string]any{"error": map[string]any{"message": e.msg, "type": "gateway_error"}})
		return encodedError{status: e.status, body: buf.Bytes()}
	}
	return map[*formatError]encodedError{
		errInvalidKey:          enc(errInvalidKey),
		errTooMany:             enc(errTooMany),
		errConcurrency:         enc(errConcurrency),
		errQuotaExhausted:      enc(errQuotaExhausted),
		errRateLimit:           enc(errRateLimit),
		errBody:                enc(errBody),
		errNoPrice:             enc(errNoPrice),
		errInsufficientBalance: enc(errInsufficientBalance),
		errServiceTierRejected: enc(errServiceTierRejected),
		errUpgradeRequired:     enc(errUpgradeRequired),
		errGroupNotFound:       enc(errGroupNotFound),
	}
}()

func writeErr(w http.ResponseWriter, e *formatError) {
	if pe, ok := errBodies[e]; ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(pe.status)
		_, _ = w.Write(pe.body)
		return
	}
	writeJSON(w, e.status, map[string]any{"error": map[string]any{"message": e.msg, "type": "gateway_error"}})
}

// --- 辅助 ---

type usageTuple struct {
	it, ot, tt int64
	cr, cc     int64 // 缓存读取/写入 token（缺失 = 0）
	// 功能调用计数（统一计费模型 spec 2026-08-13；当前唯一生产者 = 图片张数：
	// resp 检测旁路 spec §6 / images 格式直连与 codex 路径 data 长 / 流式
	// completed 事件数；search 端点接入后 = 1）。落 CallCount，不入 TotalTokens。
	calls int64
	// 图片生成分量（images 格式）：ii/io = image token 分量
	// （input/output_tokens_details.image_tokens）；text token 分量恒 0——
	// images 请求只计 image 分量。tt 含 image tokens 不含张数（评审 P3-6
	// quota 口径：张数不入 TotalTokens）。resp 检测路径 ii/io 恒 0（V1-V3
	// 实证 responses 路径无 image_tokens）。ii/io 由 buildLog 并入 in/out。
	ii, io int64
}

// recordStreamAbort 上游流中止记录（客户端断开/上游停滞统一入口）：先已收
// 到的 usage 帧照常计费。u 为 Observer 已累积的用量元组（评审 M-2：此前传
// nil → tokens 全 0 → 中止路径消费不扣费；buildLog 填 l.Cost 由 finish 的
// applyBilling 承担）。groupID 由各 caller 作用域传入（评审 M-1：此前硬编码
// 0 → 中止路径组倍率查找恒 miss → 组倍率 ≠10000 时计费与正常路径不一致）。
func (p *Proxy) recordStreamAbort(ctx context.Context, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, u usageTuple, err error) {
	if p.log != nil {
		p.log.Warn("upstream stream aborted", logx.String("request_id", reqID), logx.Error(err))
	}
	p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, sel.Format, 200, domain.ErrAbort, u, start)))
}

func (p *Proxy) handleSelectError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, scheduler.ErrNoAvailable):
		w.Header().Set("Retry-After", "1")
		writeErr(w, errTooMany)
	default:
		writeErr(w, &formatError{status: statusFor(err), msg: selectErrorMessage(err)})
	}
}

// selectErrorMessage 选号失败的错误文案（HTTP 响应与 WS 错误帧共用；statusFor
// 同款语义：格式不可用/组不存在 → 404，无可用 → 429）。
func selectErrorMessage(err error) string {
	switch {
	case errors.Is(err, scheduler.ErrFormatUnavailable):
		return "no account supports this request format"
	case errors.Is(err, scheduler.ErrNoAvailable):
		return "no available account"
	default:
		return "group not found"
	}
}

// statusClientClosedRequest 客户端在首字节前断开（nginx "client closed request"
// 约定；499 非标准码）：SDK 请求阶段（上游响应前）断连的 usage 记录状态码。
// 客户端已断——不写 HTTP 响应，只进日志（error_type=abort；tokens 必然 0 →
// cost=0 不计费）。分类正确性修复（#20 E 项）：不得按连接级网络错误处理
// （不 failover、不 MarkResult、不冷却）。
const statusClientClosedRequest = 499

func statusFor(err error) int {
	switch {
	case errors.Is(err, scheduler.ErrFormatUnavailable), errors.Is(err, scheduler.ErrGroupNotFound):
		return http.StatusNotFound
	default:
		return http.StatusTooManyRequests
	}
}

// statusOf 提取上游错误的状态码：优先 StatusCode() 方法；openai-go/anthropic
// v1.x 的 apierror.Error 用 StatusCode 字段暴露，退回反射读取。反射仅在错误
// 路径（罕见）执行，不占热路径。
func statusOf(err error) int {
	type statusCoder interface{ StatusCode() int }
	var sc statusCoder
	if errors.As(err, &sc) {
		return sc.StatusCode()
	}
	v := reflect.ValueOf(err)
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && !v.IsNil() {
		v = v.Elem()
	}
	if v.IsValid() && v.Kind() == reflect.Struct {
		if f := v.FieldByName("StatusCode"); f.IsValid() && f.Kind() == reflect.Int {
			return int(f.Int())
		}
	}
	return 0 // 连接级/超时错误
}

// upstreamBody 提取上游错误响应的原始 body：openai.Error / anthropic.Error 的
// RawJSON() 即收到的未修改 JSON 原文（apierror.Error.JSON.raw），4xx 透传用。
// 连接级/超时错误无 body，返回 nil。
func upstreamBody(err error) []byte {
	type rawJSONer interface{ RawJSON() string }
	var rj rawJSONer
	if errors.As(err, &rj) {
		if s := rj.RawJSON(); s != "" {
			return []byte(s)
		}
	}
	return nil
}

// upstreamErrMsg 提取上游错误 body 的 message（规则 when.error_message_contains
// 匹配用）：OpenAI/Anthropic 错误格式均为 {"error":{"message":...}}。
func upstreamErrMsg(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	s := gjson.GetBytes(body, "error.message").String()
	if s == "" {
		s = gjson.GetBytes(body, "message").String()
	}
	return s
}

// readUpstreamBody 读取并关闭非 200 响应的 body（4xx/5xx 透传用）。
func readUpstreamBody(resp *http.Response) []byte {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	return b
}

// errBodyNotObject 顶层非对象体（仅恶意/畸形输入可达——上游已过 json.Valid
// 硬门与 gjson 类型门，标量/数组根体才落到改写路径）。map 版由 json.Unmarshal
// 报同类错；sjson 对顶层标量整体替换为 {}（不报错）——isJSONObjectRoot 守卫
// 保持旧错误语义（旧版 null 体还会 nil map 赋值 panic，一并修复）。
var errBodyNotObject = errors.New("body must be a JSON object")

// isJSONObjectRoot O(1) 顶层形状检查（首非空白字节）：sjson 对顶层标量整体
// 替换为 {}（非报错），与 map 版 Unmarshal 报错语义分歧；数组根一并按非对象
// 体拒绝（sjson 文案不同，状态同为 400），保持旧错误面。
func isJSONObjectRoot(body []byte) bool {
	for _, b := range body {
		if b > ' ' {
			return b == '{'
		}
	}
	return false // 空/全空白体：json.Valid 已拒绝，防御性 false
}

// setModel 把原始请求体里的 "model" 字段改写为调度器选定的上游模型名
// （ModelMapping 已应用，见 scheduler.Select 的 Selection.Model）。原始转发
// 必须沿用 SDK 路径 params.Model = sel.Model 的改写语义，否则映射配置在
// 流式请求上失效（Task 3 迁移发现）。
// 短路守卫（GC 削减 P1）：model 已是目标值（gjson 字符串读取）→ 返回原切片
// 零分配。守卫只对字符串匹配生效；null/数字/缺失/需改写走 sjson 字节级改写
// 路径（单字段 splice，非 map 全文档往返——>2^53 整数精度无损、键序不变、
// 无 HTML 转义，与 WS 面 relayWS 首帧改写同库同风格；sjson 对缺失路径默认
// 补字段，与 map 版本一致。等价性核对见分析报告：sel.Model 非空时 null/数字
// 恒走改写，行为不变）。
func setModel(body []byte, model string) ([]byte, error) {
	if gjson.GetBytes(body, "model").String() == model {
		return body, nil
	}
	if !isJSONObjectRoot(body) {
		return nil, errBodyNotObject
	}
	return sjson.SetBytes(body, "model", model)
}

// setStreamAndModel 字节级改写 stream 与 model（GC 削减 P1b）：两字段各自
// 短路守卫，无需改写的字段保持原字节；任一篇需改写才做 sjson 改写（单字段
// splice，精度/键序/转义保真同 setModel）。sjson 单次调用仅支持单路径，故为
// 两次 SetBytes 调用（stream、model 顶层路径不相交，次序无关，最终字节一致）；
// sjson 对缺失路径默认加字段（stream/model 缺失时行为 = map 版本补字段，一
// 致）。重复键边界：sjson 只改首个出现（map 版本为 last-wins 去重）——上游
// JSON 合法重复键实际不可达（RFC 8259 SHOULD 唯一，真实 SDK 客户端不产生）。
func setStreamAndModel(body []byte, stream bool, model string) ([]byte, error) {
	sc := gjson.GetBytes(body, "stream")
	streamOK := (stream && sc.Type == gjson.True) || (!stream && sc.Type == gjson.False)
	if streamOK && gjson.GetBytes(body, "model").String() == model {
		return body, nil
	}
	if !isJSONObjectRoot(body) {
		return nil, errBodyNotObject
	}
	var err error
	if body, err = sjson.SetBytes(body, "stream", stream); err != nil {
		return nil, err
	}
	return sjson.SetBytes(body, "model", model)
}

// credentialFor 从 Selection 取当前凭据值（注册表分发；api_key 类型直读静态 Key）。
// 未知类型（未来号池类型未注册）→ 显式错误，不静默 fallback（评审 M1：
// fallback 到 api_key 是号池类型安全隐患）。
func (p *Proxy) credentialFor(ctx context.Context, sel *scheduler.Selection) (string, error) {
	if !sel.CredentialType.Valid() {
		return "", fmt.Errorf("unsupported credential type %q", sel.CredentialType)
	}
	return p.creds.For(sel.CredentialType).Credential(ctx, credential.CredentialInput{
		AccountID: sel.AccountID, Type: sel.CredentialType, APIKey: sel.UpstreamKey,
	})
}

// tplOf 从 Selection 构造轻量模板对象（仅用于 aiclient 取 SDK 客户端）。
// base_url 变更生效链路（main.go 组合注入）：管理端变更 → service 的 invalidate
// 回调 → 调度器 InvalidateAll 重载快照（新 base_url 随 Selection 下发）+ aiclient
// Factory.InvalidateAll 丢弃按旧 base_url 构建的 SDK 客户端（懒构建，下次使用重建）。
func tplOf(sel *scheduler.Selection) *domain.Template {
	return &domain.Template{ID: sel.TemplateID, BaseURL: sel.BaseURL}
}

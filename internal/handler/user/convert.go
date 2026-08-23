// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"math"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

// redemptionValueToAPI 毫分存储 → 契约面值（与 /admin 兑换码同规则）：
// balance/temp_balance → USD（1 USD = 100,000 毫分）；concurrency → 并发数
// float64 直出（非货币，仅类型转换——整数精确）。
func redemptionValueToAPI(typ domain.RedemptionType, millis int64) float64 {
	if typ == domain.RedemptionTypeConcurrency {
		return float64(millis)
	}
	return float64(millis) / 1e5
}

// toAPIUser 用户领域对象 → 契约类型（口令散列永不下发；Balance 毫分 → USD
// 展示换算——1 USD = 100,000 毫分，与 /admin 用户端点同语义；价格倍率按组
// 挂载，User 无倍率字段）。
func toAPIUser(u *domain.User) User {
	r := UserRole(u.Role)
	st := UserStatus(u.Status)
	return User{
		ID:             &u.ID,
		Email:          &u.Email,
		Role:           &r,
		Status:         &st,
		MaxConcurrency: &u.MaxConcurrency,
		Balance:        ptr(float64(u.Balance) / 1e5),
		CreatedAt:      &u.CreatedAt,
		UpdatedAt:      &u.UpdatedAt,
	}
}

// toAPIGroup 组领域对象 → 契约类型（/api/user/groups 只读列表；PriceMultiplier
// 万分数 → 正常值 float64，API 边界换算）。
func toAPIGroup(g *domain.Group) Group {
	v := GroupVisibility(g.Visibility)
	return Group{
		ID:              &g.ID,
		Name:            &g.Name,
		Visibility:      &v,
		PriceMultiplier: ptr(float64(g.PriceMultiplier) / 10000.0),
		CreatedAt:       &g.CreatedAt,
		UpdatedAt:       &g.UpdatedAt,
		DeletedAt:       g.DeletedAt, // 软删除时间戳（只读字段；已删组不进可选列表）
	}
}

// toAPIKey key 领域对象 → 契约类型（明文长期回显——列表/详情/创建/轮换）。
func toAPIKey(k *domain.Key) Key {
	st := KeyStatus(k.Status)
	return Key{
		ID:             &k.ID,
		UserID:         &k.UserID,
		GroupID:        &k.GroupID,
		Name:           &k.Name,
		Key:            &k.KeyRaw,
		Status:         &st,
		MaxConcurrency: &k.MaxConcurrency,
		Quota:          &k.Quota,
		QuotaUsed:      &k.QuotaUsed,
		CreatedAt:      &k.CreatedAt,
		UpdatedAt:      &k.UpdatedAt,
		DeletedAt:      k.DeletedAt, // 软删除时间戳（只读字段；已删 key 不进列表，GET 单个可查）
	}
}

// toAPIUsageLog 用量日志领域对象 → 用户面契约类型（/api/user/usage_logs；
// UserUsageLog 无 AccountID/TemplateID——用户无上游账号拓扑概念；err_logs
// 分表后无 status_code/error_message 字段）。
func toAPIUsageLog(l *domain.UsageLog) UserUsageLog {
	f := RequestFormat(l.Format)
	et := ErrorType(l.ErrorType)
	return UserUsageLog{
		ID:                       &l.ID,
		RequestID:                &l.RequestID,
		ClientIP:                 &l.ClientIP,
		GroupID:                  &l.GroupID,
		UserID:                   &l.UserID,
		KeyID:                    &l.KeyID,
		Model:                    &l.Model,
		MappedModel:              &l.MappedModel,
		Format:                   &f,
		ErrorType:                &et,
		LatencyMS:                &l.LatencyMS,
		TTFTMS:                   l.TTFTMS,
		InputTokens:              &l.InputTokens,
		PriceInputMillis:         l.PriceInputMillis,
		OutputTokens:             &l.OutputTokens,
		PriceOutputMillis:        l.PriceOutputMillis,
		TotalTokens:              &l.TotalTokens,
		CacheReadTokens:          &l.CacheReadTokens,
		PriceCacheReadMillis:     l.PriceCacheReadMillis,
		CacheCreationTokens:      &l.CacheCreationTokens,
		PriceCacheCreationMillis: l.PriceCacheCreationMillis,
		Cost:                     &l.Cost,
		RawCost:                  &l.RawCost,
		BillingTier:              &l.BillingTier,
		AboveHit:                 &l.AboveHit,
		Overdraft:                &l.Overdraft,
		CreatedAt:                &l.CreatedAt,
	}
}

// toAPIErrLog 错误明细领域对象 → 用户面契约类型（/api/user/err_logs；
// UserErrLog 无 AccountID/TemplateID——用户无上游账号拓扑概念；BillingTier
// 空 = 未计费路径 → null）。行级脱敏（规则引擎已注入时）：平台问题行
// error_message 按同一策略替换固定文案——管理面恒原文不动（管理员排障）。
func (h *UserAPI) toAPIErrLog(l *domain.UsageLog) UserErrLog {
	f := RequestFormat(l.Format)
	et := ErrorType(l.ErrorType)
	e := UserErrLog{
		ID:           &l.ID,
		RequestID:    &l.RequestID,
		ClientIP:     &l.ClientIP,
		GroupID:      &l.GroupID,
		UserID:       &l.UserID,
		KeyID:        &l.KeyID,
		Model:        &l.Model,
		Format:       &f,
		StatusCode:   &l.StatusCode,
		ErrorType:    &et,
		ErrorMessage: l.ErrorMessage,
		LatencyMS:    &l.LatencyMS,
		CreatedAt:    &l.CreatedAt,
	}
	if h.rules != nil {
		if msg, ok := h.sanitizeErrLog(l); ok {
			e.ErrorMessage = &msg
		}
	}
	if l.BillingTier != "" {
		e.BillingTier = &l.BillingTier
	}
	return e
}

// upstreamRejectedMsg 归一固定文案（与 httpSink.writeUpstreamRejection 空
// body 分支 / wsSink 错误帧同文案——响应归一与日志脱敏一处定义）。
const upstreamRejectedMsg = "upstream rejected request"

// errTypeKind error_type → 规则事件类别（行级脱敏全函数映射）：Err4xx→4xx、
// Err429→429、Err5xx→5xx、ErrNetwork→network、ErrAbort→5xx（保守——半异常
// 行可能含上游文本）；其余 error_type（ErrAuth/ErrBilling/ErrNoAccount/
// ErrNone 等本地拒绝行）一律无 kind——不参与策略匹配，原样返回（本地拒绝行
// message 恒网关文案 "invalid gateway key"/"no available account" 等，无泄漏面）。
func errTypeKind(et domain.ErrorType) (rule.Kind, bool) {
	switch et {
	case domain.Err4xx:
		return rule.Kind4xx, true
	case domain.Err429:
		return rule.Kind429, true
	case domain.Err5xx:
		return rule.Kind5xx, true
	case domain.ErrNetwork:
		return rule.KindNetwork, true
	case domain.ErrAbort:
		return rule.Kind5xx, true
	}
	return 0, false
}

// sanitizeErrLog 用户面 err_logs 行级脱敏：行 {kind ← error_type 全函数映射、http_status ← status_code、message ← error_message} 调同一策略（Classify）→ 统一公式 msg=CustomMessage!=nil?*CustomMessage:orig（via rule.UnifiedMessage，与 pipeline 响应同源 I-3）；代理日志保留原文边界另述。Model 口径 = 最终请求模型（映射后 sel.Model，与 failoverLoop 一致）。返回 (替换后文本, 是否替换)。
func (h *UserAPI) sanitizeErrLog(l *domain.UsageLog) (string, bool) {
	k, ok := errTypeKind(l.ErrorType)
	if !ok {
		return "", false
	}
	model := l.MappedModel
	if model == "" {
		model = l.Model
	}
	ev := rule.Event{
		AccountID: l.AccountID, TemplateID: l.TemplateID,
		Model: model, Kind: k,
	}
	if l.GroupID > 0 {
		ev.GroupID = &l.GroupID
	}
	if l.StatusCode > 0 {
		ev.HTTPStatus = &l.StatusCode
	}
	if l.ErrorMessage != nil {
		ev.ErrorMessage = *l.ErrorMessage
	}
	then, _ := h.rules.Classify(ev)
	// 与 pipeline 同源（I-3 helper 单点）
	upstream := ""
	if l.ErrorMessage != nil {
		upstream = *l.ErrorMessage
	}
	return rule.UnifiedMessage(then, upstream)
}

func toAPIStatTrendPoint(b *domain.StatBucket) StatTrendPoint {
	var avg float64
	if b.TTFTCount > 0 {
		avg = math.Round(float64(b.TTFTTotalMS) / float64(b.TTFTCount))
	}
	return StatTrendPoint{
		BucketTime:          &b.BucketTime,
		RequestCount:        &b.RequestCount,
		ErrorCount:          &b.ErrorCount,
		CallCount:           &b.CallCount,
		InputTokens:         &b.InputTokens,
		OutputTokens:        &b.OutputTokens,
		TotalTokens:         &b.TotalTokens,
		CacheReadTokens:     &b.CacheReadTokens,
		CacheCreationTokens: &b.CacheCreationTokens,
		Cost:                ptr(float64(b.Cost) / 1e5),
		RawCost:             ptr(float64(b.RawCost) / 1e5),
		TTFTAvgMS:           ptr(avg),
		TTFTMaxMS:           &b.TTFTMaxMS,
	}
}

func toAPIEntityStatTrendPoint(b *domain.EntityStatBucket) StatTrendPoint {
	var avg float64
	if b.TTFTCount > 0 {
		avg = math.Round(float64(b.TTFTTotalMS) / float64(b.TTFTCount))
	}
	return StatTrendPoint{
		BucketTime:          &b.BucketTime,
		RequestCount:        &b.RequestCount,
		ErrorCount:          &b.ErrorCount,
		CallCount:           &b.CallCount,
		InputTokens:         &b.InputTokens,
		OutputTokens:        &b.OutputTokens,
		TotalTokens:         &b.TotalTokens,
		CacheReadTokens:     &b.CacheReadTokens,
		CacheCreationTokens: &b.CacheCreationTokens,
		Cost:                ptr(float64(b.Cost) / 1e5),
		RawCost:             ptr(float64(b.RawCost) / 1e5),
		TTFTAvgMS:           ptr(avg),
		TTFTMaxMS:           &b.TTFTMaxMS,
	}
}

func toAPIStatTTFTSummary(s *domain.TTFTSummary) StatTTFTSummary {
	if s == nil {
		s = &domain.TTFTSummary{Source: "sketch"}
	}
	src := StatTTFTSummarySource(s.Source)
	return StatTTFTSummary{
		Count:  s.Count,
		AvgMS:  float64(s.AvgMS),
		P50MS:  s.P50MS,
		P95MS:  s.P95MS,
		P99MS:  s.P99MS,
		MaxMS:  s.MaxMS,
		Source: src,
	}
}

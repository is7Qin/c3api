// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"math"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
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

// toAPIGroup 组领域对象 → 契约类型（/user/groups 只读列表；PriceMultiplier
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

// toAPIUsageLog 用量日志领域对象 → 用户面契约类型（/user/usage_logs；
// UserUsageLog 无 AccountID/TemplateID——用户无上游账号拓扑概念；err_logs
// 分表后无 status_code/error_message 字段）。
func toAPIUsageLog(l *domain.UsageLog) UserUsageLog {
	f := RequestFormat(l.Format)
	et := ErrorType(l.ErrorType)
	return UserUsageLog{
		ID:                       &l.ID,
		RequestID:                &l.RequestID,
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
		BillingTier:              &l.BillingTier,
		AboveHit:                 &l.AboveHit,
		Overdraft:                &l.Overdraft,
		CreatedAt:                &l.CreatedAt,
	}
}

// toAPIErrLog 错误明细领域对象 → 用户面契约类型（/user/err_logs；
// UserErrLog 无 AccountID/TemplateID——用户无上游账号拓扑概念；BillingTier
// 空 = 未计费路径 → null）。
func toAPIErrLog(l *domain.UsageLog) UserErrLog {
	f := RequestFormat(l.Format)
	et := ErrorType(l.ErrorType)
	e := UserErrLog{
		ID:           &l.ID,
		RequestID:    &l.RequestID,
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
	if l.BillingTier != "" {
		e.BillingTier = &l.BillingTier
	}
	return e
}

// toAPIStatBucket 统计桶领域对象 → 契约类型（/user/stats；rewrite spec
// 2026-08-14 端点重写：Cost 毫分 → USD（/1e5，与 /admin 同口径——本包
// 兑换码换算同款系数）；TTFT 六指标在 convert 边界算定——avg = sum/count
// （无样本 0）、pN = 直方图插值（复用 repository.TTFTPercentileMS，与
// overview /admin/stats 同一实现）。
// 毫秒值输出前 math.Round 收敛整数（用户裁决 2026-08-14：除法/插值裸浮点
// 直出 → 前端显示 335.12241653418124；毫秒语义整数即可，契约 number 不变）。
func toAPIStatBucket(b *domain.StatBucket) StatBucket {
	var ttftAvg float64
	if b.TTFTCount > 0 {
		ttftAvg = math.Round(float64(b.TTFTTotalMS) / float64(b.TTFTCount))
	}
	return StatBucket{
		BucketTime:          &b.BucketTime,
		GroupID:             &b.GroupID,
		AccountID:           &b.AccountID,
		TemplateID:          &b.TemplateID,
		UserID:              &b.UserID,
		Model:               &b.Model,
		IsError:             &b.IsError,
		RequestCount:        &b.RequestCount,
		ErrorCount:          &b.ErrorCount,
		InputTokens:         &b.InputTokens,
		OutputTokens:        &b.OutputTokens,
		TotalTokens:         &b.TotalTokens,
		CacheReadTokens:     &b.CacheReadTokens,
		CacheCreationTokens: &b.CacheCreationTokens,
		Cost:                ptr(float64(b.Cost) / 1e5),
		CallCount:           &b.CallCount,
		TTFTCount:           &b.TTFTCount,
		TTFTAvgMS:           ptr(ttftAvg),
		TTFTMaxMS:           &b.TTFTMaxMS,
		TTFTP50MS:           ptr(repository.TTFTPercentileMS(b.TTFTHist, b.TTFTCount, 0.50)),
		TTFTP90MS:           ptr(repository.TTFTPercentileMS(b.TTFTHist, b.TTFTCount, 0.90)),
		TTFTP95MS:           ptr(repository.TTFTPercentileMS(b.TTFTHist, b.TTFTCount, 0.95)),
		TTFTP99MS:           ptr(repository.TTFTPercentileMS(b.TTFTHist, b.TTFTCount, 0.99)),
	}
}

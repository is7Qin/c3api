// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package config

import (
	"fmt"
	"math"
	"time"
)

// SupplierConfig 供应商收益（spec 2026-10-09 §6.4）。config 值只在快照装配时
// （share_bp_default）或记账链读取时（freeze_hours）解析 NULL 用，**不写回 DB**
// （保持「DB 无行即默认」的读路径免初始化语义）。
type SupplierConfig struct {
	// Enabled 全局开关：false ⇒ 收益链在构造期不存在（不注册 worker、视图恒
	// nil、新行不入收益索引、供应商面路由不挂载）；四张新表仍由 ent bootstrap
	// 创建（表存在零行）。
	Enabled bool `koanf:"enabled"`
	// FreezeEnabled 冻结机制开关：false ⇒ 不注册解冻 worker，全部收益直接入
	// available，且存量冻结最终释放（§5.4，显式 G3 例外）。
	FreezeEnabled bool `koanf:"freeze_enabled"`
	// FreezeHours 全局默认冻结小时（逐供应商 freeze_hours 覆盖）。
	FreezeHours int `koanf:"freeze_hours"`
	// ThawGranularity 解冻时间对齐粒度（整秒）。
	ThawGranularity time.Duration `koanf:"thaw_granularity"`
	// ShareBpDefault 全局默认分成率（bp；10000 = 100%）。
	ShareBpDefault int `koanf:"share_bp_default"`
	// 付款风控门（C1/§6.5）。
	PayoutMaxBacklogRows int           `koanf:"payout_max_backlog_rows"`
	PayoutMaxBacklogAge  time.Duration `koanf:"payout_max_backlog_age"`
	PayoutMaxObserveAge  time.Duration `koanf:"payout_max_observe_age"`
	RiskReviewMaxAge     time.Duration `koanf:"risk_review_max_age"`
}

// shareBpFull 分成率满值（10000 = 100%），与 domain/proxy 同口径。
const shareBpFull = 10000

// maxSupplierBuckets 桶密度跨字段上界（§6.4）：ceil(freeze_hours×3600/g)+1 ≤ 64。
const maxSupplierBuckets = 64

// validateSupplier 供应商配置 fail-fast 专用校验（spec 2026-10-09 §6.4）：
// **不塞既有两个表**——既有 duration 表只判 <1ms、既有 int 表只判 <1，直接
// 塞入会把合法的 share_bp=0 判为非法且不校验上限。
//
// 无条件校验（config fail-fast 无条件，§5.5 启动条件表）。
func validateSupplier(c *SupplierConfig) error {
	// thaw_granularity ≥ 1m 且整秒（避免 duration 截秒歧义）。
	if c.ThawGranularity < time.Minute {
		return fmt.Errorf("supplier.thaw_granularity must be >= 1m (got %s)", c.ThawGranularity)
	}
	if c.ThawGranularity%time.Second != 0 {
		return fmt.Errorf("supplier.thaw_granularity must be a whole number of seconds (got %s)", c.ThawGranularity)
	}
	if err := ValidateSupplierShareBp(c.ShareBpDefault); err != nil {
		return fmt.Errorf("supplier.share_bp_default: %w", err)
	}
	if err := ValidateSupplierFreezeHours(c.FreezeHours, c.ThawGranularity); err != nil {
		return fmt.Errorf("supplier.freeze_hours: %w", err)
	}
	// 付款风控四键。
	if c.PayoutMaxBacklogRows < 0 {
		return fmt.Errorf("supplier.payout_max_backlog_rows must be >= 0 (got %d)", c.PayoutMaxBacklogRows)
	}
	for _, d := range []struct {
		path  string
		value time.Duration
	}{
		{"supplier.payout_max_backlog_age", c.PayoutMaxBacklogAge},
		{"supplier.payout_max_observe_age", c.PayoutMaxObserveAge},
		{"supplier.risk_review_max_age", c.RiskReviewMaxAge},
	} {
		if d.value < time.Second {
			return fmt.Errorf("%s must be >= 1s (got %s)", d.path, d.value)
		}
	}
	return nil
}

// ValidateSupplierShareBp 分成率值域校验（[0,10000]；0 合法）。管理面 PATCH
// （§6.3 share_bp）复用本函数——config 校验不覆盖 DB 逐供应商 override。
func ValidateSupplierShareBp(bp int) error {
	if bp < 0 || bp > shareBpFull {
		return fmt.Errorf("share_bp must be in [0, %d] (got %d)", shareBpFull, bp)
	}
	return nil
}

// ValidateSupplierFreezeHours 冻结小时值域 + 跨字段上界校验（spec §6.4）：
// freeze_hours ≥ 0；ceil(freeze_hours×3600 / g_seconds) + 1 ≤ 64（g 整秒）。
// 管理面 PATCH（§6.3 freeze_hours）复用本函数，按「该供应商生效后的整体配置」
// 判跨字段（全局 g）。乘法前做上限检查防 freeze_hours×3600 溢出。
func ValidateSupplierFreezeHours(freezeHours int, g time.Duration) error {
	if freezeHours < 0 {
		return fmt.Errorf("freeze_hours must be >= 0 (got %d)", freezeHours)
	}
	gSeconds := int64(g / time.Second)
	if gSeconds <= 0 {
		return fmt.Errorf("thaw_granularity must be >= 1s (got %s)", g)
	}
	fh := int64(freezeHours)
	if fh > math.MaxInt64/3600 {
		return fmt.Errorf("freeze_hours %d overflows seconds", freezeHours)
	}
	seconds := fh * 3600
	// ceil(seconds / gSeconds) + 1 ≤ maxSupplierBuckets。
	// 用 (seconds + gSeconds - 1) 可能溢出（seconds 接近 MaxInt64）——因 fh 已
	// 受 MaxInt64/3600 约束，seconds ≤ MaxInt64，但 +gSeconds-1 仍可能溢出：
	// 改判定 ceil ≤ maxSupplierBuckets-1 ⇔ seconds ≤ (maxSupplierBuckets-1)×gSeconds。
	maxSeconds := int64(maxSupplierBuckets-1) * gSeconds
	if seconds > maxSeconds {
		return fmt.Errorf("freeze_hours %d incompatible with thaw_granularity %s: ceil(H/g)+1 > %d", freezeHours, g, maxSupplierBuckets)
	}
	return nil
}

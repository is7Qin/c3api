// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import "context"

// 价格表结构约束引导（与分区引导同生命周期：ent migrate 不承载 CHECK，
// 由幂等 DDL 独占建约束——裁决 2026-08-24，Atlas 注解路线在"每测试 DROP+
// 重建 schema"模式下引发间歇性迁移污染，弃用）。

const priceVariantsEffectCheckDDL = `DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'price_variants_effect_at_least_one') THEN
    ALTER TABLE price_variants DROP CONSTRAINT price_variants_effect_at_least_one;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'price_variants_effect_at_least_one_v2') THEN
    ALTER TABLE price_variants ADD CONSTRAINT price_variants_effect_at_least_one_v2
      CHECK (mult_bp IS NOT NULL OR set_input_per_m IS NOT NULL OR set_output_per_m IS NOT NULL OR set_cache_read_per_m IS NOT NULL OR set_cache_creation_per_m IS NOT NULL OR set_price_per_call IS NOT NULL OR set_img_in_tok_per_m IS NOT NULL OR set_img_out_tok_per_m IS NOT NULL OR set_price_per_image IS NOT NULL);
  END IF;
END $$;`

// EnsurePriceVariantsEffectCheck 幂等补齐 price_variants 效果字段至少一非空
// 约束（spec P-2：变体必须携带至少一种效果——mult_bp 或 set_input/set_output）。
// 服务层 ReplacePriceVariants 已做同语义校验；此约束兜底 litellm 直写路径与
// 手工 DDL。启动即执行，失败 fatal（与分区引导同级）。
func (r *PartitionRepo) EnsurePriceVariantsEffectCheck(ctx context.Context) error {
	return r.execDDLTolerateRace(ctx, priceVariantsEffectCheckDDL)
}

const codexSearchSeedDDL = `INSERT INTO price_entries (model, mode, price_per_call, source, created_at, updated_at)
VALUES ('codex-search', 'call', 1000, 'manual', NOW(), NOW())
ON CONFLICT (model) DO NOTHING;`

// EnsureCodexSearchSeed 幂等种子：model='codex-search', mode='call',
// price_per_call=1000 (毫分=$0.01), source='manual'。litellm 同步守卫天然
// 不碰 manual 行；管理员删除后内联兜底仍生效。
func (r *PartitionRepo) EnsureCodexSearchSeed(ctx context.Context) error {
	return r.execDDLTolerateRace(ctx, codexSearchSeedDDL)
}

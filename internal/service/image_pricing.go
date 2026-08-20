// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// imagePriceReloadPage 快照全量加载的分页大小（与 pricingReloadPage 同量级：
// image_price 行数 ≤ pricings 行数，单页内取完）。
const imagePriceReloadPage = 1000

// ReloadImagePricing 全量重载图片价格快照（公开）：litellm 同步拉取成功后 +
// 管理端改价后调用，读路径（GetImagePrice）即时生效。失败 fail-safe
// （Warn + 保留旧快照，对齐 ReloadPricing）。
func (s *Service) ReloadImagePricing() {
	s.reloadImagePricing(context.Background())
}

// ReloadImagePricingCtx 全量重载图片价格快照并返回错误（snapshot.Registry
// 适配：错误进注册表 Status 可观测；启动就绪统一首刷入口）。失败保留旧快照
// （fail-safe，与 ReloadImagePricing 同语义）。
func (s *Service) ReloadImagePricingCtx(ctx context.Context) error {
	m, err := s.loadImagePricing(ctx)
	if err != nil {
		return err
	}
	s.imagePrice.Store(&m)
	return nil
}

// reloadImagePricing 从 DB 全量加载图片价格快照（管理端改价/sync 路径调用）。
// 失败 fail-safe：仅 Warn + 保留旧快照/空快照，不阻断服务启动——读快照缺失
// 按 ErrNotFound（Task B images 端点 402 拒绝计费而非按 0 计价）。
func (s *Service) reloadImagePricing(ctx context.Context) {
	m, err := s.loadImagePricing(ctx)
	if err != nil {
		return // loadImagePricing 已 Warn
	}
	s.imagePrice.Store(&m)
}

// loadImagePricing 从 DB 分页加载图片价格快照；错误原样返回（Warn 在此统一）。
func (s *Service) loadImagePricing(ctx context.Context) (map[string]*domain.ImagePrice, error) {
	var all []*domain.ImagePrice
	for offset := 0; ; offset += imagePriceReloadPage {
		rows, _, err := s.store.ListImagePrice(ctx, repository.ListQuery{
			Limit: imagePriceReloadPage, Offset: offset, Sort: "model", Order: "asc",
		}, nil, "", "")
		if err != nil {
			if s.log != nil {
				s.log.Warn("image price snapshot reload failed", logx.Error(err))
			}
			return nil, err
		}
		all = append(all, rows...)
		if len(rows) < imagePriceReloadPage {
			break
		}
	}
	return buildImagePriceSnapshot(all), nil
}

// buildImagePriceSnapshot 快照构建：model → 图片价格行。同一 model 出现多行
// （防御——仓库 unique(model) 约束下不应出现）按 source 优先级收敛：manual
// 覆盖 litellm（与表内"一行 = 最终生效价"、拉取 upsert 的行级互斥语义一致，
// 对齐 buildPricingSnapshot）。
func buildImagePriceSnapshot(rows []*domain.ImagePrice) map[string]*domain.ImagePrice {
	m := make(map[string]*domain.ImagePrice, len(rows))
	for _, p := range rows {
		if prev, ok := m[p.Model]; ok && prev.Source == domain.PricingSourceManual {
			continue // 已收敛为 manual：后续行（含 manual/litellm）一律不覆盖
		}
		m[p.Model] = p
	}
	return m
}

// GetImagePrice 快照读图价格（Task B images 端点计费热路径，零 DB）：命中返回
// 价格行；缺失 → ErrNotFound（调用方应 402 拒绝计费而非按 0 计价——空行语义
// = 端点定生死，对齐 GetPrice/M-1 语义）。
func (s *Service) GetImagePrice(model string) (*domain.ImagePrice, error) {
	if m := s.imagePrice.Load(); m != nil {
		if p, ok := (*m)[model]; ok {
			return p, nil
		}
	}
	// G3-2 分层约定：对外（响应体）恒英文、内部可中文——本错误仅内部消费
	// （forward.go/caller.go 只 log.Warn，不向响应体回显；402 走固定 errNoPrice
	// 文案），中文保留可读性。
	return nil, fmt.Errorf("%w: model=%q（无图片价格数据：请管理端设价或等待 litellm 同步）", ErrNotFound, model)
}

// UpsertManualImagePrice 手动设图价格（管理端 PUT /api/admin/image-price?model=X）：
// 校验（model 非空；三分量全 nil → 400——行有效性 = 至少一价非 nil；非 nil 且
// < 0 → 400）+ 落库（upsert 强制 source=manual，可接管 litellm 行）+ 成功后
// 重载快照（读路径即时生效）。ImagePriceManual 单位：token 价毫分/1M、
// per-image 毫分/张（handler 边界已由 API 入参 USD 换算）。
func (s *Service) UpsertManualImagePrice(ctx context.Context, m *repository.ImagePriceManual) (*domain.ImagePrice, error) {
	if m.Model == "" {
		return nil, fmt.Errorf("%w: model is required", ErrInvalidInput)
	}
	if !m.HasAnyPrice() {
		return nil, fmt.Errorf("%w: at least one image price component is required", ErrInvalidInput)
	}
	nonNeg := func(v *int64, name string) error {
		if v != nil && *v < 0 {
			return fmt.Errorf("%w: %s must be >= 0", ErrInvalidInput, name)
		}
		return nil
	}
	for _, f := range []struct {
		name string
		v    *int64
	}{
		{"input_image_token_price_per_million", m.InputImageTokenPricePerMillion},
		{"output_image_token_price_per_million", m.OutputImageTokenPricePerMillion},
		{"output_cost_per_image", m.OutputCostPerImageMilli},
	} {
		if err := nonNeg(f.v, f.name); err != nil {
			return nil, err
		}
	}
	p, err := s.store.UpsertImageManual(ctx, m)
	if err != nil {
		return nil, err
	}
	s.reloadImagePricing(ctx)
	return p, nil
}

// DeleteManualImagePrice 删除手动图价格（管理端 DELETE /api/admin/image-price?model=X）：
// 仅 source=manual 行可删（litellm 行 → ErrConflict；仓库语义）；成功后重载
// 快照——该 model 从快照消失（缺失窗口内 GetImagePrice → ErrNotFound，
// 下轮拉取补回）。
func (s *Service) DeleteManualImagePrice(ctx context.Context, model string) error {
	if err := s.store.DeleteImageManual(ctx, model); err != nil {
		return mapRepoErr(err)
	}
	s.reloadImagePricing(ctx)
	return nil
}

// ListImagePrice 管理端图片价格列表（GET /api/admin/image-price）：分页 +
// source/model 筛选 + sort 白名单校验（非法 → ErrInvalidInput 400）。
func (s *Service) ListImagePrice(ctx context.Context, q repository.ListQuery, source *domain.PricingSource, provider, model string) ([]*domain.ImagePrice, int64, error) {
	if source != nil && !source.Valid() {
		return nil, 0, fmt.Errorf("%w: invalid source %q", ErrInvalidInput, *source)
	}
	if err := validateListQuery(q, listSortFields["image_price"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListImagePrice(ctx, q, source, provider, model)
}

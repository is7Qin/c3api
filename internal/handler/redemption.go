// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"errors"
	"math"
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/server"
	"github.com/is7qin/c3api/internal/service"
)

// 兑换码管理面（/api/admin/redemption-codes）：生成/列表/审计/失效。
// 生成参数校验在 service 层（validateGenerateRequest），handler 只做 JSON 解码
// 与 created_by 注入（决策 5：JWT 路径 context 有 UserID；静态 admin token → 0）。

// createdBy 取认证中间件注入的 platform_admin 用户 id（/admin JWT 路径）；
// 静态 admin token 路径未注入 → 0（系统创建，决策 5）。
func createdBy(r *http.Request) int64 {
	id, _ := server.UserIDFromContext(r.Context())
	return id
}

// pageToQuery 增强分页范式（page 1-based + page_size）→ repository.ListQuery。
// page 缺省/越下界按 1；page_size 缺省 20，越界（<1 或 >1000）→ ErrInvalidInput 400。
func pageToQuery(page, pageSize *int) (repository.ListQuery, error) {
	p := 1
	if page != nil && *page > 1 {
		p = *page
	}
	size := 20
	if pageSize != nil {
		size = *pageSize
	}
	if size < 1 || size > 1000 {
		return repository.ListQuery{}, service.ErrInvalidInput
	}
	return repository.ListQuery{Limit: size, Offset: (p - 1) * size}, nil
}

// apiRedemptionValueToMillis 契约面值 → 毫分存储（API 输入边界换算，与 balance
// 毫分↔USD 同构）：balance/temp_balance = USD（1 USD = 100,000 毫分，
// math.Round 消除取整误差）；concurrency = 并发数——非货币，必须整数（小数 →
// 400，不静默取整）；≤ 0 → 400。type 非法 → (0, nil) 原样传 service 兜底
// validateGenerateRequest（保持既有 400 文案）。
func apiRedemptionValueToMillis(typ domain.RedemptionType, v float64) (int64, error) {
	switch typ {
	case domain.RedemptionTypeBalance, domain.RedemptionTypeTempBalance:
		if v <= 0 {
			return 0, errors.New("value must be > 0")
		}
		return usdToMillis(v), nil
	case domain.RedemptionTypeConcurrency:
		if v != math.Trunc(v) {
			return 0, errors.New("concurrency value must be an integer")
		}
		if v <= 0 {
			return 0, errors.New("concurrency value must be > 0")
		}
		return int64(v), nil
	}
	return 0, nil
}

// redemptionValueToAPI 毫分存储 → 契约面值（API 边界展示换算）：
// balance/temp_balance → USD float64（1 USD = 100,000 毫分）；concurrency →
// 并发数 float64 直出（非货币，仅类型转换——整数 float64 精确，JSON 序列化
// 5.0 → "5"，无精度问题）。
func redemptionValueToAPI(typ domain.RedemptionType, millis int64) float64 {
	if typ == domain.RedemptionTypeConcurrency {
		return float64(millis)
	}
	return millisToUSD(millis)
}

// PostRedemptionCodes 生成兑换码（1..count 个；count 默认 1，上限 1000，
// ServerInterface）。
func (h *AdminAPI) PostRedemptionCodes(w http.ResponseWriter, r *http.Request) {
	var in GenerateRequest
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	// 面值边界换算（balance/temp_balance USD → 毫分；concurrency 整数校验）——
	// 存储恒毫分，service 仍收 int64。
	value, err := apiRedemptionValueToMillis(domain.RedemptionType(in.Type), in.Value)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	codes, err := h.svc.GenerateCodes(r.Context(), service.GenerateRequest{
		Type:              domain.RedemptionType(in.Type),
		Value:             value,
		Remark:            in.Remark,
		ExpiresAt:         in.ExpiresAt,
		ResourceExpiresAt: in.ResourceExpiresAt,
		MaxUses:           deref(in.MaxUses),
		Count:             deref(in.Count),
	}, createdBy(r))
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]RedemptionCode, 0, len(codes))
	for _, c := range codes {
		out = append(out, toAPIRedemptionCode(c))
	}
	httpface.WriteJSON(w, http.StatusOK, GenerateResponse{Codes: out})
}

// GetRedemptionCodes 兑换码列表（增强分页范式 + type/status 筛选 + sort 白名单，
// ServerInterface）。
func (h *AdminAPI) GetRedemptionCodes(w http.ResponseWriter, r *http.Request, params GetRedemptionCodesParams) {
	q, err := pageToQuery(params.Page, params.PageSize)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	q.Sort = deref(params.Sort)
	q.Order = string(deref(params.Order))
	var typ *domain.RedemptionType
	if params.Type != nil {
		t := domain.RedemptionType(*params.Type)
		typ = &t
	}
	var st *domain.RedemptionStatus
	if params.Status != nil {
		s := domain.RedemptionStatus(*params.Status)
		st = &s
	}
	rows, total, err := h.svc.ListCodes(r.Context(), q, typ, st)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]RedemptionCode, 0, len(rows))
	for _, c := range rows {
		out = append(out, toAPIRedemptionCode(c))
	}
	httpface.WriteJSON(w, http.StatusOK, RedemptionCodeListResponse{Total: total, Rows: out})
}

// GetRedemptionCodesIdUses 某码的兑换记录（审计；码缺失 → 404，ServerInterface）。
// 面值换算需码的 type（use 行不存类型——与前端 uses 弹窗同构：取码行 Type 换算）。
// limit/offset 直透 ListQuery（缺省归一在 repo——spec 2026-08-17 补分页）。
func (h *AdminAPI) GetRedemptionCodesIdUses(w http.ResponseWriter, r *http.Request, id int64, params GetRedemptionCodesIdUsesParams) {
	code, err := h.svc.GetCode(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	rows, total, err := h.svc.GetCodeUses(r.Context(), id, repository.ListQuery{
		Limit:  int(deref(params.Limit)),
		Offset: int(deref(params.Offset)),
	})
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]RedemptionUse, 0, len(rows))
	for _, u := range rows {
		out = append(out, toAPIRedemptionUse(code.Type, u))
	}
	httpface.WriteJSON(w, http.StatusOK, RedemptionUseListResponse{Total: total, Rows: out})
}

// PostRedemptionCodesIdDeactivate 单码失效（已失效 no-op 成功；缺失 → 404，
// ServerInterface）。
func (h *AdminAPI) PostRedemptionCodesIdDeactivate(w http.ResponseWriter, r *http.Request, id int64) {
	if err := h.svc.DeactivateCode(r.Context(), id); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, DeactivateResponse{Deactivated: true})
}

// PostRedemptionCodesBatchDeactivate 批量失效（单事务；已失效 no-op；缺失 id →
// 404 含缺失详情，ServerInterface）。
func (h *AdminAPI) PostRedemptionCodesBatchDeactivate(w http.ResponseWriter, r *http.Request) {
	var in BatchDeactivateRequest
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	ids, err := normalizeIDs(in.Ids)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := h.svc.DeactivateCodesBatch(r.Context(), ids)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, BatchDeactivateResponse{Deactivated: int(n)})
}

// toAPIRedemptionCode 兑换码领域对象 → 契约类型（Value 毫分 → 面值换算：按
// type——balance/temp_balance → USD；concurrency → 并发数直出）。
func toAPIRedemptionCode(c *domain.RedemptionCode) RedemptionCode {
	return RedemptionCode{
		ID:                c.ID,
		Code:              c.Code,
		Type:              RedemptionType(c.Type),
		Value:             redemptionValueToAPI(c.Type, c.Value),
		Remark:            c.Remark,
		ExpiresAt:         c.ExpiresAt,
		ResourceExpiresAt: c.ResourceExpiresAt,
		MaxUses:           c.MaxUses,
		UsedCount:         c.UsedCount,
		Status:            RedemptionStatus(c.Status),
		CreatedBy:         c.CreatedBy,
		CreatedAt:         c.CreatedAt,
		UpdatedAt:         c.UpdatedAt,
	}
}

// toAPIRedemptionUse 兑换审计领域对象 → 契约类型（Value 换算同 toAPIRedemptionCode；
// use 行不存码类型，由调用方传码 type——审计端点先取码）。
func toAPIRedemptionUse(codeType domain.RedemptionType, u *domain.RedemptionUse) RedemptionUse {
	return RedemptionUse{
		ID:                u.ID,
		CodeID:            u.CodeID,
		UserID:            u.UserID,
		Value:             redemptionValueToAPI(codeType, u.Value),
		ResourceExpiresAt: u.ResourceExpiresAt,
		CreatedAt:         u.CreatedAt,
	}
}

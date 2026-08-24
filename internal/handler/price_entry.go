// SPDX-License-Identifier: AGPL-3.0-or-later
package handler

import (
	"errors"
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

func (h *AdminAPI) GetPrices(w http.ResponseWriter, r *http.Request, params GetPricesParams) {
	q, err := pageToQuery(params.Page, params.PageSize)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	q.Sort = deref(params.Sort)
	q.Order = string(deref(params.Order))
	var src *domain.PricingSource
	if params.Source != nil {
		s := domain.PricingSource(*params.Source)
		src = &s
	}
	var mode *domain.PriceMode
	if params.Mode != nil {
		m := domain.PriceMode(*params.Mode)
		mode = &m
	}
	var prov *string
	if params.Provider != nil {
		v := deref(params.Provider)
		if v != "" {
			prov = &v
		}
	}
	rows, total, err := h.svc.ListPriceEntries(r.Context(), q, src, mode, prov, deref(params.Model))
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]PriceEntry, 0, len(rows))
	for _, p := range rows {
		out = append(out, toAPIPriceEntry(p))
	}
	httpface.WriteJSON(w, http.StatusOK, PriceEntryListResponse{Total: total, Rows: out})
}

func (h *AdminAPI) GetPriceEntry(w http.ResponseWriter, r *http.Request, params GetPriceEntryParams) {
	p, err := h.svc.GetPriceEntry(r.Context(), params.Model)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	pe := toAPIPriceEntry(p)
	httpface.WriteJSON(w, http.StatusOK, pe)
}

func (h *AdminAPI) PutPriceEntry(w http.ResponseWriter, r *http.Request, params PutPriceEntryParams) {
	var in PriceEntryUpsert
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	m := &repository.PriceEntryManual{
		Model: params.Model, Mode: domain.PriceMode(in.Mode),
		InputPerM: usdToMillisPtr(in.InputPerM), OutputPerM: usdToMillisPtr(in.OutputPerM),
		CacheReadPerM: usdToMillisPtr(in.CacheReadPerM), CacheWritePerM: usdToMillisPtr(in.CacheWritePerM),
		PricePerCall: usdPerCallToMilliPtr(in.PricePerCall),
		ImgInTokPerM: usdToMillisPtr(in.ImgInTokPerM), ImgOutTokPerM: usdToMillisPtr(in.ImgOutTokPerM),
		PricePerImage: usdPerImageToMilliPtr(in.PricePerImage),
	}
	p, err := h.svc.UpsertPriceEntry(r.Context(), m)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIPriceEntry(p))
}

func (h *AdminAPI) DeletePriceEntry(w http.ResponseWriter, r *http.Request, params DeletePriceEntryParams) {
	if err := h.svc.DeletePriceEntry(r.Context(), params.Model); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, DeletedResponse{Deleted: true})
}

func (h *AdminAPI) GetPriceVariants(w http.ResponseWriter, r *http.Request, params GetPriceVariantsParams) {
	rows, err := h.svc.ListPriceVariants(r.Context(), params.Model)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]PriceVariant, 0, len(rows))
	for _, v := range rows {
		out = append(out, toAPIPriceVariant(v))
	}
	httpface.WriteJSON(w, http.StatusOK, PriceVariantListResponse{Rows: &out})
}

func (h *AdminAPI) PutPriceVariants(w http.ResponseWriter, r *http.Request, params PutPriceVariantsParams) {
	var in PriceVariantListRequest
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	var vars []*domain.PriceVariant
	if in.Variants != nil {
		for _, v := range *in.Variants {
			pv := &domain.PriceVariant{Model: params.Model, Seq: deref(v.Seq)}
			if v.ServiceTier != nil {
				pv.ServiceTier = v.ServiceTier
			}
			if v.CtxMin != nil {
				pv.CtxMin = v.CtxMin
			}
			if v.CtxMax != nil {
				pv.CtxMax = v.CtxMax
			}
			pv.TimeStart = v.TimeStart
			pv.TimeEnd = v.TimeEnd
			pv.DowMask = v.DowMask
			if v.Multiplier != nil {
				m := normalToMult(*v.Multiplier)
				pv.MultBP = &m
			}
			pv.SetInputPerM = usdToMillisPtr(v.SetInputPerM)
			pv.SetOutputPerM = usdToMillisPtr(v.SetOutputPerM)
			pv.SetCacheReadPerM = usdToMillisPtr(v.SetCacheReadPerM)
			pv.SetCacheCreationPerM = usdToMillisPtr(v.SetCacheCreationPerM)
			pv.SetPricePerCall = usdPerCallToMilliPtr(v.SetPricePerCall)
			pv.SetImgInTokPerM = usdToMillisPtr(v.SetImgInTokPerM)
			pv.SetImgOutTokPerM = usdToMillisPtr(v.SetImgOutTokPerM)
			pv.SetPricePerImage = usdPerImageToMilliPtr(v.SetPricePerImage)
			vars = append(vars, pv)
		}
	}
	out, err := h.svc.ReplacePriceVariants(r.Context(), params.Model, vars)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	apiOut := make([]PriceVariant, 0, len(out))
	for _, v := range out {
		apiOut = append(apiOut, toAPIPriceVariant(v))
	}
	httpface.WriteJSON(w, http.StatusOK, PriceVariantListResponse{Rows: &apiOut})
}

func (h *AdminAPI) DeletePriceVariants(w http.ResponseWriter, r *http.Request, params DeletePriceVariantsParams) {
	_, err := h.svc.ReplacePriceVariants(r.Context(), params.Model, nil)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, DeletedResponse{Deleted: true})
}

func (h *AdminAPI) PostPricingSync(w http.ResponseWriter, r *http.Request) {
	stats, err := h.svc.SyncPricingNow(r.Context())
	if err != nil {
		if errors.Is(err, service.ErrPriceFetch) {
			httpface.WriteErr(w, http.StatusBadGateway, "pricing sync failed")
			return
		}
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, PricingSyncResponse{
		Rows: stats.Rows, Skipped: stats.Skipped, Updated: stats.Updated,
	})
}

func (h *AdminAPI) PostPricingSyncPreview(w http.ResponseWriter, r *http.Request) {
	preview, err := h.svc.PreviewPricingSync(r.Context())
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, PricingSyncPreviewResponse{
		ToAdd: preview.ToAdd, ToUpdate: preview.ToUpdate, Skipped: preview.Skipped, VariantsChanged: &preview.VariantsChanged,
	})
}

func toAPIPriceEntry(p *domain.PriceEntry) PriceEntry {
	return PriceEntry{
		Model: p.Model, Mode: PriceEntryMode(p.Mode),
		InputPerM: millisToUSDPtr(p.InputPerM), OutputPerM: millisToUSDPtr(p.OutputPerM),
		CacheReadPerM: millisToUSDPtr(p.CacheReadPerM), CacheWritePerM: millisToUSDPtr(p.CacheWritePerM),
		PricePerCall: milliPerCallToUSDPtr(p.PricePerCall),
		ImgInTokPerM: millisToUSDPtr(p.ImgInTokPerM), ImgOutTokPerM: millisToUSDPtr(p.ImgOutTokPerM),
		PricePerImage: milliPerImageToUSDPtr(p.PricePerImage),
		Provider:      (*Provider)(p.Provider), Source: PricingSource(p.Source),
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}
func toAPIPriceVariant(v *domain.PriceVariant) PriceVariant {
	var mult *float64
	if v.MultBP != nil {
		f := multToNormal(*v.MultBP)
		mult = &f
	}
	return PriceVariant{
		Model: v.Model, Seq: v.Seq,
		ServiceTier: v.ServiceTier, CtxMin: v.CtxMin, CtxMax: v.CtxMax,
		TimeStart: v.TimeStart, TimeEnd: v.TimeEnd, DowMask: v.DowMask, Multiplier: mult,
		SetInputPerM: millisToUSDPtr(v.SetInputPerM), SetOutputPerM: millisToUSDPtr(v.SetOutputPerM),
		SetCacheReadPerM: millisToUSDPtr(v.SetCacheReadPerM), SetCacheCreationPerM: millisToUSDPtr(v.SetCacheCreationPerM),
		SetPricePerCall: milliPerCallToUSDPtr(v.SetPricePerCall),
		SetImgInTokPerM: millisToUSDPtr(v.SetImgInTokPerM), SetImgOutTokPerM: millisToUSDPtr(v.SetImgOutTokPerM),
		SetPricePerImage: milliPerImageToUSDPtr(v.SetPricePerImage),
		CreatedAt:        v.CreatedAt, UpdatedAt: v.UpdatedAt,
	}
}

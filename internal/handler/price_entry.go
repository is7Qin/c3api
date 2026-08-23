// SPDX-License-Identifier: AGPL-3.0-or-later
package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
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
	rows, total, err := h.svc.ListPriceEntries(r.Context(), q, src, mode, deref(params.Model))
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

func (h *AdminAPI) GetPricesModel(w http.ResponseWriter, r *http.Request, model string) {
	p, err := h.svc.GetPriceEntry(r.Context(), model)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	pe := toAPIPriceEntry(p)
	httpface.WriteJSON(w, http.StatusOK, pe)
}

func (h *AdminAPI) PutPricesModel(w http.ResponseWriter, r *http.Request, model string) {
	var in PriceEntryUpsert
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	m := &repository.PriceEntryManual{
		Model: model, Mode: domain.PriceMode(in.Mode),
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

func (h *AdminAPI) DeletePricesModel(w http.ResponseWriter, r *http.Request, model string) {
	if err := h.svc.DeletePriceEntry(r.Context(), model); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, DeletedResponse{Deleted: true})
}

func (h *AdminAPI) GetPricesModelVariants(w http.ResponseWriter, r *http.Request, model string) {
	rows, err := h.svc.ListPriceVariants(r.Context(), model)
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

func (h *AdminAPI) PutPricesModelVariants(w http.ResponseWriter, r *http.Request, model string) {
	var in PriceVariantListRequest
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	var vars []*domain.PriceVariant
	if in.Variants != nil {
		for _, v := range *in.Variants {
			pv := &domain.PriceVariant{Model: model, Seq: deref(v.Seq)}
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
			pv.MultBP = v.MultBp
			pv.SetInputPerM = usdToMillisPtr(v.SetInputPerM)
			pv.SetOutputPerM = usdToMillisPtr(v.SetOutputPerM)
			vars = append(vars, pv)
		}
	}
	out, err := h.svc.ReplacePriceVariants(r.Context(), model, vars)
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

func (h *AdminAPI) DeletePricesModelVariants(w http.ResponseWriter, r *http.Request, model string) {
	_, err := h.svc.ReplacePriceVariants(r.Context(), model, nil)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, DeletedResponse{Deleted: true})
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

func intPtr(i int) *int { return &i }

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
	return PriceVariant{
		Model: v.Model, Seq: v.Seq,
		ServiceTier: v.ServiceTier, CtxMin: v.CtxMin, CtxMax: v.CtxMax,
		TimeStart: v.TimeStart, TimeEnd: v.TimeEnd, DowMask: v.DowMask, MultBP: v.MultBP,
		SetInputPerM: millisToUSDPtr(v.SetInputPerM), SetOutputPerM: millisToUSDPtr(v.SetOutputPerM),
		CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/service"
)

// 代理池管理面（spec-proxy-pool §6.1）的六个端点。
//
// **本文件只做线格式投影**：JSON → service.ProxyInput、domain.Proxy → Proxy。
// 语义校验（name/scheme/host/port/凭据、409、404、探测语义）全在 service
// （internal/service/proxy.go，M1b 已实现）——在 handler 再写一份只会得到两套
// 判据，第二套迟早和第一套不一致。

// GetProxies 代理条目列表（无分页：条目数是十到百量级的小表，按 id 稳定序）。
func (h *AdminAPI) GetProxies(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListProxies(r.Context())
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]Proxy, 0, len(rows))
	for _, p := range rows {
		out = append(out, toAPIProxy(p))
	}
	httpface.WriteJSON(w, http.StatusOK, ProxyListResponse{Rows: out})
}

// PostProxies 新建代理条目：service 落库返回 id，再读一次回显（探测三列初值也
// 由这一读给出，与 GET 单个同形）。
func (h *AdminAPI) PostProxies(w http.ResponseWriter, r *http.Request) {
	var body ProxyConfig
	if err := httpface.Decode(r, &body); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	in, err := proxyInputFromBody(&body)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := h.svc.CreateProxy(r.Context(), in)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	writeProxyByID(w, r, h, id)
}

// GetProxiesId 单个条目（不存在/已软删 → 404，由 service 的 GetProxy 判定）。
func (h *AdminAPI) GetProxiesId(w http.ResponseWriter, r *http.Request, id int64) {
	writeProxyByID(w, r, h, id)
}

// PutProxiesId 全量更新（凭据三态见 proxyInputFromBody）。回显走一次读：service
// 的 UpdateProxy 只返回 error，而 PUT 的响应契约是更新后的条目。
//
// 更新**不改 last_probe_***（spec §6.3 列的写者归属：那三列只由 probe 端点写），
// 故回显里的探测结论是上一次探测的真实结论，而不是被这次更新抹成「从未探测」。
func (h *AdminAPI) PutProxiesId(w http.ResponseWriter, r *http.Request, id int64) {
	var body ProxyConfig
	if err := httpface.Decode(r, &body); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	in, err := proxyInputFromBody(&body)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.svc.UpdateProxy(r.Context(), id, in); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	writeProxyByID(w, r, h, id)
}

// DeleteProxiesId 软删 + 同事务解绑（service 已把绑定账号的 proxy_id 置 NULL ⇒
// 回落直连）。响应形态照抄相邻 delete：200 + DeletedResponse。
func (h *AdminAPI) DeleteProxiesId(w http.ResponseWriter, r *http.Request, id int64) {
	if err := h.svc.DeleteProxy(r.Context(), id); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, DeletedResponse{Deleted: true})
}

// PostProxiesIdProbe TCP 探测（5s）并写回 last_probe_*，返回最新条目。
//
// **拨不通不是 API 错误**：service 把失败记成探测结论（last_probe_ok=false +
// 已脱敏的错误文本）并返回 (条目, nil)，故这里仍是 200——探测的目的就是把
// 「连不上」记下来；只有条目不存在（404）或写回失败才是错误。
func (h *AdminAPI) PostProxiesIdProbe(w http.ResponseWriter, r *http.Request, id int64) {
	p, err := h.svc.ProbeProxy(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIProxy(p))
}

// writeProxyByID 读一次并回显（POST/PUT/GET 单个共用的收尾）。
func writeProxyByID(w http.ResponseWriter, r *http.Request, h *AdminAPI, id int64) {
	p, err := h.svc.GetProxy(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIProxy(p))
}

// proxyInputFromBody 线格式 → service.ProxyInput：**只做投影与三态翻译**，值域判定
// 交给 service（name 长度、scheme 白名单、host 分隔符/IPv6、port 范围、凭据控制
// 字符）。缺省（未提供的必填）落零值，同样由 service 判 400——handler 不猜。
//
// 三态用既有 helper（与 account.go 同一套，语义不另写一份）：
//   - username/password/remark（可空标量）：缺席 → nil（不变）；null → &""（清空）；
//     "" → 400；非空 → &v。**空串哨兵已取消**：清空只有一个拼法 null；
//   - enabled（不可空标量）：缺席 → nil（Create 缺省 true）；null → 400（null 不是
//     enabled 的一种状态）；true/false → &v。
func proxyInputFromBody(f *ProxyConfig) (service.ProxyInput, error) {
	var in service.ProxyInput
	in.Name = f.Name
	in.Scheme = domain.ProxyScheme(f.Scheme)
	in.Host = f.Host
	in.Port = f.Port
	username, err := nullableScalar(f.Username, "username")
	if err != nil {
		return in, err
	}
	in.Username = username
	password, err := nullableScalar(f.Password, "password")
	if err != nil {
		return in, err
	}
	in.Password = password
	enabled, err := requiredScalar(f.Enabled, "enabled")
	if err != nil {
		return in, err
	}
	in.Enabled = enabled
	remark, err := nullableScalar(f.Remark, "remark")
	if err != nil {
		return in, err
	}
	in.Remark = remark
	return in, nil
}

// toAPIProxy domain → 线格式。三条不可逆的投影决定：
//
//  1. **password 恒不出**（spec §3.3）：读模型里没有这个键，只回 has_password；
//  2. username 非密、原样回显；空 = 无认证 ⇒ null（而不是空串），让前端用一处
//     判据（present/absent）区分「有认证」与「无认证」；
//  3. last_probe_at / last_probe_error 的 never-probed / 无错误在 domain 侧是
//     nil / ""，线格式统一折成 null（与 §6.1 读模型的可空字段一致）。
//  4. **owner_uid 只读回显**（§3.6）：nil 原样落成 null（= 平台代理）。管理员靠它看出
//     这条属于谁；供应商面看到的恒是自己的 uid（不构成泄露——它看不到别人的条目）。
func toAPIProxy(p domain.Proxy) Proxy {
	out := Proxy{
		Id:          p.ID,
		Name:        p.Name,
		Scheme:      ProxyScheme(p.Scheme),
		Host:        p.Host,
		Port:        p.Port,
		HasPassword: p.Password != "",
		Enabled:     p.Enabled,
		OwnerUid:    p.OwnerUID,
		Remark:      p.Remark,
		LastProbeAt: p.LastProbeAt,
		LastProbeOk: p.LastProbeOK,
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.UpdatedAt,
	}
	if p.Username != "" {
		username := p.Username
		out.Username = &username
	}
	if p.LastProbeError != "" {
		probeErr := p.LastProbeError
		out.LastProbeError = &probeErr
	}
	return out
}

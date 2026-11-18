// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
)

func loginProvider(raw *DeviceLoginProvider) domain.DeviceLoginProvider {
	if raw == nil || *raw == "" {
		return domain.DeviceLoginCodex
	}
	return domain.DeviceLoginProvider(*raw)
}

// PostAccountsCodexDeviceLogin is the shared ChatGPT/BPS device-login start.
// An empty body remains valid for backwards-compatible Codex clients.
func (h *AdminAPI) PostAccountsCodexDeviceLogin(w http.ResponseWriter, r *http.Request) {
	var in DeviceLoginStartBody
	if r.ContentLength != 0 {
		if err := httpface.Decode(r, &in); err != nil && !errors.Is(err, io.EOF) {
			httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
	}
	ch, err := h.svc.StartDeviceLogin(r.Context(), loginProvider(in.Provider))
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, CodexDeviceLoginStart{
		DeviceAuthId: ch.DeviceAuthID, UserCode: ch.UserCode,
		VerificationUri: ch.VerificationURL, IntervalSeconds: ch.IntervalSeconds,
		ExpiresInSeconds: ch.ExpiresInSeconds,
	})
}

// PostAccountsCodexDeviceLoginComplete performs exactly one manual completion.
func (h *AdminAPI) PostAccountsCodexDeviceLoginComplete(w http.ResponseWriter, r *http.Request) {
	body, err := httpface.DecodeRaw(r)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	var in CodexDeviceLoginCompleteBody
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = errors.New("unexpected trailing data after JSON body")
		}
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	var profile []byte
	if raw, ok := fields["bps_profile"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		profile = append([]byte(nil), raw...)
	}
	res, err := h.svc.CompleteDeviceLogin(r.Context(), loginProvider(in.Provider), in.DeviceAuthId, in.UserCode, in.TemplateId, in.GroupId, profile)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := CodexDeviceLoginResult{Status: CodexDeviceLoginResultStatus(res.Status)}
	if res.AccountID > 0 {
		id, imported := res.AccountID, res.Imported
		out.AccountId, out.Imported = &id, &imported
		out.AccountName, out.Email, out.CodexAccountId = optionalStr(res.AccountName), optionalStr(res.Email), optionalStr(res.CodexAccountID)
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}

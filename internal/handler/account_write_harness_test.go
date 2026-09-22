// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

func newIfMatchRouter(h *AdminAPI) http.Handler {
	r := chi.NewRouter()
	r.Mount("/", h.Router())
	return r
}

func doPatch(r http.Handler, id, body, ifMatch string, setHeader bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/"+id, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if setHeader {
		req.Header.Set("If-Match", ifMatch)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// enabledFilterHarness enabled 过滤测试装配（create + list 直调）。
type enabledFilterHarness struct {
	do func(method, path, body string) *httptest.ResponseRecorder
}

func newEnabledFilterRouter(t *testing.T) (*enabledFilterHarness, *fakeStore, *hProber) {
	t.Helper()
	store := newFakeStore()
	store.tpls[1] = &domain.Template{ID: 1, Name: "tpl", CredentialType: "api_key"}
	prober := &hProber{}
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil,
		service.ServiceDeps{EmailCodeStore: store, RecoverProber: prober})
	h := New(svc)
	r := chi.NewRouter()
	r.Mount("/", h.Router())
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	return &enabledFilterHarness{do: do}, store, prober
}

func serviceForDefaults(t *testing.T) *service.Service {
	t.Helper()
	store := newFakeStore()
	store.tpls[1] = &domain.Template{ID: 1, Name: "tpl", CredentialType: "api_key"}
	return service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil,
		service.ServiceDeps{EmailCodeStore: store, DefaultMaxConcurrency: 7})
}

func createDefaultAccount(svc *service.Service) (*domain.Account, error) {
	name := "def"
	tplID := int64(1)
	key := "sk-def"
	return svc.CreateAccount(context.Background(), repository.AccountPatch{
		Name: &name, TemplateID: &tplID, UpstreamKey: &key,
	})
}

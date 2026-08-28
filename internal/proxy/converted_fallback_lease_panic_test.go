// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestConvertedFallbackPanicReleasesLease(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIResponses}, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp})

	orig := convertRequest
	convertRequest = func(_ []byte, _ domain.ProtocolConvert) ([]byte, error) {
		panic("injected panic during conversion after real fallback Select")
	}
	defer func() { convertRequest = orig }()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()

	require.Panics(t, func() { p.HandleChat(rec, req) })

	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, int64(0), ri.Concurrency, "converted fallback panic must release exact sel2 lease once")
	require.GreaterOrEqual(t, ri.Concurrency, int64(0))

	// Liveness: next request succeeds after restore.
	convertRequest = orig
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Authorization", "Bearer ck-1")
	rec2 := httptest.NewRecorder()
	p.HandleChat(rec2, req2)
	require.Equal(t, 200, rec2.Code)
	ri, _ = p.sched.Runtime(1)
	require.Equal(t, int64(0), ri.Concurrency)
}

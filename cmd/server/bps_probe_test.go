// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/pkg/egress"
)

// TestBPSAdapterSingleInstance pins the shared-instance wiring: the only
// sdkbridge.NewBPS call lives in the billing stage, the codex stage consumes
// the injected instance, and the probe adapter is built in billing.
func TestBPSAdapterSingleInstance(t *testing.T) {
	for file, want := range map[string]int{
		"wiring_billing.go": 1,
		"wiring_codex.go":   0,
		"main.go":           0,
	} {
		src, err := os.ReadFile(file)
		require.NoError(t, err)
		require.Equal(t, want, strings.Count(string(src), "sdkbridge.NewBPS("), file)
	}
	billing, err := os.ReadFile("wiring_billing.go")
	require.NoError(t, err)
	require.Contains(t, string(billing), "bpsProbe := &bpsProber{bps: bpsAdapter")
	require.Contains(t, string(billing), "BPSProbe:           bpsProbe")
	codex, err := os.ReadFile("wiring_codex.go")
	require.NoError(t, err)
	require.Contains(t, string(codex), "bps           *sdkbridge.BPS")
	require.Contains(t, string(codex), "bpsAdapter := d.bps")
	mainSrc, err := os.ReadFile("main.go")
	require.NoError(t, err)
	require.Contains(t, string(mainSrc), "bps: bl.bps")
}

// TestBPSProberMapsPrepareFatalToInvalidInput pins the adapter's fixed-400
// mapping for a stored credential the SDK rejects at construction.
func TestBPSProberMapsPrepareFatalToInvalidInput(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = rdb.Close() })
	adapter := sdkbridge.NewBPS(rdb)
	t.Cleanup(func() { _ = adapter.Close() })
	profile, err := sdkbridge.NewBPSLogin(nil).GenerateProfile()
	require.NoError(t, err)
	p := &bpsProber{bps: adapter, pool: egress.NewPool(http.DefaultClient)}

	_, err = p.ProbeAccount(ctx, service.BPSAccountProbeRequest{
		Credential: &domain.AccountCredential{
			AccountID: 1, CredentialType: credential.TypeBPS, IdentityRevision: 1,
			CodexAccountID: "acct", OAuthToken: "not-a-jwt", OAuthRefreshToken: "rt", BPSProfile: profile,
		},
		Format: domain.FormatOpenAIResponses, UpstreamModel: "m", Input: "x",
	})
	require.ErrorIs(t, err, service.ErrInvalidInput)
	require.Contains(t, err.Error(), "BPS probe credential invalid")
	require.NotContains(t, err.Error(), "malformed JWT")
}

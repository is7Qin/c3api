// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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

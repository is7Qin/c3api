// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package pricing

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestParsePriceEntryBasic(t *testing.T) {
	jsonStr := `{
  "gpt-4o": {
    "input_cost_per_token": 2.5e-06,
    "output_cost_per_token": 1e-05,
    "mode": "chat",
    "litellm_provider": "openai"
  },
  "gpt-image-1": {
    "input_cost_per_image_token": 1e-05,
    "output_cost_per_image": 0.02,
    "mode": "image_generation"
  },
  "search-m": {
    "input_cost_per_query": 0.0001,
    "mode": "search"
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Equal(t, 3, len(res.PriceEntries))
	require.Equal(t, 0, res.Skipped)
	byModel := map[string]*domain.PriceEntry{}
	for _, e := range res.PriceEntries {
		byModel[e.Model] = e
	}
	require.Equal(t, domain.PriceModeToken, byModel["gpt-4o"].Mode)
	require.NotNil(t, byModel["gpt-4o"].InputPerM)
	require.Equal(t, domain.PriceModeImage, byModel["gpt-image-1"].Mode)
	require.Equal(t, domain.PriceModeCall, byModel["search-m"].Mode)
}

func TestParsePriceEntryVariants(t *testing.T) {
	jsonStr := `{
  "gpt-5": {
    "input_cost_per_token": 2e-06,
    "output_cost_per_token": 8e-06,
    "input_cost_per_token_priority": 3e-06,
    "output_cost_per_token_priority": 1.2e-05,
    "mode": "chat",
    "provider_specific_entry": {"fast": 2.0}
  }
}`
	res, err := Parse([]byte(jsonStr), nil)
	require.NoError(t, err)
	require.Equal(t, 1, len(res.PriceEntries))
	require.Equal(t, 2, len(res.Variants))
	// priority and fast variants
	foundPriority := false
	foundFast := false
	for _, v := range res.Variants {
		if v.ServiceTier != nil && *v.ServiceTier == "priority" {
			foundPriority = true
		}
		if v.ServiceTier != nil && *v.ServiceTier == "fast" {
			foundFast = true
		}
	}
	require.True(t, foundPriority)
	require.True(t, foundFast)
}

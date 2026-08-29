// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import "github.com/is7qin/c3api/internal/quality"

// AdaptOutcomeToObservation maps a canonical AttemptOutcome onto a
// quality Observation. The outcome must pass Validate and IsCountedForQuality;
// cancel/local/reservation and invalid outcomes are excluded (ok=false).
// Failures — malformed included, i.e. post-commit failures — are classified by
// HTTP status: 429 / ordinary 4xx / 5xx / status-0 network.
func AdaptOutcomeToObservation(o AttemptOutcome) (quality.Observation, bool) {
	if o.Validate() != nil {
		return quality.Observation{}, false
	}
	if !o.IsCountedForQuality() {
		return quality.Observation{}, false
	}
	obs := quality.Observation{
		InputTokens:         o.Usage.InputTokens,
		OutputTokens:        o.Usage.OutputTokens,
		CacheReadTokens:     o.Usage.CacheReadTokens,
		CacheCreationTokens: o.Usage.CacheCreationTokens,
		Tokens:              o.Usage.InputTokens + o.Usage.OutputTokens + o.Usage.CacheReadTokens + o.Usage.CacheCreationTokens,
		Calls:               o.Usage.CallCount,
	}
	if o.Result == ResultSuccess {
		obs.Success = true
		obs.TTFTMs = o.Timing.TTFTMS
		obs.ErrClass = quality.ErrClassNone
		return obs, true
	}
	switch {
	case o.HTTPStatus == 429:
		obs.ErrClass = quality.ErrClass429
	case o.HTTPStatus >= 400 && o.HTTPStatus <= 499:
		obs.ErrClass = quality.ErrClass4xx
	case o.HTTPStatus >= 500 && o.HTTPStatus <= 599:
		obs.ErrClass = quality.ErrClass5xx
	default:
		obs.ErrClass = quality.ErrClassNetwork
	}
	return obs, true
}

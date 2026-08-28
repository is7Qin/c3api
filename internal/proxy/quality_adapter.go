// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import "github.com/is7qin/c3api/internal/quality"

func AdaptOutcomeToObservation(o AttemptOutcome) (quality.Observation, bool) {
	if o.IsMalformed {
		obs := quality.Observation{
			Success:             false,
			ErrClass:            quality.ErrClass4xx,
			InputTokens:         o.Usage.InputTokens,
			OutputTokens:        o.Usage.OutputTokens,
			CacheReadTokens:     o.Usage.CacheReadTokens,
			CacheCreationTokens: o.Usage.CacheCreationTokens,
			Tokens:              o.Usage.InputTokens + o.Usage.OutputTokens + o.Usage.CacheReadTokens + o.Usage.CacheCreationTokens,
			Calls:               o.Usage.CallCount,
			IsMalformed:         true,
		}
		return obs, true
	}
	if o.Result == ResultClientCancel {
		return quality.Observation{}, false
	}
	if o.Result == ResultLocalReject {
		return quality.Observation{}, false
	}
	if o.Result == ResultReservationReject {
		return quality.Observation{}, false
	}
	if o.Result == ResultUnknown {
		return quality.Observation{}, false
	}
	if !o.IsDispatched() {
		return quality.Observation{}, false
	}
	obs := quality.Observation{
		InputTokens:         o.Usage.InputTokens,
		OutputTokens:        o.Usage.OutputTokens,
		CacheReadTokens:     o.Usage.CacheReadTokens,
		CacheCreationTokens: o.Usage.CacheCreationTokens,
		Tokens:              o.Usage.InputTokens + o.Usage.OutputTokens + o.Usage.CacheReadTokens + o.Usage.CacheCreationTokens,
		Calls:               o.Usage.CallCount,
		Images:              0,
	}
	if o.Result == ResultSuccess {
		obs.Success = true
		obs.TTFTMs = o.Timing.TTFTMS
		obs.ErrClass = quality.ErrClassNone
		return obs, true
	}
	obs.Success = false
	obs.TTFTMs = nil
	switch {
	case o.HTTPStatus == 429:
		obs.ErrClass = quality.ErrClass429
	case o.HTTPStatus >= 500 && o.HTTPStatus <= 599:
		obs.ErrClass = quality.ErrClass5xx
	case o.HTTPStatus == 0:
		obs.ErrClass = quality.ErrClassNetwork
	default:
		obs.ErrClass = quality.ErrClass4xx
	}
	return obs, true
}

func AdaptOutcomeToObservationWithCancel(o AttemptOutcome) quality.Observation {
	if o.Result == ResultClientCancel {
		return quality.Observation{IsCancel: true}
	}
	if o.Result == ResultLocalReject {
		return quality.Observation{IsLocal: true}
	}
	if o.Result == ResultReservationReject {
		return quality.Observation{IsReservation: true}
	}
	if o.IsMalformed {
		return quality.Observation{IsMalformed: true, Success: false, ErrClass: quality.ErrClass4xx, InputTokens: o.Usage.InputTokens, OutputTokens: o.Usage.OutputTokens, CacheReadTokens: o.Usage.CacheReadTokens, CacheCreationTokens: o.Usage.CacheCreationTokens, Tokens: o.Usage.InputTokens + o.Usage.OutputTokens + o.Usage.CacheReadTokens + o.Usage.CacheCreationTokens, Calls: o.Usage.CallCount}
	}
	obs, ok := AdaptOutcomeToObservation(o)
	if !ok {
		return quality.Observation{IsCancel: true}
	}
	return obs
}

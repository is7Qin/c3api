// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	bpssdk "github.com/is7Qin/bps-sdk"

	"github.com/is7qin/c3api/internal/domain"
)

// BPSLogin adapts the SDK's one-shot device authorization flow.
type BPSLogin struct{ device *bpssdk.DeviceAuth }

func NewBPSLogin(client *http.Client) *BPSLogin {
	return &BPSLogin{device: bpssdk.NewDeviceAuth("", client)}
}

// GenerateProfile delegates complete DeviceProfile generation to the SDK.
// The profile is created server-side and returned only to the service import path.
func (*BPSLogin) GenerateProfile() ([]byte, error) {
	profile, err := bpssdk.GenerateDesktopDeviceProfile()
	if err != nil {
		return nil, err
	}
	return json.Marshal(profile)
}

func (l *BPSLogin) StartDeviceFlow(ctx context.Context) (*domain.CodexDeviceCode, error) {
	code, err := l.device.Start(ctx)
	if err != nil {
		return nil, err
	}
	return &domain.CodexDeviceCode{
		AuthID: code.DeviceAuthID, UserCode: code.UserCode,
		VerificationURL: code.VerificationURL,
		Interval:        code.Interval, ExpiresIn: max(0, time.Until(code.ExpiresAt)),
	}, nil
}

func (l *BPSLogin) PollDeviceFlow(ctx context.Context, authID, userCode string) (domain.CodexLoginStatus, *domain.CodexLoginTokens, error) {
	tokens, err := l.device.Poll(ctx, &bpssdk.DeviceCode{DeviceAuthID: authID, UserCode: userCode})
	if err != nil {
		if errors.Is(err, bpssdk.ErrDeviceAuthPending) {
			return domain.CodexLoginPending, nil, nil
		}
		if errors.Is(err, bpssdk.ErrDeviceAuthExpired) {
			return domain.CodexLoginExpired, nil, nil
		}
		var authErr *bpssdk.DeviceAuthError
		if errors.As(err, &authErr) {
			switch strings.ToLower(strings.TrimSpace(authErr.Code)) {
			case "deviceauth_user_code_expired":
				return domain.CodexLoginExpired, nil, nil
			case "access_denied", "authorization_declined", "authorization_denied", "authorization_failed":
				return domain.CodexLoginDenied, nil, nil
			}
			if authErr.Status == http.StatusGone {
				return domain.CodexLoginExpired, nil, nil
			}
		}
		return "", nil, err
	}
	// Keep account claim checks in the SDK, including the case where device
	// exchange cannot derive the upstream identity from the access token.
	if _, err := bpssdk.NewStaticAuth(tokens.AccessToken, tokens.AccountID, ""); err != nil {
		return "", nil, err
	}
	return domain.CodexLoginAuthorized, &domain.CodexLoginTokens{
		AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken,
		AccountID: tokens.AccountID, Email: tokens.Email, ExpiresAt: expiresAtOrNil(tokens.ExpiresAt),
	}, nil
}

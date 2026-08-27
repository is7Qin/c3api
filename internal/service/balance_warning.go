// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"
	"math"
	"time"

	mail "github.com/wneessen/go-mail"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/pkg/logx"
)

// SetBalanceWarningCooldownCleaner injects the Redis-backed known-key cleanup
// used only after a successful preference change. Nil leaves cleanup disabled.
func (s *Service) SetBalanceWarningCooldownCleaner(clear func(context.Context, int64, int64) error) {
	s.clearBalanceWarningCooldown = clear
}

func (s *Service) UpdateBalanceWarningThreshold(ctx context.Context, userID int64, thresholdUSD float64) (*domain.User, error) {
	if math.IsNaN(thresholdUSD) || math.IsInf(thresholdUSD, 0) {
		return nil, ErrInvalidInput
	}
	if thresholdUSD < 0 {
		return nil, ErrInvalidInput
	}
	if thresholdUSD > float64(math.MaxInt64)/1e5 {
		return nil, ErrInvalidInput
	}
	millis := int64(math.Round(thresholdUSD * 1e5))
	if thresholdUSD > 0 && millis == 0 {
		return nil, ErrInvalidInput
	}
	if millis < 0 {
		return nil, ErrInvalidInput
	}
	updated, previousThreshold, err := s.store.UpdateUserBalanceWarningThreshold(ctx, userID, millis)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	s.inv.Users()
	s.publish(ctx, notify.Change{Users: true})
	if previousThreshold > 0 && previousThreshold != millis && s.clearBalanceWarningCooldown != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 200*time.Millisecond)
		err := s.clearBalanceWarningCooldown(cleanupCtx, userID, previousThreshold)
		cancel()
		if err != nil && s.log != nil {
			s.log.Warn("balance warning cooldown cleanup failed", logx.Int64("user_id", userID))
		}
	}
	return updated, nil
}

func (s *Service) GetBalanceWarningThreshold(ctx context.Context, userID int64) (int64, error) {
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return 0, mapRepoErr(err)
	}
	return u.BalanceWarningThreshold, nil
}

func (s *Service) BalanceWarningEnabled() bool {
	v := s.settingValue("balance_warning.enabled")
	if v == "" {
		if def := domain.DefaultSetting("balance_warning.enabled"); def != nil {
			return def.Value == "true"
		}
		return true
	}
	return v == "true"
}

func (s *Service) MailConfig() (host string, port int, username, password, fromAddr, tlsPolicy string, ok bool) {
	return s.mailConfig()
}

func (s *Service) SendMailChannelTest(ctx context.Context, toEmail string) error {
	if !validEmail(toEmail) {
		return ErrInvalidInput
	}
	host, port, username, password, fromAddr, tlsPolicy, ok := s.mailConfig()
	if !ok {
		return ErrMailNotConfigured
	}
	subj := fmt.Sprintf("%s email channel test", domain.AppName)
	body := "This is a test email from c3api to verify SMTP configuration. If you received this, the email channel is working."
	opts := []mail.Option{
		mail.WithPort(port),
		mail.WithTimeout(15 * time.Second),
	}
	switch tlsPolicy {
	case "implicit":
		opts = append(opts, mail.WithSSL())
	case "none":
		opts = append(opts, mail.WithTLSPolicy(mail.NoTLS))
	default:
		opts = append(opts, mail.WithTLSPolicy(mail.TLSMandatory))
	}
	if username != "" {
		opts = append(opts, mail.WithSMTPAuth(mail.SMTPAuthPlain), mail.WithUsername(username), mail.WithPassword(password))
	}
	client, err := mail.NewClient(host, opts...)
	if err != nil {
		return ErrMailChannelTestFailed
	}
	msg := mail.NewMsg()
	if err := msg.From(fromAddr); err != nil {
		return ErrMailChannelTestFailed
	}
	if err := msg.To(toEmail); err != nil {
		return ErrMailChannelTestFailed
	}
	msg.Subject(subj)
	msg.SetBodyString(mail.TypeTextPlain, body)
	sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := client.DialAndSendWithContext(sendCtx, msg); err != nil {
		return ErrMailChannelTestFailed
	}
	return nil
}

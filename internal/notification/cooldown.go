// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package notification

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/is7qin/c3api/internal/domain"
)

const (
	cooldownTTL    = 24 * time.Hour
	cooldownPrefix = "c3api:notification:balance-warning:email"
)

var compareDeleteLua = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
else
  return 0
end
`)

// Cooldown provides atomic per-channel threshold cooldown via Redis.
// Key = c3api:notification:balance-warning:email:<userID>:<thresholdMillis>
// Value = random token, NX EX 86400. Only the token owner may delete.
type Cooldown struct {
	client *redis.Client
}

// NewCooldown constructs a cooldown helper. client must be non-nil (Redis
// is a required dependency; nil panics to surface wiring errors early).
func NewCooldown(client *redis.Client) *Cooldown {
	if client == nil {
		panic("notification: NewCooldown(nil): Redis is required")
	}
	return &Cooldown{client: client}
}

func cooldownKey(event domain.BalanceWarningEvent) string {
	return cooldownKeyFor(event.EntityID, event.ThresholdMillis)
}

func cooldownKeyFor(userID, thresholdMillis int64) string {
	return fmt.Sprintf("%s:%d:%d", cooldownPrefix, userID, thresholdMillis)
}

// TryClaim attempts to atomically claim the cooldown slot. It returns
// (token, true, nil) on success (caller owns the claim), (\"\", false, nil)
// when another claimant holds it, or an error on Redis failure.
func (c *Cooldown) TryClaim(ctx context.Context, event domain.BalanceWarningEvent) (string, bool, error) {
	token, err := newToken()
	if err != nil {
		return "", false, errCooldown
	}
	key := cooldownKey(event)
	ok, err := c.client.SetNX(ctx, key, token, cooldownTTL).Result()
	if err != nil {
		return "", false, errCooldown
	}
	if !ok {
		return "", false, nil
	}
	return token, true, nil
}

// Release atomically deletes the key only if the stored value equals token.
// Uses Lua compare-delete to avoid GET->DEL race.
func (c *Cooldown) Release(ctx context.Context, event domain.BalanceWarningEvent, token string) error {
	if token == "" {
		return nil
	}
	key := cooldownKey(event)
	_, err := compareDeleteLua.Run(ctx, c.client, []string{key}, token).Result()
	if err != nil && err != redis.Nil {
		return errCooldown
	}
	return nil
}

// Clear deletes a known retired threshold claim regardless of token ownership.
// Preference updates intentionally retire the entire old warning cycle.
func (c *Cooldown) Clear(ctx context.Context, userID, thresholdMillis int64) error {
	if err := c.client.Del(ctx, cooldownKeyFor(userID, thresholdMillis)).Err(); err != nil {
		return errCooldown
	}
	return nil
}

func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

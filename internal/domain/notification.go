// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

// NotificationEventType identifies a notification event contract.
type NotificationEventType string

// NotificationBalanceWarningCrossed is emitted for a committed threshold crossing.
const NotificationBalanceWarningCrossed NotificationEventType = "balance_warning_crossed"

// NotificationEntityType identifies the entity carried by a notification event.
type NotificationEntityType string

// NotificationUser identifies a user notification subject.
const NotificationUser NotificationEntityType = "user"

// BalanceWarningEvent snapshots a committed permanent-balance threshold crossing.
type BalanceWarningEvent struct {
	EventType       NotificationEventType
	EntityType      NotificationEntityType
	EntityID        int64
	BalanceMillis   int64
	ThresholdMillis int64
	Email           string
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package notification

import "errors"

const (
	failureCooldownClaim     = "cooldown_claim_failed"
	failureCooldownRelease   = "cooldown_release_failed"
	failureMailEnqueue       = "mail_enqueue_failed"
	failureMailEnqueuePanic  = "mail_enqueue_panicked"
	failureMailDelivery      = "mail_delivery_failed"
	failureNotificationDrain = "notification_drain_panicked"
)

var (
	errCooldown                  = errors.New("cooldown_error")
	errMailEnqueuePanicked       = errors.New(failureMailEnqueuePanic)
	errNotificationDrainPanicked = errors.New(failureNotificationDrain)
)

// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

export type DeviceLoginProvider = 'codex' | 'bps'

export function deviceLoginProvider(credentialType?: string): DeviceLoginProvider | null {
  if (credentialType === 'codex-oauth') return 'codex'
  if (credentialType === 'bps') return 'bps'
  return null
}

export type DeviceLoginCompleteFields = { provider?: DeviceLoginProvider }

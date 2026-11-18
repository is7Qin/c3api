// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

export type DeviceLoginProvider = 'codex' | 'bps'

export function deviceLoginProvider(credentialType?: string): DeviceLoginProvider | null {
  if (credentialType === 'codex-oauth') return 'codex'
  if (credentialType === 'bps') return 'bps'
  return null
}

export type DeviceLoginCompleteFields =
  | { provider?: 'codex'; bps_profile?: never }
  | { provider: 'bps'; bps_profile: Record<string, unknown> }

export function deviceLoginCompleteFields(provider: DeviceLoginProvider, profileText: string): DeviceLoginCompleteFields | null {
  if (provider === 'codex') return {}
  // Keep the SDK profile opaque. The backend must validate the complete profile
  // before checking the grant. Do not echo parse errors: they can include input.
  try {
    const value: unknown = JSON.parse(profileText)
    if (value && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length > 0) {
      return { provider: 'bps', bps_profile: value as Record<string, unknown> }
    }
  } catch {
    return null
  }
  return null
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import { deviceLoginCompleteFields, deviceLoginProvider } from './device-login.ts'

test('device login is enabled only for a BPS or Codex OAuth template', () => {
  assert.equal(deviceLoginProvider('bps'), 'bps')
  assert.equal(deviceLoginProvider('codex-oauth'), 'codex')
  for (const credentialType of [undefined, 'codex-pat', 'api_key', 'responses-special', 'custom']) {
    assert.equal(deviceLoginProvider(credentialType), null)
  }
})

test('BPS must send an explicit object; no default or schema projection is applied', () => {
  for (const text of ['', '{', 'null', '[]', '"profile"', '{}', '7']) {
    assert.equal(deviceLoginCompleteFields('bps', text), null)
  }
  const profile = { schema_version: 1, browser: { name: 'edge' }, future_field: ['opaque'] }
  assert.deepEqual(deviceLoginCompleteFields('bps', JSON.stringify(profile)), { provider: 'bps', bps_profile: profile })
  assert.deepEqual(deviceLoginCompleteFields('codex', JSON.stringify(profile)), {})
})

test('login uses one scoped start and complete call, both clicked, without browser storage', () => {
  const dialog = readFileSync(new URL('../components/codex-import/login-dialog.tsx', import.meta.url), 'utf8')
  assert.match(dialog, /const api = useScopedApi\(\)/)
  assert.match(dialog, /const start = async \(\) => \{[\s\S]*?setTarget\(selectedTarget\)[\s\S]*?await api\.startCodexDeviceLogin\(/)
  assert.match(dialog, /const complete = async \(\) => \{[\s\S]*?deviceLoginCompleteFields\(target\.provider, profileText\)[\s\S]*?await api\.completeCodexDeviceLogin\(\{[\s\S]*?template_id: target\.templateId/)
  for (const name of ['start', 'complete']) {
    assert.equal(dialog.match(new RegExp(`onClick=\\{${name}\\}`, 'g'))?.length, 1)
  }
  assert.equal(dialog.match(/api\.(?:start|complete)CodexDeviceLogin\(/g)?.length, 2)
  assert.doesNotMatch(dialog, /\b(?:setInterval|localStorage|sessionStorage|indexedDB)\b/)
})

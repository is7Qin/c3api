// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import { deviceLoginProvider } from './device-login.ts'

test('device login is enabled only for a BPS or Codex OAuth template', () => {
  assert.equal(deviceLoginProvider('bps'), 'bps')
  assert.equal(deviceLoginProvider('codex-oauth'), 'codex')
  for (const credentialType of [undefined, 'codex-pat', 'api_key', 'responses-special', 'custom']) {
    assert.equal(deviceLoginProvider(credentialType), null)
  }
})

test('BPS login uses the same completion shape; profile is server-generated', () => {
  assert.equal(deviceLoginProvider('bps'), 'bps')
})

test('login uses one scoped start and complete call, both clicked, without browser storage', () => {
  const dialog = readFileSync(new URL('../components/codex-import/login-dialog.tsx', import.meta.url), 'utf8')
  assert.match(dialog, /const api = useScopedApi\(\)/)
  assert.match(dialog, /const start = async \(\) => \{[\s\S]*?setTarget\(selectedTarget\)[\s\S]*?await api\.startCodexDeviceLogin\(/)
  assert.match(dialog, /const complete = async \(\) => \{[\s\S]*?await api\.completeCodexDeviceLogin\(\{[\s\S]*?template_id: target\.templateId/)
  assert.doesNotMatch(dialog, /bps_profile|profileText|Textarea/)
  for (const name of ['start', 'complete']) {
    assert.equal(dialog.match(new RegExp(`onClick=\\{${name}\\}`, 'g'))?.length, 1)
  }
  assert.equal(dialog.match(/api\.(?:start|complete)CodexDeviceLogin\(/g)?.length, 2)
  assert.doesNotMatch(dialog, /\b(?:setInterval|localStorage|sessionStorage|indexedDB)\b/)
})

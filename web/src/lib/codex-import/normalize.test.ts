import assert from 'node:assert/strict'
import test from 'node:test'
import { normalizeRow } from './normalize.ts'

function token(accountId: string) {
  const payload = Buffer.from(JSON.stringify({
    'https://api.openai.com/auth': { chatgpt_account_id: accountId },
  })).toString('base64url')
  return `header.${payload}.sig`
}

test('derives a missing OAuth account id from access_token', () => {
  const row = normalizeRow({
    email: 'user@example.com',
    access_token: token('acc-1'),
    refresh_token: 'refresh-1',
  }, 'codex-oauth', 0)
  assert.equal(row.error, undefined)
  assert.equal(row.item && 'codex_account_id' in row.item && row.item.codex_account_id, 'acc-1')
})

test('keeps an explicit account id ahead of the token claim', () => {
  const row = normalizeRow({
    email: 'user@example.com',
    account_id: 'explicit-1',
    access_token: token('acc-1'),
    refresh_token: 'refresh-1',
  }, 'codex-oauth', 0)
  assert.equal(row.item && 'codex_account_id' in row.item && row.item.codex_account_id, 'explicit-1')
})

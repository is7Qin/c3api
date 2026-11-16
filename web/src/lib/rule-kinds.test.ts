// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 规则页事件类型与后端 rule.Kind 的线格式对齐（回归：前端漏了 timeout）。
//
// 后端 ValidateWhen 接受 ok/429/4xx/5xx/network/timeout，种子规则里也有
// kind=timeout；前端 KINDS 少一项时，页面既建不出 timeout 规则，列表里那条
// 种子规则还会显示原始 i18n key。timeout 的 status 恒 0、且不进窗口桶
// （rule/windows.go 只记 ok/failure/t429）：状态码条件必须隐藏；窗口/总次数是
// 账号级全事件总量，仍保留（timeout 只是不贡献计数）。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const read = (rel: string) => readFileSync(new URL(rel, import.meta.url), 'utf8')
const page = read('../pages/rules.tsx')
const zh = read('../locales/zh.ts')
const en = read('../locales/en.ts')

describe('rules.tsx：timeout 事件类型', () => {
  it('KINDS 与后端 rule.Kind 线格式一致（含 timeout）', () => {
    assert.ok(page.includes("const KINDS = ['ok', '429', '4xx', '5xx', 'network', 'timeout'] as const"))
  })

  it('timeout 不提供错误信息/状态码条件（固定文案与 status=0 都会造成死配置）', () => {
    // timeout 的 ErrorMessage 是固定网关文案（上游在预算内未产出），按原文匹配恒假。
    assert.ok(page.includes("key: 'error_message_contains', kinds: ['4xx', '5xx', 'network']"))
    assert.ok(page.includes("key: 'error_message_contains_in', kinds: ['4xx', '5xx', 'network']"))
    // timeout 的事件 status 恒 0：状态码条件永不命中。
    assert.ok(page.includes("const STATUS_KINDS = ['ok', '429', '4xx', '5xx', 'network'] as const"))
    for (const key of ['http_status', 'http_status_in']) {
      assert.ok(page.includes(`key: '${key}', kinds: STATUS_KINDS`), `${key} 必须排除 timeout`)
    }
  })

  it('窗口/总次数条件对 timeout 保留（total 是账号级全事件总量）', () => {
    for (const key of ['window_seconds', 'count_total_ge']) {
      assert.ok(page.includes(`key: '${key}', kinds: ['any']`), `${key} 不应排除 timeout`)
    }
  })

  it('i18n zh/en 都有 rules.kind.timeout（列表不再显示原始 key）', () => {
    assert.ok(zh.includes('"timeout": "超时（上游零产出）"'))
    assert.ok(en.includes('"timeout": "Timeout (no upstream output)"'))
  })
})

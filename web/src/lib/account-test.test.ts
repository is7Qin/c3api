// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { formatSupports, probeFormats, probeModelsFor } from './account-test.ts'
import type { ProbeTemplate } from './account-test.ts'

const template = (overrides: Partial<ProbeTemplate> = {}): ProbeTemplate => ({
  SupportedFormats: ['openai-chat'],
  Models: [],
  FormatModels: {},
  ModelMapping: {},
  ...overrides,
})

const mapping = { alias: { mapped_model: 'upstream-alias', mode: 'explicit' as const } }

describe('probeFormats', () => {
  it('只返回模板支持格式与四种可探测格式的交集', () => {
    const tpl = template({ SupportedFormats: ['openai-images', 'anthropic', 'openai-chat', 'openai-search'] })
    assert.deepEqual(probeFormats(tpl), ['openai-chat', 'anthropic'])
    assert.deepEqual(probeFormats(template({ SupportedFormats: ['openai-images'] })), [])
  })
})

describe('probeModelsFor', () => {
  it('Models 为空时仍从 format_models 产生候选', () => {
    const tpl = template({
      SupportedFormats: ['openai-responses'],
      Models: [],
      FormatModels: { 'openai-responses': ['gpt-5.2', 'gpt-5.1'] },
    })
    assert.deepEqual(probeModelsFor(tpl, 'openai-responses'), { models: ['gpt-5.1', 'gpt-5.2'], wildcard: false })
  })

  it('mapping-only 模板把映射别名纳入候选', () => {
    const tpl = template({ ModelMapping: mapping })
    assert.deepEqual(probeModelsFor(tpl, 'openai-chat'), { models: ['alias'], wildcard: false })
  })

  it('格式模型表已配置时排除该格式禁止的 Models 与映射模型', () => {
    const tpl = template({
      SupportedFormats: ['openai-chat', 'anthropic'],
      Models: ['shared', 'chat-only'],
      FormatModels: { 'openai-chat': ['chat-only'], anthropic: ['shared', 'alias'] },
      ModelMapping: mapping,
    })
    assert.deepEqual(probeModelsFor(tpl, 'openai-chat').models, ['chat-only'])
    assert.deepEqual(probeModelsFor(tpl, 'anthropic').models, ['alias', 'shared'])
    assert.equal(formatSupports(tpl, 'openai-chat', 'shared'), false)
    assert.equal(formatSupports(tpl, 'openai-chat', 'alias'), false)
  })

  it('未配置格式模型表时合并完整模型空间并去重排序', () => {
    const tpl = template({
      Models: ['shared', 'z-model'],
      FormatModels: { anthropic: ['a-model', 'shared'] },
      ModelMapping: mapping,
    })
    assert.deepEqual(probeModelsFor(tpl, 'openai-chat').models, ['a-model', 'alias', 'shared', 'z-model'])
    assert.deepEqual(probeModelsFor(tpl, 'openai-responses').models, [])
  })

  it('Models、所有格式模型列表与映射全空时才是通配模型空间', () => {
    const wildcard = template({ FormatModels: { 'openai-chat': [], anthropic: [] } })
    assert.deepEqual(probeModelsFor(wildcard, 'openai-chat'), { models: [], wildcard: true })
    assert.equal(probeModelsFor(template({ Models: ['one'] }), 'openai-chat').wildcard, false)
    assert.equal(probeModelsFor(template({ FormatModels: { anthropic: ['one'] } }), 'openai-chat').wildcard, false)
    assert.equal(probeModelsFor(template({ ModelMapping: mapping }), 'openai-chat').wildcard, false)
  })
})

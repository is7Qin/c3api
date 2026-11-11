// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { formatSupports, probeComboboxValue, probeFormats, probeModelsFor } from './account-test.ts'
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

  it('Models、FormatModels map 与映射全空时才是通配模型空间；只含空列表的 map 不是', () => {
    const wildcard = template({ Models: [], FormatModels: {}, ModelMapping: {} })
    assert.deepEqual(probeModelsFor(wildcard, 'openai-chat'), { models: [], wildcard: true })
    // 后端 HasModelSpace 以 map 键存在与否为准：{format: []} 已配置，不是通配。
    const emptyLists = template({ FormatModels: { 'openai-chat': [], anthropic: [] } })
    assert.deepEqual(probeModelsFor(emptyLists, 'openai-chat'), { models: [], wildcard: false })
    assert.equal(probeModelsFor(template({ Models: ['one'] }), 'openai-chat').wildcard, false)
    assert.equal(probeModelsFor(template({ FormatModels: { anthropic: ['one'] } }), 'openai-chat').wildcard, false)
    assert.equal(probeModelsFor(template({ ModelMapping: mapping }), 'openai-chat').wildcard, false)
  })
})

describe('probeComboboxValue', () => {
  it('通配模板的自由输入不在候选中时仍作为受控值保留', () => {
    const wildcard = template({ Models: [], FormatModels: {}, ModelMapping: {} })
    const candidates = probeModelsFor(wildcard, 'openai-chat')
    assert.equal(candidates.wildcard, true)
    assert.deepEqual(candidates.models, [])
    // 旧实现把“不在候选内”映射成 null：关闭/清空 Combobox 会连输入一起清掉。
    assert.equal(candidates.models.includes('free-model') ? 'free-model' : null, null)
    assert.equal(probeComboboxValue('free-model'), 'free-model')
    assert.equal(probeComboboxValue(''), null)
  })

  it('候选内选择与显式清空语义不变', () => {
    assert.equal(probeComboboxValue('chat-only'), 'chat-only')
    assert.equal(probeComboboxValue('   '), '   ')
    assert.equal(probeComboboxValue(''), null)
  })
})

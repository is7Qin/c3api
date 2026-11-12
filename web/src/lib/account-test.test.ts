// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import {
  buildProbeRequest,
  formatSupports,
  PROBE_MAX_IMAGE_URL_BYTES,
  PROBE_MAX_INPUT_BYTES,
  probeComboboxValue,
  probeImageURLShapeValid,
  probeInputByteLength,
  probeModelsFor,
  probeTargetFormat,
  probeTargetImageMode,
  probeTargets,
} from './account-test.ts'
import type { ProbeTemplate } from './account-test.ts'

const template = (overrides: Partial<ProbeTemplate> = {}): ProbeTemplate => ({
  SupportedFormats: ['openai-chat'],
  Models: [],
  FormatModels: {},
  ModelMapping: {},
  ...overrides,
})

const mapping = { alias: { mapped_model: 'upstream-alias', mode: 'explicit' as const } }

describe('probeTargets', () => {
  it('7 个 UI 目标按模板 wire format 过滤；images 两 mode 各一条', () => {
    const tpl = template({ SupportedFormats: ['openai-images', 'anthropic', 'openai-chat', 'openai-search'] })
    assert.deepEqual(probeTargets(tpl), [
      'openai-chat',
      'anthropic',
      'openai-images.generations',
      'openai-images.edits',
      'openai-search',
    ])
    assert.deepEqual(probeTargets(template({ SupportedFormats: ['openai-images'] })), [
      'openai-images.generations',
      'openai-images.edits',
    ])
    assert.deepEqual(probeTargets(template({ SupportedFormats: ['openai-search'] })), ['openai-search'])
  })

  it('UI 目标映射到 wire format + image_mode', () => {
    assert.equal(probeTargetFormat('openai-images.generations'), 'openai-images')
    assert.equal(probeTargetFormat('openai-images.edits'), 'openai-images')
    assert.equal(probeTargetFormat('openai-search'), 'openai-search')
    assert.equal(probeTargetImageMode('openai-images.generations'), 'generations')
    assert.equal(probeTargetImageMode('openai-images.edits'), 'edits')
    assert.equal(probeTargetImageMode('openai-search'), undefined)
    assert.equal(probeTargetImageMode('openai-chat'), undefined)
  })
})

describe('probeInputByteLength / probeImageURLShapeValid', () => {
  it('input 字节数按 trim 后的 UTF-8 计算，与 service 同口径', () => {
    assert.equal(probeInputByteLength('  你好  '), 6)
    assert.equal(probeInputByteLength('   '), 0)
    assert.ok(PROBE_MAX_INPUT_BYTES === 8192)
  })

  it('接受 http(s) 与 data:image/...;base64，拒绝其他 scheme/空 data', () => {
    assert.equal(probeImageURLShapeValid('https://example.com/a.png'), true)
    assert.equal(probeImageURLShapeValid('HTTP://example.com/a.png'), true)
    assert.equal(probeImageURLShapeValid('data:image/png;base64,AAAA'), true)
    assert.equal(probeImageURLShapeValid('DATA:IMAGE/PNG;BASE64,AAAA'), true)
    assert.equal(probeImageURLShapeValid('data:image/png,AAAA'), false)
    assert.equal(probeImageURLShapeValid('data:text/plain;base64,AAAA'), false)
    assert.equal(probeImageURLShapeValid('data:image/;base64,AAAA'), false)
    assert.equal(probeImageURLShapeValid('ftp://example.com/a.png'), false)
    assert.equal(probeImageURLShapeValid('/relative.png'), false)
    assert.equal(probeImageURLShapeValid(''), false)
    assert.ok(PROBE_MAX_IMAGE_URL_BYTES === 6 * 1024 * 1024)
  })
})

describe('buildProbeRequest', () => {
  const base = { target: 'openai-chat' as const, model: '  m1  ', input: '', imageURL: '' }

  it('空白 input 不发送，非空先 trim', () => {
    assert.deepEqual(buildProbeRequest(base), { model: 'm1', format: 'openai-chat' })
    assert.deepEqual(buildProbeRequest({ ...base, input: '  你好  ' }), {
      model: 'm1',
      format: 'openai-chat',
      input: '你好',
    })
  })

  it('非图片目标忽略 imageURL；generations 发送 image_mode 但不发送 image_url', () => {
    assert.deepEqual(buildProbeRequest({ ...base, imageURL: 'https://example.com/a.png' }), {
      model: 'm1',
      format: 'openai-chat',
    })
    assert.deepEqual(buildProbeRequest({
      ...base,
      target: 'openai-images.generations',
      imageURL: 'https://example.com/a.png',
    }), {
      model: 'm1',
      format: 'openai-images',
      image_mode: 'generations',
    })
  })

  it('images edits 携带 trim 后的 image_url 与 image_mode', () => {
    assert.deepEqual(buildProbeRequest({
      ...base,
      target: 'openai-images.edits',
      imageURL: '  data:image/png;base64,AAAA  ',
    }), {
      model: 'm1',
      format: 'openai-images',
      image_mode: 'edits',
      image_url: 'data:image/png;base64,AAAA',
    })
  })

  it('search 目标只映射 format', () => {
    assert.deepEqual(buildProbeRequest({ ...base, target: 'openai-search', input: 'news', imageURL: 'data:image/png;base64,AAAA' }), {
      model: 'm1',
      format: 'openai-search',
      input: 'news',
    })
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

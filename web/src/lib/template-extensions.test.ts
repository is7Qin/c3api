// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// custom 路径保存门禁的纯逻辑用例：固定状态注入，不触网、不渲染。
// 运行：`node --test`（Node 内置运行器原生跑 TS，零新依赖）。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import {
  CUSTOM_PATH_FIELDS,
  extFormFields,
  nonEmptyPaths,
  pathsEchoBlocked,
} from './template-extensions.ts'

describe('extFormFields', () => {
  it('有 ext 行 → 回显路径与图像开关', () => {
    assert.deepEqual(
      extFormFields({
        strip_image_tools: true,
        upstream_paths: { anthropic: 'v1/messages', 'openai-chat': 'chat/completions' },
      }),
      {
        strip_image_tools: true,
        upstream_paths: { anthropic: 'v1/messages', 'openai-chat': 'chat/completions' },
      },
    )
  })

  it('无 ext 行 → 空表（该模板确实没有配置，可以标记已同步）', () => {
    assert.deepEqual(
      extFormFields(null),
      { strip_image_tools: false, upstream_paths: {} },
    )
  })

  it('没读到（undefined）→ 不产出任何值，调用方不得据此清空表单', () => {
    assert.equal(extFormFields(undefined), null)
  })

  // 回归：只用管理 API 建过的 custom 模板没有 ext 行（GET 404 → null）。把
  // 「无行」与「没读到」当成同一件事，会让这类模板永远同步不上 ⇒ 门禁永久拦截。
  it('custom 模板没有 ext 行时仍视为已回显（可保存）', () => {
    const fields = extFormFields(null)
    assert.notEqual(fields, null)
    assert.deepEqual(fields?.upstream_paths, {})
    assert.equal(fields?.strip_image_tools, false)
  })

  it('存在 ext 行但字段全为 null → 未配置', () => {
    assert.deepEqual(
      extFormFields({ upstream_paths: null, strip_image_tools: null }),
      { strip_image_tools: false, upstream_paths: {} },
    )
  })

  it('返回副本：改表单不会写回缓存对象', () => {
    const cached = { upstream_paths: { anthropic: 'v1/messages' } }
    const fields = extFormFields(cached)!
    fields.upstream_paths.anthropic = 'edited'
    assert.equal(cached.upstream_paths.anthropic, 'v1/messages')
  })
})

describe('pathsEchoBlocked', () => {
  it('新建模板：没有既有配置可清空 → 不拦', () => {
    assert.equal(pathsEchoBlocked('custom', null, null), false)
  })

  it('编辑 custom 且尚未同步过该模板 → 拦', () => {
    assert.equal(pathsEchoBlocked('custom', 7, null), true)
    // 同步过别的模板不算（切模板后表单已被重置）
    assert.equal(pathsEchoBlocked('custom', 7, 9), true)
  })

  it('编辑 custom 且已按该模板同步 → 放行', () => {
    assert.equal(pathsEchoBlocked('custom', 7, 7), false)
  })

  it('api_key / 生态三类型不写路径 → 不拦', () => {
    for (const ct of ['api_key', 'responses-special', 'codex-oauth', 'codex-pat']) {
      assert.equal(pathsEchoBlocked(ct, 7, null), false, ct)
    }
  })

  // 回归：关闭再打开同一模板时 react-query 复用同一份 data 引用，回显 effect
  // 不会重跑，而 openEdit 已把表单重置成空表。若门禁只看查询状态（isLoading），
  // 这里会放行并清空 7 条路径。
  it('重开同一模板但表单被重置 → 仍拦（门禁看同步状态，不看查询状态）', () => {
    const reopeningSameTemplate = { editingId: 7, syncedFor: null }
    assert.equal(pathsEchoBlocked('custom', reopeningSameTemplate.editingId, reopeningSameTemplate.syncedFor), true)
  })
})

describe('nonEmptyPaths', () => {
  it('留空/纯空白的键不上送', () => {
    assert.equal(nonEmptyPaths({}), undefined)
    assert.deepEqual(
      nonEmptyPaths({ anthropic: ' v1/messages ', 'openai-chat': '   ' }),
      { anthropic: 'v1/messages' },
    )
  })

  it('全部留空 → undefined（后端 nil = 全部回落默认端点）', () => {
    assert.equal(nonEmptyPaths({ 'openai-chat': '', anthropic: ' ' }), undefined)
  })

  it('query 原样保留', () => {
    assert.deepEqual(
      nonEmptyPaths({ 'openai-responses': 'responses?api-version=2024-10-21' }),
      { 'openai-responses': 'responses?api-version=2024-10-21' },
    )
  })
})

describe('CUSTOM_PATH_FIELDS', () => {
  it('7 个键与后端键集合一致且唯一', () => {
    const keys = CUSTOM_PATH_FIELDS.map(f => f.key)
    assert.deepEqual(keys, [
      'openai-chat',
      'openai-responses',
      'openai-responses-ws',
      'anthropic',
      'openai-search',
      'openai-images.generations',
      'openai-images.edits',
    ])
    assert.equal(new Set(keys).size, keys.length)
  })
})

// 接线层 tripwire：本项目没有 jsdom / 渲染测试，纯函数覆盖不到「页面有没有按这些
// 规则接线」。这三条检查直接读页面源码——它们不是行为测试，只守最容易静默回归的
// 三处接线（重开清标记、effect 依赖对话框状态、门禁在任何写之前）。故意写得窄：
// 改动页面结构时会失败，提醒作者确认语义而不是删掉测试。
describe('templates 页面接线 tripwire（源码形状）', () => {
  const page = readFileSync(new URL('../pages/templates.tsx', import.meta.url), 'utf8')

  it('openEdit / openCreate 都会清掉「已同步」标记', () => {
    for (const handler of ['const openCreate = () => {', 'const openEdit = (t: Template) => {']) {
      const re = new RegExp(
        handler.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '[\\s\\S]*?setPathsSyncedFor\\(null\\)[\\s\\S]*?\\n  \\}',
      )
      assert.match(page, re, `${handler} 必须 setPathsSyncedFor(null)，否则重置后的空表单会被当成已回显`)
    }
  })

  it('回显 effect 通过 shouldSyncEcho 判定，且依赖含 dialogOpen', () => {
    assert.match(page, /shouldSyncEcho\(\{[^}]*dialogOpen[^}]*\}\)/,
      'effect 判据必须走 shouldSyncEcho（含 dialogOpen），否则重开不会重灌表单')
    assert.match(page, /\[editingID, dialogOpen, templateExtEcho\.isSuccess, templateExtEcho\.data\]/,
      'effect 依赖必须含 dialogOpen，否则缓存命中时不会重跑')
  })

  it('门禁在写模板之前执行', () => {
    const guard = page.indexOf('echoBlocked(ct,')
    const write = page.indexOf('api.updateTemplate(')
    assert.notEqual(guard, -1, '保存前必须有 echoBlocked 门禁')
    assert.notEqual(write, -1, '找不到 api.updateTemplate')
    assert.ok(guard < write, '门禁必须在 updateTemplate 之前，否则会「模板已改、却报没保存」')
  })
})

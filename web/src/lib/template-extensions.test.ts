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
  saveTargetID,
  shouldSyncEcho,
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

describe('shouldSyncEcho', () => {
  it('只有「有模板 + 对话框打开 + 读取成功」三者同时成立才回显', () => {
    assert.equal(shouldSyncEcho({ editingId: 7, dialogOpen: true, extReadOk: true }), true)
    assert.equal(shouldSyncEcho({ editingId: null, dialogOpen: true, extReadOk: true }), false, '新建无 ext')
    assert.equal(shouldSyncEcho({ editingId: 7, dialogOpen: false, extReadOk: true }), false, '关闭时不必灌表单')
    assert.equal(shouldSyncEcho({ editingId: 7, dialogOpen: true, extReadOk: false }), false, '读失败不得回显')
  })

  // 回归：关掉再打开同一模板要重灌一次表单（dialogOpen 是触发器，见页面 effect）。
  it('重开（dialogOpen false → true）在缓存命中时仍会触发回显', () => {
    const closed = shouldSyncEcho({ editingId: 7, dialogOpen: false, extReadOk: true })
    const reopened = shouldSyncEcho({ editingId: 7, dialogOpen: true, extReadOk: true })
    assert.equal(closed, false)
    assert.equal(reopened, true)
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

describe('saveTargetID', () => {
  // 回归：模板与 ext 是两次调用，ext 写失败时模板行已存在。若重试仍判定为
  // 「新建」，会留孤儿模板并再造一个。
  it('ext 写失败后的重试落在同一个模板上（update 而非再建）', () => {
    assert.equal(saveTargetID(null, 42), 42, '新建后 ext 失败 → 重试 update 42')
    assert.equal(saveTargetID(7, 42), 7, '正在编辑的模板优先')
  })

  it('真正的新建返回 null（只有这时才允许 create）', () => {
    assert.equal(saveTargetID(null, null), null)
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

  // 门禁：placeholder 是「留空时该格式默认端点」的照抄示例，因此必须是相对尾段
  // ——不带主机/协议/前导斜杠。前缀不受约束：custom 路径原样拼到 base_url 之后，
  // 带不带 v1/ 由用户自己决定，页面不做规范要求。
  it('placeholder 是可直接照抄的相对尾段', () => {
    assert.equal(CUSTOM_PATH_FIELDS.length, 7, '7 个键必须全部覆盖')
    for (const f of CUSTOM_PATH_FIELDS) {
      assert.ok(f.placeholder.length > 0, `${f.key} 的 placeholder 不能为空`)
      assert.ok(!f.placeholder.startsWith('/'), `${f.key} 的 placeholder ${f.placeholder} 不应带前导斜杠`)
      assert.ok(!f.placeholder.includes('://'), `${f.key} 的 placeholder ${f.placeholder} 不应含协议`)
    }
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

describe('templates 页面接线 tripwire（保存侧）', () => {
  const page = readFileSync(new URL('../pages/templates.tsx', import.meta.url), 'utf8')

  it('保存目标由 saveTargetID 决定，且创建成功后立刻记住 pending id', () => {
    assert.match(page, /let id = saveTargetID\(editing\?\.ID \?\? null, pendingTemplateID\)/,
      '目标 id 必须走 saveTargetID，否则 ext 写失败后的重试会再建一个模板')
    const create = page.indexOf('api.createTemplate(')
    const remember = page.indexOf('setPendingTemplateID(id)')
    assert.notEqual(create, -1, '找不到 api.createTemplate')
    assert.notEqual(remember, -1, '创建成功后必须 setPendingTemplateID(id)')
    assert.ok(create < remember, '必须在 createTemplate 之后立刻记住 id（ext 写失败也要记住）')
  })

})

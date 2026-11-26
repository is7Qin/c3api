// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// ext（template_ext）保存门禁与写面组装的纯逻辑用例：固定状态注入，不触网、不渲染。
// 运行：`node --test`（Node 内置运行器原生跑 TS，零新依赖）。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import {
  CUSTOM_PATH_FIELDS,
  TRANSPORT_DOWNGRADE_CONFIRM,
  emptyRequestPatchForm,
  extEchoBlocked,
  extFormFields,
  extWriteBody,
  extWriteConfirm,
  isEmptyRequestPatch,
  isCodexCredentialType,
  nonEmptyPaths,
  requestPatchFromForm,
  requestPatchToForm,
  saveTargetID,
  shouldSyncEcho,
} from './template-extensions.ts'

describe('extFormFields', () => {
  it('有 ext 行 → 回显路径、图像开关与出站传输', () => {
    assert.deepEqual(
      extFormFields({
        strip_image_tools: true,
        upstream_paths: { anthropic: 'v1/messages', 'openai-chat': 'chat/completions' },
        codex_responses_transport: 'ws',
      }),
      {
        strip_image_tools: true,
        upstream_paths: { anthropic: 'v1/messages', 'openai-chat': 'chat/completions' },
        codex_responses_transport: 'ws',
        request_patch: emptyRequestPatchForm(),
      },
    )
  })

  it('无 ext 行 → 空表（该模板确实没有配置，可以标记已同步）', () => {
    assert.deepEqual(
      extFormFields(null),
      {
        strip_image_tools: false,
        upstream_paths: {},
        codex_responses_transport: '',
        request_patch: emptyRequestPatchForm(),
      },
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
      extFormFields({ upstream_paths: null, strip_image_tools: null, codex_responses_transport: null }),
      {
        strip_image_tools: false,
        upstream_paths: {},
        codex_responses_transport: '',
        request_patch: emptyRequestPatchForm(),
      },
    )
  })

  // "" 本身就是有意义的取值（默认面），不能与「未读取」合并成同一态。
  it('显式 "" 与"未配置"回显同一个字符串（不做额外归一）', () => {
    assert.equal(extFormFields({ codex_responses_transport: '' })?.codex_responses_transport, '')
    assert.equal(extFormFields({ codex_responses_transport: 'http' })?.codex_responses_transport, 'http')
  })

  it('返回副本：改表单不会写回缓存对象', () => {
    const cached = { upstream_paths: { anthropic: 'v1/messages' } }
    const fields = extFormFields(cached)!
    fields.upstream_paths.anthropic = 'edited'
    assert.equal(cached.upstream_paths.anthropic, 'v1/messages')
  })
})

describe('extEchoBlocked', () => {
  it('新建模板：没有既有配置可清空 → 不拦', () => {
    assert.equal(extEchoBlocked('custom', null, null), false)
    assert.equal(extEchoBlocked('codex-oauth', null, null), false)
  })

  it('编辑 custom 且尚未同步过该模板 → 拦', () => {
    assert.equal(extEchoBlocked('custom', 7, null), true)
    // 同步过别的模板不算（切模板后表单已被重置）
    assert.equal(extEchoBlocked('custom', 7, 9), true)
  })

  it('编辑 custom 且已按该模板同步 → 放行', () => {
    assert.equal(extEchoBlocked('custom', 7, 7), false)
  })

  // Responses 原生 provider（codex/bps）走同一个门禁：写面是全列替换，
  // 没回显就保存会把 ws 静默清回默认。
  it('编辑 codex/bps 且尚未同步 → 拦（否则一次改名就把 ws 清回默认）', () => {
    for (const ct of ['codex-oauth', 'codex-pat', 'bps']) {
      assert.equal(extEchoBlocked(ct, 7, null), true, ct)
      assert.equal(extEchoBlocked(ct, 7, 9), true, ct)
      assert.equal(extEchoBlocked(ct, 7, 7), false, ct)
    }
  })

  it('api_key / responses-special 不写 ext 字段 → 不拦', () => {
    for (const ct of ['api_key', 'responses-special']) {
      assert.equal(extEchoBlocked(ct, 7, null), false, ct)
    }
  })

  // 回归：关闭再打开同一模板时 react-query 复用同一份 data 引用，回显 effect
  // 不会重跑，而 openEdit 已把表单重置成空表。若门禁只看查询状态（isLoading），
  // 这里会放行并清空 7 条路径。
  it('重开同一模板但表单被重置 → 仍拦（门禁看同步状态，不看查询状态）', () => {
    const reopeningSameTemplate = { editingId: 7, syncedFor: null }
    assert.equal(extEchoBlocked('custom', reopeningSameTemplate.editingId, reopeningSameTemplate.syncedFor), true)
  })
})

describe('extWriteBody', () => {
  const requestPatch = {
    headers: { 'User-Agent': 'vibe-code/1.0' },
    formats: { anthropic: { set: { 'metadata.user_id': 'vibe_code' }, delete: ['metadata.client_field'] } },
  }
  const fields = {
    strip_image_tools: true,
    upstream_paths: { anthropic: 'v1/messages', 'openai-chat': '  ' },
    codex_responses_transport: 'ws',
    request_patch: requestPatch,
  }

  it('codex：三字段一起发（全列替换）', () => {
    assert.deepEqual(extWriteBody(fields, 'codex-oauth'), {
      credential_type: 'codex-oauth',
      strip_image_tools: true,
      codex_responses_transport: 'ws',
    })
  })

  it('custom：带归一后的路径 + request_patch，不带传输字段', () => {
    assert.deepEqual(extWriteBody(fields, 'custom'), {
      credential_type: 'custom',
      strip_image_tools: true,
      upstream_paths: { anthropic: 'v1/messages' },
      request_patch: requestPatch,
    })
  })

  it('自定义类型省略 request_patch（= 清空；非 custom 提交非空会被 400）', () => {
    for (const ct of ['responses-special', 'codex-oauth', 'codex-pat', 'bps', 'api_key']) {
      const body = extWriteBody(fields, ct) as Record<string, unknown>
      assert.equal('request_patch' in body, false, ct)
    }
  })

  it('custom 的空 request_patch 省略（全空 = 未配置 = 清空，不发送 {} ）', () => {
    const body = extWriteBody({ ...fields, request_patch: {} }, 'custom') as Record<string, unknown>
    assert.equal('request_patch' in body, false)
  })

  // 非 codex 不发送该字段：陈旧行（模板类型改掉后残留 ws）原样带回会被判 400。
  it('非 Responses 原生类型不发送传输字段（陈旧行原样带回会被判 400）', () => {
    for (const ct of ['responses-special', 'custom', 'api_key']) {
      const body = extWriteBody(fields, ct) as Record<string, unknown>
      assert.equal('codex_responses_transport' in body, false, ct)
    }
  })

  it('bps：复用同一传输字段（与 codex 同住 template_ext）', () => {
    assert.deepEqual(extWriteBody(fields, 'bps'), {
      credential_type: 'bps',
      strip_image_tools: true,
      codex_responses_transport: 'ws',
    })
  })

  it('codex 的 "" 原样上送（未配置 = 默认 http，也是全列替换的一次清空）', () => {
    assert.equal(
      extWriteBody({ ...fields, codex_responses_transport: '' }, 'codex-pat').codex_responses_transport,
      '',
    )
  })

  it('strip_image_tools 的三态透传（对话框能表达「未配置」）', () => {
    assert.equal(extWriteBody({ ...fields, strip_image_tools: null }, 'codex-oauth').strip_image_tools, null)
  })
})

describe('extWriteConfirm', () => {
  it('未确认 → 不带 confirm（无条件带会让服务端 409 门形同虚设）', () => {
    assert.equal(extWriteConfirm(false), undefined)
  })

  it('确认后 → 带 transport-downgrade 令牌', () => {
    assert.deepEqual(extWriteConfirm(true), { confirm: TRANSPORT_DOWNGRADE_CONFIRM })
    assert.equal(TRANSPORT_DOWNGRADE_CONFIRM, 'transport-downgrade')
  })
})

describe('isCodexCredentialType', () => {
  it('只有 codex-oauth / codex-pat 是 codex 类型', () => {
    assert.equal(isCodexCredentialType('codex-oauth'), true)
    assert.equal(isCodexCredentialType('codex-pat'), true)
    for (const ct of ['api_key', 'responses-special', 'custom', '']) {
      assert.equal(isCodexCredentialType(ct), false, ct)
    }
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
// 规则接线」。这些检查直接读页面源码——它们不是行为测试，只守最容易静默回归的
// 接线（重开清标记、effect 依赖对话框状态、门禁在任何写之前、两处写面共用组装点、
// confirm 只在 409 分支出现）。故意写得窄：改动页面结构时会失败，提醒作者确认
// 语义而不是删掉测试。
describe('templates 页面接线 tripwire（源码形状）', () => {
  const page = readFileSync(new URL('../pages/templates.tsx', import.meta.url), 'utf8')

  it('openEdit / openCreate 都会清掉「已同步」标记', () => {
    for (const handler of ['const openCreate = () => {', 'const openEdit = (t: Template) => {']) {
      const re = new RegExp(
        handler.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '[\\s\\S]*?setExtSyncedFor\\(null\\)[\\s\\S]*?\\n  \\}',
      )
      assert.match(page, re, `${handler} 必须 setExtSyncedFor(null)，否则重置后的空表单会被当成已回显`)
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

  it('两处写面共用 extWriteBody（对话框 + 主表单链式写）', () => {
    const uses = page.match(/extWriteBody\(/g) ?? []
    assert.equal(uses.length, 2, 'extWriteBody 必须被两处写面使用（多一处＝又造了一个组装点）')
  })

  it('confirm 只在 409 分支后出现（禁止无条件带）', () => {
    // 决策必须走 extWriteConfirm，且它的入参来自用户确认标记
    assert.match(page, /extWriteConfirm\(vars\.confirmed\)/,
      'confirm 决策必须走 extWriteConfirm(vars.confirmed)')
    // 页面里不得硬编码 confirm 令牌（拼 URL / 传字面量都会绕过 409 门）
    assert.equal(page.includes('transport-downgrade'), false,
      '页面不得硬编码 confirm 令牌（令牌与「是否带」的决策都在 lib 里）')
    assert.match(page, /status === 409/, '409 必须被显式识别并弹二次确认')
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

// 扩展配置入口必须对 BPS 可见：出站传输住 template_ext，按钮却曾按
// ECO_CREDENTIAL_TYPES（codex 三类）门控，BPS 模板根本打不开弹窗。
describe('templates 页：扩展配置入口', () => {
  const page = readFileSync(new URL('../pages/templates.tsx', import.meta.url), 'utf8')
  it('按钮与主表单链式写都按「有 ext 行」判定（api_key 除外）', () => {
    assert.match(page, /const hasTemplateExt = .*credentialType !== 'api_key'/)
    assert.match(page, /\{hasTemplateExt\(t\.CredentialType as string\) && \(/)
    assert.match(page, /if \(id && hasTemplateExt\(ct\)\) \{/)
  })
  it('BPS 弹窗不渲染 strip_image_tools（该开关只属于 codex/responses-special）', () => {
    assert.match(page, /!isBPSCredential\(\(extTarget\?\.CredentialType as string\) \?\? ''\)/)
  })
})
// —— request_patch（custom 出站请求修饰）的纯逻辑 ——
// 全列替换语义：回显链路任何一环漏掉 request_patch，保存一次就会把已有配置清空。
describe('request_patch 回显与写面', () => {
  const wire = {
    headers: { Authorization: 'Bearer {api_key}', 'X-Drop': '' },
    formats: {
      'openai-chat': {
        headers: { 'User-Agent': 'ua' },
        // 2^53-1 是 JS Number 能精确表示的最大整数；更大的整数只能由 API 精确维护
        // （浏览器 JSON.parse 会先经过 float64，见管理 API/P1 报告已知边界）。
        set: { 'metadata.call_source': 'vibe_code', big: 9007199254740991, nested: { b: 2, a: 1 }, nul: null },
        delete: ['metadata.client_field'],
      },
      anthropic: { set: { 'metadata.user_id': 'x' } },
    },
  }

  it('wire → 行状态 → wire 往返不丢字段（set 值以 JSON 文本编辑）', () => {
    const form = requestPatchToForm(wire)
    assert.deepEqual(form.headers, [
      { name: 'Authorization', value: 'Bearer {api_key}' },
      { name: 'X-Drop', value: '' },
    ])
    const chat = form.formats['openai-chat']
    assert.deepEqual(chat.headers, [{ name: 'User-Agent', value: 'ua' }])
    assert.deepEqual(chat.delete, ['metadata.client_field'])
    assert.equal(chat.set.find(r => r.path === 'big')?.value, '9007199254740991')
    assert.equal(chat.set.find(r => r.path === 'nul')?.value, 'null')
    const round = requestPatchFromForm(form)
    assert.equal(round.invalid, false)
    assert.deepEqual(round.patch, wire)
  })

  it('三态：undefined 与 null 都得到空表（调用方用 undefined 区分「没读到」）', () => {
    assert.deepEqual(requestPatchToForm(undefined), emptyRequestPatchForm())
    assert.deepEqual(requestPatchToForm(null), emptyRequestPatchForm())
    assert.deepEqual(requestPatchToForm({ headers: {} }), emptyRequestPatchForm())
  })

  it('深拷贝：改表单不写回 echo 缓存', () => {
    const cached = {
      headers: { 'X-A': '1' },
      formats: { anthropic: { set: { 'a.b': 1 }, delete: ['c'] } },
    }
    const form = requestPatchToForm(cached)
    form.headers[0].value = 'edited'
    form.formats['anthropic'].set[0].value = '2'
    form.formats['anthropic'].delete.push('d')
    assert.equal(cached.headers['X-A'], '1')
    assert.deepEqual(cached.formats['anthropic'].set, { 'a.b': 1 })
    assert.deepEqual(cached.formats['anthropic'].delete, ['c'])
  })

  it('空行忽略；空 set 值/非法 JSON 标记 invalid（不静默丢弃）', () => {
    const form = requestPatchToForm(null)
    form.headers.push({ name: '  ', value: 'x' })
    form.headers.push({ name: ' X-A ', value: '' })
    form.formats['openai-chat'] = {
      headers: [{ name: '  ', value: 'v' }],
      set: [
        { path: 'a.b', value: '' },
        { path: 'c.d', value: '{bad' },
        { path: 'e.f', value: '{"ok":true}' },
      ],
      delete: ['', '  ', ' g.h '],
    }
    const out = requestPatchFromForm(form)
    assert.equal(out.invalid, true, '空 set 值 / 非法 JSON 必须被标记（调用方拦住保存）')
    assert.deepEqual(out.patch, {
      headers: { 'X-A': '' },
      formats: { 'openai-chat': { set: { 'e.f': { ok: true } }, delete: ['g.h'] } },
    })
  })

  it('全空表单 ⇒ patch undefined（上层省略字段 = 清空）', () => {
    const out = requestPatchFromForm(emptyRequestPatchForm())
    assert.equal(out.patch, undefined)
    assert.equal(out.invalid, false)
  })

  it('isEmptyRequestPatch：空值头（Del）是有效配置，不是空 patch', () => {
    assert.equal(isEmptyRequestPatch(undefined), true)
    assert.equal(isEmptyRequestPatch({}), true)
    assert.equal(isEmptyRequestPatch({ headers: { 'X-A': '' } }), false)
    assert.equal(isEmptyRequestPatch({ formats: { anthropic: { delete: ['a'] } } }), false)
  })
})

// 接线 tripwire：全列替换下两个写面都必须把 request_patch 带回（漏一个就清空）。
describe('templates 页：request_patch 两个写面', () => {
  const page = readFileSync(new URL('../pages/templates.tsx', import.meta.url), 'utf8')

  it('扩展对话框：回显原样带回 request_patch', () => {
    assert.match(page, /request_patch: extRequestPatch/)
    assert.match(page, /setExtRequestPatch\(extQ\.data\?\.request_patch \?\? \{\}\)/,
      '对话框回显必须记录 request_patch，否则保存一次就清空')
  })

  it('主表单链式写：行状态转 wire 后带回 request_patch', () => {
    assert.match(page, /requestPatchFromForm\(f\.request_patch\)/)
    assert.match(page, /request_patch: requestPatch\.patch \?\? \{\}/)
  })

  it('custom 主表单渲染请求修饰编辑器（全局头 + 按格式面板 + JSON 语法提示）', () => {
    assert.match(page, /templates\.ext\.requestPatchHeaders/)
    assert.match(page, /templates\.ext\.requestPatchSet/)
    assert.match(page, /const layer = patchFormat\(f\.key\)/)
  })
})

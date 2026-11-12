// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 支付账户表单的三态门禁（spec 支付网关 v2 §18-3 / §13 P52）回归。
//
// 事故形状（N49）：配置表单在 fields 还没到的时候用 `useState(() => initialConfig(...))`
// 求值，把 config 初始化成一串空串 → "每个字段都算改过" → 一次保存把密钥提交成空串；
// 空串 ≠ `***`，服务端不会当成"保持原值"，密钥就这么没了。
// 本文件的每个用例都在钉同一件事：**元数据没就绪时不存在"可提交的空配置"**。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import {
  SECRET_MASK,
  controlFor,
  fieldLabel,
  metaStateOf,
  providerOf,
  resolveFormState,
  unknownKindWarning,
  warnUnknownKinds,
  type ConfigField,
  type PaymentProviderInfo,
} from './payment-form.ts'

// 顺序刻意与"字母序"不同：schema 的顺序即表单顺序，前端不得排序（§18-2）。
const SCHEMA: ConfigField[] = [
  { name: 'base_url', kind: 'url', secret: false, required: true, example: 'https://pay.example.com' },
  { name: 'pid', kind: 'text', secret: false, required: true, example: '1001' },
  { name: 'private_key', kind: 'password', secret: true, required: true },
  { name: 'public_key', kind: 'multiline', secret: false, required: false },
]

const ACCOUNT = {
  config: {
    base_url: 'https://pay.example.com',
    pid: '1001',
    private_key: SECRET_MASK,
    public_key: '-----BEGIN PUBLIC KEY-----',
  },
}

// 「可提交的空串」：任何键的值是 ''。空串不会被 `***` 合并当成"保持原值"，是最危险的形状。
const emptyStrings = (config: Record<string, string>) =>
  Object.entries(config).filter(([, v]) => v === '').map(([k]) => k)

describe('resolveFormState：元数据三态（P52）', () => {
  it('loading：不可打开、不可提交，且 config 里一个键都没有', () => {
    const state = resolveFormState('loading', ACCOUNT, SCHEMA)
    assert.equal(state.openable, false)
    assert.equal(state.submittable, false)
    assert.deepEqual(state.fields, [])
    assert.deepEqual(state.config, {})
  })

  it('loading：账户已有值也不返回任何键（没有键就没有可提交的空串）', () => {
    const state = resolveFormState('loading', ACCOUNT, SCHEMA)
    for (const field of SCHEMA) {
      assert.equal(Object.hasOwn(state.config, field.name), false, `${field.name} 不得出现在 loading 态`)
    }
    assert.deepEqual(emptyStrings(state.config), [])
  })

  it('loading：新建态同样是空表（不是"每个字段都算改过"的初值）', () => {
    const state = resolveFormState('loading', null, SCHEMA)
    assert.equal(state.openable, false)
    assert.equal(state.submittable, false)
    assert.deepEqual(state.config, {})
  })

  it('failed：不可提交（列表照常可读由页面决定，纯函数这里一律关死）', () => {
    const state = resolveFormState('failed', ACCOUNT, SCHEMA)
    assert.equal(state.openable, false)
    assert.equal(state.submittable, false)
    assert.deepEqual(state.fields, [])
    assert.deepEqual(state.config, {})
  })

  it('ready：fields 来自 schema，顺序原样（不排序）', () => {
    const state = resolveFormState('ready', ACCOUNT, SCHEMA)
    assert.equal(state.openable, true)
    assert.equal(state.submittable, true)
    assert.deepEqual(state.fields.map(f => f.name), SCHEMA.map(f => f.name))
  })

  it('ready：fields 是副本，调用方改写不会污染入参', () => {
    const state = resolveFormState('ready', ACCOUNT, SCHEMA)
    state.fields.pop()
    assert.equal(SCHEMA.length, 4)
  })

  it('ready：secret 字段回落 ***，非 secret 字段取账户值', () => {
    const state = resolveFormState('ready', ACCOUNT, SCHEMA)
    assert.deepEqual(state.config, {
      base_url: 'https://pay.example.com',
      pid: '1001',
      private_key: SECRET_MASK,
      public_key: '-----BEGIN PUBLIC KEY-----',
    })
    assert.deepEqual(emptyStrings(state.config), [])
  })

  it('ready：secret 字段在账户里缺席或是空串时回落 ***（绝不留空串）', () => {
    for (const config of [{}, { private_key: '' }, { private_key: null }]) {
      const state = resolveFormState('ready', { config }, SCHEMA)
      assert.equal(state.config.private_key, SECRET_MASK, JSON.stringify(config))
    }
  })

  it('ready：非 secret 字段缺席/非字符串回落空串（与 secret 的 *** 语义不同）', () => {
    const state = resolveFormState('ready', { config: { pid: 1001 } }, SCHEMA)
    assert.equal(state.config.pid, '')
    assert.equal(state.config.base_url, '')
  })

  it('account === null（新建）：fields 来自 schema，初值全为空串（secret 也不预置 ***）', () => {
    const state = resolveFormState('ready', null, SCHEMA)
    assert.equal(state.openable, true)
    assert.deepEqual(state.fields.map(f => f.name), SCHEMA.map(f => f.name))
    assert.deepEqual(state.config, { base_url: '', pid: '', private_key: '', public_key: '' })
  })

  it('缺省第三参：ready 也只得到空字段表（不凭账户 config 猜字段）', () => {
    const state = resolveFormState('ready', ACCOUNT)
    assert.equal(state.openable, true)
    assert.deepEqual(state.fields, [])
    assert.deepEqual(state.config, {})
  })

  it('每次都是新对象（不存在被调用方共享改写的单例）', () => {
    const a = resolveFormState('loading', ACCOUNT, SCHEMA)
    const b = resolveFormState('loading', ACCOUNT, SCHEMA)
    assert.notEqual(a, b)
    assert.notEqual(a.config, b.config)
  })
})

describe('controlFor：kind → 控件（未知 kind 的 secret 感知降级，N43）', () => {
  it('已知 kind 直接映射', () => {
    assert.equal(controlFor({ name: 'a', kind: 'text' }), 'text')
    assert.equal(controlFor({ name: 'b', kind: 'url' }), 'text')
    assert.equal(controlFor({ name: 'c', kind: 'password', secret: true }), 'password')
    assert.equal(controlFor({ name: 'd', kind: 'multiline' }), 'multiline')
  })

  it('未知 kind + secret === true → password 控件（秘密字段绝不因"前端不认识"落成明文）', () => {
    assert.equal(controlFor({ name: 'otp', kind: 'otp', secret: true }), 'password')
    assert.equal(controlFor({ name: 'pin', kind: 'pin-pad', secret: true }), 'password')
  })

  it('未知 kind + secret === false → text 控件', () => {
    assert.equal(controlFor({ name: 'region', kind: 'select', secret: false }), 'text')
  })

  it('未知 kind 且未声明 secret → 按非秘密降级为 text', () => {
    assert.equal(controlFor({ name: 'region', kind: 'select' }), 'text')
  })
})

describe('unknownKindWarning / warnUnknownKinds：降级必须可观测（§18-7）', () => {
  it('已知 kind 不产生告警', () => {
    assert.equal(unknownKindWarning('epay_v2', { name: 'pid', kind: 'text' }), null)
    assert.equal(unknownKindWarning('epay_v2', { name: 'private_key', kind: 'password', secret: true }), null)
  })

  it('未知 kind 的文案含 provider 名与 kind 值', () => {
    const msg = unknownKindWarning('epay_v2', { name: 'otp', kind: 'otp', secret: true })
    assert.ok(msg !== null)
    assert.match(msg, /epay_v2/)
    assert.match(msg, /otp/)
    assert.match(msg, /password/)
  })

  it('warnUnknownKinds 走 console.warn，且同一 (provider, kind) 只发一次', () => {
    const seen: string[] = []
    const original = console.warn
    console.warn = (...args: unknown[]) => { seen.push(args.map(String).join(' ')) }
    try {
      const first = warnUnknownKinds('unit-test-provider', [
        { name: 'otp', kind: 'otp', secret: true },
        { name: 'otp2', kind: 'otp', secret: true },
        { name: 'pid', kind: 'text' },
      ])
      const second = warnUnknownKinds('unit-test-provider', [{ name: 'otp', kind: 'otp', secret: true }])
      assert.equal(first.length, 1, '同一 (provider, kind) 只该发一条')
      assert.equal(second.length, 0, '已告警过的组合不再重复')
    } finally {
      console.warn = original
    }
    assert.equal(seen.length, 1)
    assert.match(seen[0], /unit-test-provider/)
    assert.match(seen[0], /otp/)
  })
})

describe('fieldLabel：标签优先级（§18-3）', () => {
  // 模拟 i18next：命中返回译文，缺 key 时原样返回 key。
  const translate = (dict: Record<string, string>) => (key: string) => dict[key] ?? key

  it('i18n 命中 → 译文优先（高于协议自述 label）', () => {
    const t = translate({ 'paymentAccounts.field.pid': '商户号' })
    assert.equal(fieldLabel({ name: 'pid', kind: 'text', label: 'Merchant PID' }, t), '商户号')
  })

  it('i18n 未命中 → 协议 label 兜底', () => {
    const t = translate({})
    assert.equal(fieldLabel({ name: 'sk', kind: 'password', secret: true, label: 'Secret key' }, t), 'Secret key')
  })

  it('label 为空/纯空白 → 原始 name', () => {
    const t = translate({})
    assert.equal(fieldLabel({ name: 'sk', kind: 'password', secret: true, label: '   ' }, t), 'sk')
    assert.equal(fieldLabel({ name: 'sk', kind: 'password', secret: true }, t), 'sk')
  })

  it('label 前后空白被裁掉（服务端不做本地化，只做纯展示文本）', () => {
    assert.equal(fieldLabel({ name: 'sk', kind: 'text', label: '  Secret key  ' }, translate({})), 'Secret key')
  })
})

describe('metaStateOf / providerOf：元数据装配', () => {
  it('pending → loading（首次加载失败时 pending 与 isError 同时为真，取 loading）', () => {
    assert.equal(metaStateOf({ isPending: true, isError: false }), 'loading')
    assert.equal(metaStateOf({ isPending: true, isError: true }), 'loading')
  })

  it('isError → failed', () => {
    assert.equal(metaStateOf({ isPending: false, isError: true }), 'failed')
  })

  it('其余 → ready（空注册表也是 ready，只是没有可选协议）', () => {
    assert.equal(metaStateOf({ isPending: false, isError: false }), 'ready')
  })

  it('providerOf 按 name 命中；未注册/空名 → null（页面据此关掉表单，不回退镜像）', () => {
    const providers: PaymentProviderInfo[] = [
      { name: 'epay_v2', capabilities: ['alipay', 'wxpay'], config_schema: { fields: SCHEMA } },
    ]
    assert.equal(providerOf(providers, 'epay_v2')?.capabilities?.length, 2)
    assert.equal(providerOf(providers, 'nope'), null)
    assert.equal(providerOf(providers, ''), null)
    assert.equal(providerOf(providers, null), null)
  })
})

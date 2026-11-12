// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 前端协议镜像门禁（spec 支付网关 v2 §13 P50 的前端部分 / §18-5）。
//
// 为什么会漂移两次：管理面把协议的字段名/秘密标记/capabilities 抄一份副本后，
//   ①协议加字段 → 表单少一个框 → 严格 JSON（DisallowUnknownFields）下**必然 400**；
//   ②字段改判为秘密 → 前端仍用明文框回显 → **密钥进浏览器历史/自动补全**。
// 所以字段清单只能有一份（本文件的 ALLOWED 那一份，供 Go 侧取等号核对），
// 其余支付模块必须**只消费元数据**（`GET /payment/providers`）。
//
// 扫描面（§13 P50）：`pages/payment-*` + `lib/payment-*` + `components/payment-*`；
// 排除 codegen 产物 `lib/api/schema.d.ts`、`locales/`、本族测试文件（探测器自身不含断言对象），
// 以及上面那份被允许的字段清单文件。
import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync, readFileSync, readdirSync } from 'node:fs'
import { join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { PAYMENT_CONFIG_FIELDS, PAYMENT_PROVIDER_NAMES } from './payment-fields.ts'

const SRC = fileURLToPath(new URL('../', import.meta.url))

// fs 递归（不依赖 shell 的 find/grep：本机没有那些命令，CI 也未必有）。
function walk(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const abs = join(dir, entry.name)
    if (entry.isDirectory()) walk(abs, out)
    else out.push(abs)
  }
  return out
}

const toPosix = (p: string) => p.split('\\').join('/')

// 唯一被允许保留字段名/协议名的文件：Go 侧拿它与 epay.ConfigField* 取等号（派生方向反过来，
// 因为 Docker 的 web 阶段只 COPY web/，Go 常量在容器里不存在）。
const ALLOWED = ['lib/payment-fields.ts']

/** 支付模块全集（= 门禁扫描面）；按 §13 P50 的扫描面与排除项计算。 */
function paymentModules(): string[] {
  const families = ['pages', 'lib', 'components']
  const out: string[] = []
  for (const family of families) {
    const abs = join(SRC, family)
    if (!existsSync(abs)) continue
    for (const file of walk(abs)) {
      const rel = toPosix(relative(SRC, file))
      const base = rel.slice(rel.lastIndexOf('/') + 1)
      if (!base.startsWith('payment-')) continue // 只扫支付模块
      if (/\.test\.tsx?$/.test(base)) continue // 探测器/测试不进产物，不是镜像风险面
      if (ALLOWED.includes(rel)) continue // 被允许的那一份
      if (rel === 'lib/api/schema.d.ts') continue // codegen 产物
      if (rel.startsWith('locales/')) continue // 文案不是字段定义
      out.push(rel)
    }
  }
  return out.sort()
}

const FILES = paymentModules()
const read = (rel: string) => readFileSync(join(SRC, rel), 'utf8')

/** 在扫描面里找 needle（按"独立标识符"匹配，避免 pid 命中 rapid 这类误报），返回 file:line 列表。 */
function hits(needle: string): string[] {
  const escaped = needle.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const re = new RegExp(`(?<![A-Za-z0-9_$])${escaped}(?![A-Za-z0-9_$])`)
  const found: string[] = []
  for (const rel of FILES) {
    const lines = read(rel).split(/\r?\n/)
    lines.forEach((line, i) => {
      if (re.test(line)) found.push(`${rel}:${i + 1}: ${line.trim()}`)
    })
  }
  return found
}

/** 同上，但直接给正则（用于"字段表形状"这类模式匹配）。 */
function patternHits(re: RegExp): string[] {
  const found: string[] = []
  for (const rel of FILES) {
    const lines = read(rel).split(/\r?\n/)
    lines.forEach((line, i) => {
      if (re.test(line)) found.push(`${rel}:${i + 1}: ${line.trim()}`)
    })
  }
  return found
}

describe('P50 前端门禁：支付模块不得保留协议镜像', () => {
  it('扫描面非空且覆盖页面/纯函数层（glob 坏掉时不许静默全绿）', () => {
    assert.ok(FILES.includes('pages/payment-accounts.tsx'), `扫描面: ${FILES.join(', ')}`)
    assert.ok(FILES.includes('lib/payment-form.ts'), `扫描面: ${FILES.join(', ')}`)
    assert.ok(!FILES.includes('lib/payment-fields.ts'), '字段清单文件必须被豁免（它是唯一被允许的一份）')
    assert.ok(!FILES.includes('lib/api/schema.d.ts'), 'codegen 产物不在扫描面')
  })

  it('不出现本地协议镜像标识符（PROVIDERS / ProviderField）', () => {
    for (const name of ['PROVIDERS', 'ProviderField']) {
      assert.deepEqual(hits(name), [], `支付模块里残留 ${name}（改用元数据 providers[]）`)
    }
  })

  it('不出现内建协议名字面量（协议名只能来自元数据）', () => {
    for (const name of PAYMENT_PROVIDER_NAMES) {
      assert.deepEqual(hits(name), [], `支付模块里出现协议名字面量 ${name}`)
    }
  })

  it("不出现 `{ name: '<字段名>'` 形式的本地字段表", () => {
    assert.deepEqual(
      patternHits(/\{\s*name:\s*['"`]/),
      [],
      '支付模块里出现字段表形状（字段清单只允许在 lib/payment-fields.ts）',
    )
  })

  it('config 字段名只允许出现在 lib/payment-fields.ts（PAYMENT_CONFIG_FIELDS 反向自检）', () => {
    const allowlistPath = ALLOWED[0]
    assert.ok(existsSync(join(SRC, allowlistPath)), `${allowlistPath} 必须存在（Go 侧门禁读它）`)
    const allowlist = read(allowlistPath)
    for (const name of PAYMENT_CONFIG_FIELDS) {
      // ① 扫描面（除豁免文件外）不得出现该字段名
      assert.deepEqual(hits(name), [], `支付模块里出现 config 字段名 ${name}（字段清单只允许在 ${allowlistPath}）`)
      // ② 自检：清单文件本身确实还留着它（防止"清空清单"式假绿）
      assert.ok(allowlist.includes(name), `${allowlistPath} 缺少字段 ${name}`)
    }
  })
})

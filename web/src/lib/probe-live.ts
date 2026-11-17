// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 批量探测的**行内即时结果**（live）纯逻辑：轮次归属、不可变更新、显示取舍、页面侧超时。
//
// 为什么不放进组件：这些规则要么会被并发收尾的回调（刷新完成时）晚一步执行，要么要按
// 「服务端是否已经给出更新的结论」自愈——放在 JSX 里就只能靠手测。全是纯函数/无 React，
// `node --test` 直接跑（本文件不 import 任何运行时依赖，只有 type import）。
import type { ProxyView } from '@/lib/proxy-form'

// 单条探测的本地结果：
// - `row`：服务端 200 返回的整行（拨不通也是这一支：last_probe_ok=false）。
// - `requestFailed`：**浏览器↔服务器**的失败（4xx/5xx/网络/超时）。它不是探测结论，
//   所以不得写 last_probe_ok / last_probe_ip（否则「请求失败」会被渲染成「不可达」）。
export type ProbeOutcome =
  | { kind: 'row'; row: ProxyView; at: string }
  | { kind: 'requestFailed'; message: string; at: string }

export type ProbeLive = ReadonlyMap<number, ProbeOutcome>

/** 回填一条结果（不可变：返回新 Map，旧 Map 保持不变）。 */
export function applyOutcome(live: ProbeLive, id: number, outcome: ProbeOutcome): ProbeLive {
  const next = new Map(live)
  next.set(id, outcome)
  return next
}

/**
 * 丢掉若干 id 的本地结论——用于「服务端**重新**决定了这一行」（单条探测成功、编辑保存成功、
 * 删除成功）：此刻列表刷新给出的才是事实，本地快照只会挡住它。没有命中时原样返回（React
 * 状态同一引用 ⇒ 不触发渲染）。
 */
export function dropIds(live: ProbeLive, ids: readonly number[]): ProbeLive {
  let hit = false
  const next = new Map(live)
  for (const id of ids) {
    if (next.delete(id)) hit = true
  }
  return hit ? next : live
}

/**
 * 一轮跑批结束时的收尾过滤。`round` = 发起这一轮的轮次令牌，`currentRound` = 现在的令牌。
 *
 * 两个不变量：
 * - **跨轮不动**：上一轮的收尾（它还在等列表刷新）绝不能删掉新一轮已经回填的条目，否则
 *   新一轮的 `探测中 x/N` 会跳着走、已出的结果会被抹掉。轮次不同 ⇒ 原样返回。
 * - **同轮只留 requestFailed**：列表刷新成功后，成功结论由服务端事实取代（别让本地整行
 *   快照盖住之后的编辑）；`requestFailed` 服务端根本没写过，必须留着才能在行内看到。
 */
export function pruneForRound(live: ProbeLive, round: number, currentRound: number): ProbeLive {
  if (round !== currentRound) return live
  let hit = false
  const next = new Map<number, ProbeOutcome>()
  for (const [id, outcome] of live) {
    if (outcome.kind === 'row') {
      hit = true
      continue
    }
    next.set(id, outcome)
  }
  return hit ? next : live
}

/**
 * 该行现在还能不能显示这条本地结论。自愈规则：服务端已经有**更晚**的探测结论
 * （`last_probe_at` 比本地结论的 `at` 新：单条探测成功、别人探测、或刷新后回填）⇒ 本地作废。
 * 时间解析失败（非法/缺失）时保留本地结论——宁可显示刚拿到的结果，也不假装「从未探测」。
 */
export function pickLive(p: ProxyView, outcome: ProbeOutcome | undefined): ProbeOutcome | undefined {
  if (outcome === undefined) return undefined
  const server = p.last_probe_at == null ? Number.NaN : Date.parse(p.last_probe_at)
  const local = Date.parse(outcome.at)
  if (Number.isFinite(server) && Number.isFinite(local) && server > local) return undefined
  return outcome
}

/**
 * 行内显示用的条目：只有**服务端结论**（`row`）整行覆盖列表快照；`requestFailed` 原样返回
 * 列表快照（第四态由单元格单独渲染，绝不写 last_probe_*）。
 */
export function probeView(p: ProxyView, outcome: ProbeOutcome | undefined): ProxyView {
  return outcome?.kind === 'row' ? outcome.row : p
}

/**
 * 页面侧上限：`work` 到 `ms` 还没落地就以 `message` 拒绝（原 promise 的结果被忽略）。
 * 为什么必须有：服务端最坏 15s（TCP 5s + 取 IP 10s），但悬挂的 fetch（NAT 黑洞、代理链路
 * 半开）没有上限 ⇒ `running` 会永久为真，批量按钮与所有行内探测按钮一起被锁死。
 */
export function withTimeout<T>(work: Promise<T>, ms: number, message: string): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(message)), ms)
    work.then(
      v => { clearTimeout(timer); resolve(v) },
      e => { clearTimeout(timer); reject(e) },
    )
  })
}

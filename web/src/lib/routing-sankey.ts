// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 路由流 Sankey 纯数据构造器：服务端 RoutingFlowEdge → (route → ordinal/lane →
// account → outcome/terminal) 四层节点 + 去重链路。铁律：不重算任何服务端数值
// （Wilson/frontier/plan/守恒判定都不在这里），只做拓扑映射与 chain_count 求和。
// 输出对同一输入字节级确定：节点按 (kind rank, 语义键) 排序，链路按 (source,target) 排序。

import type { components } from '@/lib/api/schema'

type FlowEdge = components['schemas']['RoutingFlowEdge']

export type FlowSankeyNodeKind = 'route' | 'lane' | 'account' | 'outcome' | 'terminal'

export interface FlowSankeyNode {
  /** 稳定 ID：route / lane:<ordinal>:<lane> / acct:<id> / out:<outcome> / term:<outcome> */
  id: string
  /** 本地化显示名（Recharts nameKey 消费） */
  name: string
  kind: FlowSankeyNodeKind
}

export interface FlowSankeyLink {
  source: number
  target: number
  value: number
  sourceId: string
  targetId: string
  /** lane→account 到达链的 transition_reason 去重集（升序）；其余链为空 */
  reasons: string[]
  /** 到达链的 previous_outcome 去重集（升序） */
  prevOutcomes: string[]
  /** 到达链的 previous_account_id 去重集（升序） */
  prevAccounts: number[]
  /** 到达链中是否含首发起边（previous_account_id=null） */
  hasFirstDispatch: boolean
}

export interface FlowSankeyData {
  nodes: FlowSankeyNode[]
  links: FlowSankeyLink[]
}

export interface FlowSankeyLabels {
  route: string
  lane: (ordinal: number, lane: string) => string
  account: (accountId: number) => string
  outcome: (outcome: string) => string
  terminal: (outcome: string) => string
}

const KIND_RANK: Record<FlowSankeyNodeKind, number> = {
  route: 0,
  lane: 1,
  account: 2,
  outcome: 3,
  terminal: 4,
}

interface NodeDraft {
  id: string
  name: string
  kind: FlowSankeyNodeKind
  /** 列内排序键：lane=(ordinal,lane)；account=(id)；outcome/terminal=(outcome) */
  sortKey: readonly (string | number)[]
}

interface LinkDraft {
  sourceId: string
  targetId: string
  value: number
  reasons: Set<string>
  prevOutcomes: Set<string>
  prevAccounts: Set<number>
  hasFirstDispatch: boolean
}

export function buildFlowSankey(edges: readonly FlowEdge[], labels: FlowSankeyLabels): FlowSankeyData {
  const nodes = new Map<string, NodeDraft>()
  const links = new Map<string, LinkDraft>()

  const ensureNode = (draft: NodeDraft) => {
    if (!nodes.has(draft.id)) nodes.set(draft.id, draft)
  }
  const addLink = (sourceId: string, targetId: string, edge: FlowEdge, arrival: boolean) => {
    const key = `${sourceId}\u0000${targetId}`
    let draft = links.get(key)
    if (!draft) {
      draft = {
        sourceId,
        targetId,
        value: 0,
        reasons: new Set(),
        prevOutcomes: new Set(),
        prevAccounts: new Set(),
        hasFirstDispatch: false,
      }
      links.set(key, draft)
    }
    draft.value += edge.chain_count
    if (!arrival) return
    // 迁移拓扑只挂在 lane→account 到达链上：reason/prev 描述「为什么走到这个账号」。
    if (edge.transition_reason) draft.reasons.add(edge.transition_reason)
    if (edge.previous_outcome) draft.prevOutcomes.add(edge.previous_outcome)
    if (edge.previous_account_id === null) draft.hasFirstDispatch = true
    else draft.prevAccounts.add(edge.previous_account_id)
  }

  ensureNode({ id: 'route', name: labels.route, kind: 'route', sortKey: [] })
  for (const edge of edges) {
    const laneId = `lane:${edge.ordinal}:${edge.lane}`
    const accountId = `acct:${edge.account_id}`
    const terminal = edge.is_terminal
    const outcomeId = `${terminal ? 'term' : 'out'}:${edge.outcome}`
    ensureNode({ id: laneId, name: labels.lane(edge.ordinal, edge.lane), kind: 'lane', sortKey: [edge.ordinal, edge.lane] })
    ensureNode({ id: accountId, name: labels.account(edge.account_id), kind: 'account', sortKey: [edge.account_id] })
    ensureNode({
      id: outcomeId,
      name: terminal ? labels.terminal(edge.outcome) : labels.outcome(edge.outcome),
      kind: terminal ? 'terminal' : 'outcome',
      sortKey: [edge.outcome],
    })
    addLink('route', laneId, edge, false)
    addLink(laneId, accountId, edge, true)
    addLink(accountId, outcomeId, edge, false)
  }

  const sortedKeys = (a: readonly (string | number)[], b: readonly (string | number)[]) => {
    for (let i = 0; i < Math.min(a.length, b.length); i++) {
      const x = a[i]!
      const y = b[i]!
      const cmp = typeof x === 'number' && typeof y === 'number' ? x - y : String(x).localeCompare(String(y))
      if (cmp !== 0) return cmp
    }
    return a.length - b.length
  }
  const ordered = [...nodes.values()].sort(
    (a, b) => KIND_RANK[a.kind] - KIND_RANK[b.kind] || sortedKeys(a.sortKey, b.sortKey) || a.id.localeCompare(b.id)
  )
  const indexById = new Map(ordered.map((n, i) => [n.id, i]))

  const outLinks = [...links.values()]
    .map<FlowSankeyLink>(draft => ({
      source: indexById.get(draft.sourceId)!,
      target: indexById.get(draft.targetId)!,
      value: draft.value,
      sourceId: draft.sourceId,
      targetId: draft.targetId,
      reasons: [...draft.reasons].sort(),
      prevOutcomes: [...draft.prevOutcomes].sort(),
      prevAccounts: [...draft.prevAccounts].sort((x, y) => x - y),
      hasFirstDispatch: draft.hasFirstDispatch,
    }))
    .sort((a, b) => a.source - b.source || a.target - b.target)

  return {
    nodes: ordered.map(({ id, name, kind }) => ({ id, name, kind })),
    links: outLinks,
  }
}

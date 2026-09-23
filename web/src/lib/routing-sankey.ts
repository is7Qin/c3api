// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 路由流 Sankey 纯数据构造器：服务端边 → (route → ordinal/lane →
// account → outcome/terminal) 四层节点 + 去重链路。铁律：不重算任何服务端数值
// （Wilson/frontier/plan/守恒判定都不在这里），只做拓扑映射与 chain_count 求和。
// 输出对同一输入字节级确定：节点按 (kind rank, 语义键) 排序，链路按 (source,target) 排序。
//
// 折叠语义（§4.2）：`folded` 是权威判别位——折叠边（后端 account_id=0 的「其他」
// 聚合）按 (ordinal, lane) 分层归入该层独立的「其他」节点（`other:<ordinal>:<lane>`），
// 不得按 account_id 跨层合并。previous_accounts 对折叠节点恒为空（后端即如此），
// 故折叠边的 tooltip 只挂 transition_reasons / previous_outcomes。

import type { components } from '@/lib/api/schema'

type GraphEdge = components['schemas']['RoutingFlowGraphEdge']

export type FlowSankeyNodeKind = 'route' | 'lane' | 'account' | 'outcome' | 'terminal'

export interface FlowSankeyNode {
  /** 稳定 ID：route / lane:<ordinal>:<lane> / acct:<id> / other:<ordinal>:<lane> / out:<outcome> / term:<outcome> */
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
  /** 到达链的 previous_account_id 去重集（升序）；折叠节点恒为空 */
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
  /** folded=true 的层独立「其他」聚合节点的聚合列标签 */
  foldedAccount: () => string
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

/** 归一化后的边行：两种服务端边形（明细 / 桑基聚合）在此统一。 */
interface EdgeLike {
  ordinal: number
  lane: string
  /** 账号列节点稳定 ID：明细边为 acct:<id>；折叠边为层独立 other:<ordinal>:<lane> */
  accountNodeId: string
  accountName: string
  /** 层内排序键：明细边按 account_id；折叠边恒排该层末尾 */
  accountSortKey: readonly (string | number)[]
  outcome: string
  isTerminal: boolean
  chainCount: number
  reasons: readonly string[]
  prevOutcomes: readonly string[]
  prevAccounts: readonly number[]
  hasFirstDispatch: boolean
}

function buildSankey(likes: readonly EdgeLike[], labels: FlowSankeyLabels): FlowSankeyData {
  const nodes = new Map<string, NodeDraft>()
  const links = new Map<string, LinkDraft>()

  const ensureNode = (draft: NodeDraft) => {
    if (!nodes.has(draft.id)) nodes.set(draft.id, draft)
  }
  const addLink = (sourceId: string, targetId: string, like: EdgeLike, arrival: boolean) => {
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
    draft.value += like.chainCount
    if (!arrival) return
    // 迁移拓扑只挂在 lane→account 到达链上：reason/prev 描述「为什么走到这个账号」。
    for (const r of like.reasons) draft.reasons.add(r)
    for (const o of like.prevOutcomes) draft.prevOutcomes.add(o)
    for (const a of like.prevAccounts) draft.prevAccounts.add(a)
    if (like.hasFirstDispatch) draft.hasFirstDispatch = true
  }

  ensureNode({ id: 'route', name: labels.route, kind: 'route', sortKey: [] })
  for (const like of likes) {
    const laneId = `lane:${like.ordinal}:${like.lane}`
    const terminal = like.isTerminal
    const outcomeId = `${terminal ? 'term' : 'out'}:${like.outcome}`
    ensureNode({ id: laneId, name: labels.lane(like.ordinal, like.lane), kind: 'lane', sortKey: [like.ordinal, like.lane] })
    ensureNode({ id: like.accountNodeId, name: like.accountName, kind: 'account', sortKey: like.accountSortKey })
    ensureNode({
      id: outcomeId,
      name: terminal ? labels.terminal(like.outcome) : labels.outcome(like.outcome),
      kind: terminal ? 'terminal' : 'outcome',
      sortKey: [like.outcome],
    })
    addLink('route', laneId, like, false)
    addLink(laneId, like.accountNodeId, like, true)
    addLink(like.accountNodeId, outcomeId, like, false)
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

/** 桑基聚合边（RoutingFlowGraphEdge）：folded 边按层归入独立「其他」节点。 */
export function buildFoldedFlowSankey(edges: readonly GraphEdge[], labels: FlowSankeyLabels): FlowSankeyData {
  return buildSankey(
    edges.map<EdgeLike>(edge => {
      if (!edge.folded) {
        return {
          ordinal: edge.ordinal,
          lane: edge.lane,
          accountNodeId: `acct:${edge.account_id}`,
          accountName: labels.account(edge.account_id),
          accountSortKey: [0, edge.account_id],
          outcome: edge.outcome,
          isTerminal: edge.is_terminal,
          chainCount: edge.chain_count,
          reasons: edge.transition_reasons,
          prevOutcomes: edge.previous_outcomes,
          prevAccounts: edge.previous_accounts,
          hasFirstDispatch: false,
        }
      }
      // 折叠边：以前缀 other 按层独立成节点（不跨层合并）；previous_accounts
      // 后端恒为空，此处直接沿用（不推导）；transition_reasons/previous_outcomes 照常挂载。
      return {
        ordinal: edge.ordinal,
        lane: edge.lane,
        accountNodeId: `other:${edge.ordinal}:${edge.lane}`,
        accountName: labels.foldedAccount(),
        accountSortKey: [1, ''],
        outcome: edge.outcome,
        isTerminal: edge.is_terminal,
        chainCount: edge.chain_count,
        reasons: edge.transition_reasons,
        prevOutcomes: edge.previous_outcomes,
        prevAccounts: [],
        hasFirstDispatch: false,
      }
    }),
    labels,
  )
}

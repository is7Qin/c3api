// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 路由流 Sankey 呈现层：节点按层着色（CSS 变量走 style，SVG 呈现属性不解析 var()）；
// 迁移拓扑（reason/prev outcome/prev account）在 lane→account 到达链的 tooltip 上。

import { type ComponentProps } from 'react'
import { useTranslation } from 'react-i18next'
import type { SankeyLinkProps, SankeyNode, SankeyNodeProps } from 'recharts'
import { ChartTooltipContent } from '@/components/ui/chart'
import type { FlowSankeyLink, FlowSankeyNode, FlowSankeyNodeKind } from '@/lib/routing-sankey'

const SANKEY_KIND_COLOR: Record<FlowSankeyNodeKind, string> = {
  route: 'var(--chart-1)',
  lane: 'var(--chart-2)',
  account: 'var(--chart-3)',
  outcome: 'var(--chart-4)',
  terminal: 'var(--chart-5)',
}

const isFlowSankeyNode = (n: SankeyNode): n is SankeyNode & FlowSankeyNode =>
  typeof (n as Partial<FlowSankeyNode>).kind === 'string'

const isFlowSankeyLink = (p: unknown): p is FlowSankeyLink & { source: SankeyNode; target: SankeyNode } =>
  typeof p === 'object' && p !== null && 'sourceId' in p && 'targetId' in p && 'reasons' in p

export function FlowSankeyNodeShape(props: SankeyNodeProps) {
  const { x, y, width, height, payload } = props
  const kind = isFlowSankeyNode(payload) ? payload.kind : 'account'
  const labelOnRight = kind !== 'outcome' && kind !== 'terminal'
  return (
    <g>
      <rect x={x} y={y} width={width} height={height} rx={2} style={{ fill: SANKEY_KIND_COLOR[kind], fillOpacity: 0.9 }} />
      <text
        x={labelOnRight ? x + width + 6 : x - 6}
        y={y + height / 2}
        textAnchor={labelOnRight ? 'start' : 'end'}
        dominantBaseline="central"
        style={{ fill: 'var(--foreground)', fontSize: 10 }}
      >
        {payload.name}
      </text>
    </g>
  )
}

export function FlowSankeyLinkShape(props: SankeyLinkProps) {
  const { sourceX, sourceY, targetX, targetY, sourceControlX, targetControlX, linkWidth, payload } = props
  const kind = isFlowSankeyNode(payload.target) ? payload.target.kind : 'outcome'
  return (
    <path
      className="recharts-sankey-link"
      d={`M${sourceX},${sourceY} C${sourceControlX},${sourceY} ${targetControlX},${targetY} ${targetX},${targetY}`}
      fill="none"
      strokeWidth={linkWidth}
      style={{ stroke: SANKEY_KIND_COLOR[kind], strokeOpacity: 0.28 }}
    />
  )
}

export function FlowSankeyTooltip({ active, payload }: ComponentProps<typeof ChartTooltipContent>) {
  const { t } = useTranslation()
  if (!active || !payload?.length) return null
  const entry = payload[0]
  // Sankey 的 searcher 返回 {payload,name,value} 包装对象，recharts 再原样塞进
  // entry.payload——真正的链路对象在 entry.payload.payload。
  const wrapped = entry?.payload as { payload?: unknown } | undefined
  const inner = wrapped?.payload ?? wrapped
  const link = isFlowSankeyLink(inner) ? inner : null
  const value = Number(entry?.value ?? 0)
  return (
    <div className="grid min-w-40 max-w-72 items-start gap-1 rounded-lg border border-border/50 bg-background px-2.5 py-1.5 text-xs shadow-xl">
      <div className="font-medium">{link ? `${link.source.name} → ${link.target.name}` : entry?.name}</div>
      <div className="flex items-center justify-between gap-4">
        <span className="text-muted-foreground">{t('stats.routing.sankey.chains')}</span>
        <span className="font-mono font-medium tabular-nums">{value.toLocaleString()}</span>
      </div>
      {link && (
        <>
          {link.reasons.length > 0 && (
            <div className="flex items-start justify-between gap-4">
              <span className="shrink-0 text-muted-foreground">{t('stats.routing.sankey.reasons')}</span>
              <span className="text-right">{link.reasons.join(', ')}</span>
            </div>
          )}
          {link.prevOutcomes.length > 0 && (
            <div className="flex items-start justify-between gap-4">
              <span className="shrink-0 text-muted-foreground">{t('stats.routing.sankey.prevOutcomes')}</span>
              <span className="text-right">{link.prevOutcomes.join(', ')}</span>
            </div>
          )}
          {link.prevAccounts.length > 0 && (
            <div className="flex items-start justify-between gap-4">
              <span className="shrink-0 text-muted-foreground">{t('stats.routing.sankey.prevAccounts')}</span>
              <span className="text-right font-mono tabular-nums">{link.prevAccounts.map(id => `A${id}`).join(', ')}</span>
            </div>
          )}
          {link.hasFirstDispatch && (
            <div className="text-muted-foreground">{t('stats.routing.sankey.firstArrival')}</div>
          )}
        </>
      )}
    </div>
  )
}

export function FlowSankeyLegend() {
  const { t } = useTranslation()
  const items: ReadonlyArray<{ kind: FlowSankeyNodeKind; label: string }> = [
    { kind: 'route', label: t('stats.routing.sankey.kind.route') },
    { kind: 'lane', label: t('stats.routing.sankey.kind.lane') },
    { kind: 'account', label: t('stats.routing.sankey.kind.account') },
    { kind: 'outcome', label: t('stats.routing.sankey.kind.outcome') },
    { kind: 'terminal', label: t('stats.routing.sankey.kind.terminal') },
  ]
  return (
    <div className="flex flex-wrap items-center justify-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
      {items.map(({ kind, label }) => (
        <span key={kind} className="flex items-center gap-1.5">
          <span className="size-2 shrink-0 rounded-[2px]" style={{ backgroundColor: SANKEY_KIND_COLOR[kind] }} />
          {label}
        </span>
      ))}
    </div>
  )
}

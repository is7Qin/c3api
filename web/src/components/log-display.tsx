// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useTranslation } from 'react-i18next'
import { Badge } from '@/components/ui/badge'
import type { components } from '@/lib/api/schema'

type ErrorType = components['schemas']['ErrorType']
type RequestFormat = components['schemas']['RequestFormat']

// 后端 error_type 的**运行时**值域。openapi 的 ErrorType enum 尚未收录 timeout
// （§9 非目标：不把 error_type 扩成新 enum），故这里按真值放宽一格——否则筛选
// 下拉与徽章对 timeout 行失效（文案取自 locales 的 errorType.timeout）。
export type ErrorTypeKey = ErrorType | 'timeout'

// 单一值域：usage_logs 与 err_logs 共用（P0-A 之后 usage_logs 承载**所有可计费
// 错误行**——成功 none + 任一可计费失败类别 5xx/network/timeout/429/4xx/abort…，
// 不再是 none/abort 的子集，故不再有第二个收窄列表）。
export const ERROR_TYPES: readonly ErrorTypeKey[] = ['none', '429', '4xx', '5xx', 'network', 'timeout', 'auth', 'no_account', 'abort', 'billing']

const ERROR_META: Record<ErrorTypeKey, string> = {
  none: 'bg-emerald-500/10 text-emerald-600 dark:bg-emerald-400/10 dark:text-emerald-400',
  '4xx': 'bg-yellow-500/10 text-yellow-600 dark:bg-yellow-400/10 dark:text-yellow-400',
  '5xx': 'bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400',
  network: 'bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400',
  // timeout 与 network/abort 同属失败但语义不同（上游零产出 vs 传输断开 vs
  // 客户端结束）——用琥珀色区分，便于 ops 一眼分辨「罚错人」的形态。
  timeout: 'bg-amber-500/10 text-amber-600 dark:bg-amber-400/10 dark:text-amber-400',
  abort: 'bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400',
  '429': 'bg-orange-500/10 text-orange-600 dark:bg-orange-400/10 dark:text-orange-400',
  auth: 'bg-muted text-muted-foreground',
  no_account: 'bg-muted text-muted-foreground',
  billing: 'bg-violet-500/10 text-violet-600 dark:bg-violet-400/10 dark:text-violet-400',
}

export function ErrorTypeBadge({ type }: { type?: ErrorTypeKey }) {
  const { t } = useTranslation()
  if (!type) return <span className="text-xs text-muted-foreground">—</span>
  return <Badge className={ERROR_META[type]}>{t(`errorType.${type}`)}</Badge>
}

// 交付状态（commit_state，spec-stream-failure-truth §5.2）。值域由后端固定；
// null = 未知（历史行/未经该路径）——必须显示为 —，不得落成任何具体状态。
export const COMMIT_STATES = ['not_sent', 'sent_ambiguous', 'upstream_responded', 'response_started'] as const
export type CommitStateValue = (typeof COMMIT_STATES)[number]
const isCommitState = (s: string): s is CommitStateValue => (COMMIT_STATES as readonly string[]).includes(s)

const COMMIT_META: Record<string, string> = {
  not_sent: 'bg-muted text-muted-foreground',
  upstream_responded: 'bg-muted text-muted-foreground',
  sent_ambiguous: 'bg-amber-500/10 text-amber-600 dark:bg-amber-400/10 dark:text-amber-400',
  response_started: 'bg-sky-500/10 text-sky-600 dark:bg-sky-400/10 dark:text-sky-400',
}

export function CommitStateBadge({ state }: { state?: string | null }) {
  const { t } = useTranslation()
  if (!state) return <span className="text-xs text-muted-foreground">—</span>
  const label = isCommitState(state) ? t(`commitState.${state}`) : state
  return <Badge className={COMMIT_META[state] ?? 'bg-muted text-muted-foreground'}>{label}</Badge>
}

// 「流中断」判据（spec §5.2）：已交付的失败 = commit_state ∈ {sent_ambiguous,
// response_started} ∧ error_type ≠ none。commit_state 为
// null（未知）不算流中断。
export const STREAM_BROKEN_COMMIT_STATES: readonly string[] = ['sent_ambiguous', 'response_started']

export const isStreamBroken = (l: { commit_state?: string | null; ErrorType?: ErrorType }): boolean =>
  l.ErrorType !== 'none' && !!l.commit_state && STREAM_BROKEN_COMMIT_STATES.includes(l.commit_state)

export const FORMAT_LABELS: Readonly<Record<string, string>> = {
  'openai-chat': 'OpenAI Chat',
  'openai-responses': 'OpenAI Responses',
  'openai-responses-ws': 'OpenAI Responses (WS)',
  'openai-images': 'OpenAI Images',
  'openai-search': 'OpenAI Search',
  anthropic: 'Anthropic',
} satisfies Record<RequestFormat, string>

export const fmtDuration = (ms: number): string => (ms >= 1000 ? `${(ms / 1000).toFixed(1)}s` : `${ms}ms`)

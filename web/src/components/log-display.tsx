// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useTranslation } from 'react-i18next'
import { Badge } from '@/components/ui/badge'
import type { components } from '@/lib/api/schema'

type ErrorType = components['schemas']['ErrorType']
type RequestFormat = components['schemas']['RequestFormat']

export const ERROR_TYPES: readonly ErrorType[] = ['none', '429', '4xx', '5xx', 'network', 'auth', 'no_account', 'abort', 'billing']

export const USAGE_ERROR_TYPES: readonly ErrorType[] = ['none', 'abort']

const ERROR_META: Record<ErrorType, string> = {
  none: 'bg-emerald-500/10 text-emerald-600 dark:bg-emerald-400/10 dark:text-emerald-400',
  '4xx': 'bg-yellow-500/10 text-yellow-600 dark:bg-yellow-400/10 dark:text-yellow-400',
  '5xx': 'bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400',
  network: 'bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400',
  abort: 'bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400',
  '429': 'bg-orange-500/10 text-orange-600 dark:bg-orange-400/10 dark:text-orange-400',
  auth: 'bg-muted text-muted-foreground',
  no_account: 'bg-muted text-muted-foreground',
  billing: 'bg-violet-500/10 text-violet-600 dark:bg-violet-400/10 dark:text-violet-400',
}

export function ErrorTypeBadge({ type }: { type?: ErrorType }) {
  const { t } = useTranslation()
  if (!type) return <span className="text-xs text-muted-foreground">—</span>
  return <Badge className={ERROR_META[type]}>{t(`errorType.${type}`)}</Badge>
}

// 交付状态（commit_state，spec-stream-failure-truth §5.2）。值域由后端固定；
// null = 未知（历史行/未经该路径）——必须显示为 —，不得落成任何具体状态。
export const COMMIT_STATES = ['not_sent', 'sent_ambiguous', 'upstream_responded', 'response_started', 'client_committed'] as const
export type CommitStateValue = (typeof COMMIT_STATES)[number]
const isCommitState = (s: string): s is CommitStateValue => (COMMIT_STATES as readonly string[]).includes(s)

const COMMIT_META: Record<string, string> = {
  not_sent: 'bg-muted text-muted-foreground',
  upstream_responded: 'bg-muted text-muted-foreground',
  sent_ambiguous: 'bg-amber-500/10 text-amber-600 dark:bg-amber-400/10 dark:text-amber-400',
  response_started: 'bg-sky-500/10 text-sky-600 dark:bg-sky-400/10 dark:text-sky-400',
  client_committed: 'bg-emerald-500/10 text-emerald-600 dark:bg-emerald-400/10 dark:text-emerald-400',
}

export function CommitStateBadge({ state }: { state?: string | null }) {
  const { t } = useTranslation()
  if (!state) return <span className="text-xs text-muted-foreground">—</span>
  const label = isCommitState(state) ? t(`commitState.${state}`) : state
  return <Badge className={COMMIT_META[state] ?? 'bg-muted text-muted-foreground'}>{label}</Badge>
}

// 「流中断」判据（spec §5.2）：已交付的失败 = commit_state ∈ {sent_ambiguous,
// response_started, client_committed} ∧ error_type ≠ none。commit_state 为
// null（未知）不算流中断。
export const STREAM_BROKEN_COMMIT_STATES: readonly string[] = ['sent_ambiguous', 'response_started', 'client_committed']

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

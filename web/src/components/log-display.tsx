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

export const FORMAT_LABELS: Readonly<Record<string, string>> = {
  'openai-chat': 'OpenAI Chat',
  'openai-responses': 'OpenAI Responses',
  'openai-responses-ws': 'OpenAI Responses (WS)',
  'openai-images': 'OpenAI Images',
  'openai-search': 'OpenAI Search',
  anthropic: 'Anthropic',
} satisfies Record<RequestFormat, string>

export const fmtDuration = (ms: number): string => (ms >= 1000 ? `${(ms / 1000).toFixed(1)}s` : `${ms}ms`)

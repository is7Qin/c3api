// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 用户面「模型 / 定价」页（/user/models）：对用户可见的每个分组展示该组真实可
// 路由的模型、模型价格（基底 + 全部条件变体）与该用户在该组的有效倍率。最终价由
// 用户按有效倍率自算（不做合成价展示）。顶部跨组搜索模型名；命中 0 的组隐藏。
import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Boxes, RefreshCw, Search } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { userApi } from '@/lib/api/client'
import { formatGroupMultiplier } from '@/components/group-option'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { formatPricePerMillion } from '@/components/fmt'
import { condSummary, effectSummary } from '@/lib/price-variant'
import type { DynamicKey } from '@/lib/i18n'
import { filterCatalog, priceRows, variantCondition, variantEffect, type PriceRowKey, type PriceRowUnit } from '@/lib/model-catalog'

const fadeUp = {
  initial: { opacity: 0, y: 12 },
  animate: { opacity: 1, y: 0 },
}

// 价格分量 → i18n label（Static 键，抽取器可见）。
const PRICE_LABEL_KEY = {
  input: 'user.models.input',
  output: 'user.models.output',
  cacheRead: 'user.models.cacheRead',
  cacheWrite: 'user.models.cacheWrite',
  perCall: 'user.models.perCall',
  imgIn: 'user.models.imgIn',
  imgOut: 'user.models.imgOut',
  perImage: 'user.models.perImage',
} as const satisfies Record<PriceRowKey, DynamicKey>

// 价格分量值格式化：token/image token 价（perM）→ $x/M；按次/按张 → $x。
function formatPriceValue(unit: PriceRowUnit, v: number | null | undefined): string {
  if (v == null) return '—'
  return unit === 'perM' ? formatPricePerMillion(v) : `$${v}`
}

export default function UserModels() {
  const { t } = useTranslation()
  const [query, setQuery] = useState('')

  const { data, isLoading, isError, refetch, isFetching } = useQuery({
    queryKey: ['user', 'models'],
    queryFn: () => userApi.listUserModels(),
  })

  const groups = useMemo(() => filterCatalog(data ?? [], query), [data, query])
  const total = data?.length ?? 0

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('user.models.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('user.models.subtitle')}</p>
        </div>
        <Button variant="outline" size="sm" onClick={() => refetch()} disabled={isFetching}>
          <RefreshCw className={isFetching ? 'animate-spin' : ''} />
          {t('user.models.retry')}
        </Button>
      </div>

      <div className="relative">
        <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
        <Input
          className="pl-9"
          value={query}
          onChange={e => setQuery(e.target.value)}
          placeholder={t('user.models.searchPlaceholder')}
          autoComplete="off"
          spellCheck={false}
        />
      </div>

      {isLoading && (
        <div className="space-y-3">
          {Array.from({ length: 3 }, (_, i) => (
            <Skeleton key={i} className="h-24 w-full" />
          ))}
        </div>
      )}

      {isError && (
        <Card>
          <CardContent className="flex flex-col items-center gap-3 py-10 text-center">
            <p className="text-sm text-muted-foreground">{t('user.models.error')}</p>
            <Button variant="outline" size="sm" onClick={() => refetch()}>
              <RefreshCw />
              {t('user.models.retry')}
            </Button>
          </CardContent>
        </Card>
      )}

      {!isLoading && !isError && total === 0 && (
        <Card>
          <CardContent className="flex flex-col items-center gap-2 py-12 text-center">
            <Boxes className="size-6 text-muted-foreground" />
            <p className="text-sm text-muted-foreground">{t('user.models.empty')}</p>
          </CardContent>
        </Card>
      )}

      {!isLoading && !isError && total > 0 && groups.length === 0 && (
        <Card>
          <CardContent className="py-12 text-center">
            <p className="text-sm text-muted-foreground">{t('user.models.emptySearch')}</p>
          </CardContent>
        </Card>
      )}

      {!isLoading &&
        !isError &&
        groups.map((g, gi) => (
          <motion.section key={g.Group.ID ?? gi} {...fadeUp} transition={{ duration: 0.25 }} className="space-y-3">
            <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
              <h2 className="text-lg font-medium">{g.Group.Name ?? `#${g.Group.ID}`}</h2>
              <Badge variant="secondary" className="tabular-nums">
                {formatGroupMultiplier(g.Group.EffectiveMultiplier ?? g.Group.PriceMultiplier, t)}
              </Badge>
              {g.Group.Description ? (
                <span className="text-sm text-muted-foreground">{g.Group.Description}</span>
              ) : null}
            </div>

            {g.Models.length === 0 ? (
              <p className="text-sm text-muted-foreground">{t('user.models.empty')}</p>
            ) : (
              <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
                {g.Models.map(m => {
                  const rows = priceRows(m)
                  return (
                    <Card key={m.Model}>
                      <CardContent className="space-y-3 py-4">
                        <div className="flex flex-wrap items-center gap-2">
                          <span className="font-mono text-sm font-medium break-all">{m.Model}</span>
                          {m.Provider ? <Badge variant="outline">{m.Provider}</Badge> : null}
                          {m.SupportsPromptCaching ? (
                            <Badge variant="secondary">{t('user.models.promptCaching')}</Badge>
                          ) : null}
                        </div>

                        <div className="space-y-1 text-xs">
                          {rows.length === 0 ? (
                            <div className="text-muted-foreground">—</div>
                          ) : (
                            rows.map(r => (
                              <div key={r.key} className="flex items-center justify-between gap-2">
                                <span className="text-muted-foreground">{t(PRICE_LABEL_KEY[r.key])}</span>
                                <span className="tabular-nums">{formatPriceValue(r.unit, r.value)}</span>
                              </div>
                            ))
                          )}
                        </div>

                        {m.Variants.length > 0 && (
                          <div className="space-y-1 border-t pt-2">
                            <div className="text-xs font-medium text-muted-foreground">{t('user.models.variants')}</div>
                            {m.Variants.map(v => (
                              <div key={v.Seq} className="flex items-baseline gap-2 text-xs">
                                <span className="shrink-0 tabular-nums text-muted-foreground">#{v.Seq}</span>
                                <span className="text-muted-foreground" title={condSummary(t, variantCondition(v))}>
                                  {condSummary(t, variantCondition(v))}
                                </span>
                                <span className="tabular-nums">{effectSummary(variantEffect(v))}</span>
                              </div>
                            ))}
                          </div>
                        )}
                      </CardContent>
                    </Card>
                  )
                })}
              </div>
            )}
          </motion.section>
        ))}
    </div>
  )
}

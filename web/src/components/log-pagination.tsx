// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ChevronLeft, ChevronRight, Loader2, RotateCcw } from 'lucide-react'
import { pageNumbers } from '@/lib/page-numbers'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'

// 每页条数选项（两日志页共用；游标分页无 total/offset，条数只影响单页行数）。
const LIMITS = [10, 20, 50, 100, 200]

// 游标链分页底栏（分组页底栏形态，但无 total 语义——keyset 分页物理无总页数，不显示"共 N 条"）：
// 左 = 页码信息（仅"第 X 页"）+ 每页条数；右 = 页码按钮组（1..已加载最远页窗口化）
// + 跳转输入（无上限 clamp，跳未加载页 = 触发补链）+ 上一页/下一页/回最新。
// 受控组件：状态全在 useCursorLogs，本组件只渲染与回传事件。
// i18n 命名空间两页不同（logs.pagination.* vs user.logs.pagination.*），用 ns 前缀注入。
export function LogPagination({
  page,
  loadedPages,
  hasNext,
  isFetching,
  limit,
  ns,
  onChangeLimit,
  onGoToPage,
  onGoPrev,
  onGoNext,
  onGoLatest,
}: {
  page: number
  loadedPages: number
  hasNext: boolean
  isFetching: boolean
  limit: number
  /** i18n 键前缀（页码/下一页/回最新文案命名空间）。 */
  ns: 'logs.pagination' | 'user.logs.pagination'
  onChangeLimit: (limit: number) => void
  onGoToPage: (target: number) => void
  onGoPrev: () => void
  onGoNext: () => void
  onGoLatest: () => void
}) {
  const { t } = useTranslation()
  const [jumpTo, setJumpTo] = useState('')

  // 页码跳转：Enter 或按钮确认。非法输入（空/非数字）回退当前页；
  // 无上限 clamp（未知 total）——跳未加载页 = 触发补链，超出真实末页由 hook 停在末页。
  const jump = () => {
    const n = Math.floor(Number(jumpTo))
    if (jumpTo.trim() === '' || !Number.isFinite(n)) {
      setJumpTo(String(page))
      return
    }
    onGoToPage(Math.max(1, n))
    setJumpTo('')
  }

  return (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm text-muted-foreground tabular-nums">
          {t(`${ns}.pageOnly`, { page })}
          {isFetching && <Loader2 className="ml-1.5 inline size-3 animate-spin" aria-hidden="true" />}
        </span>
        <Select
          items={Object.fromEntries(LIMITS.map(n => [String(n), String(n)]))}
          value={String(limit)}
          disabled={isFetching}
          onValueChange={v => onChangeLimit(Number(v))}
        >
          <SelectTrigger size="sm" aria-label={`${t('list.pageSize')}: ${limit}`}>
            <span className="text-xs">{t('list.pageSize')}</span>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {LIMITS.map(n => <SelectItem key={n} value={String(n)} label={String(n)}>{n}</SelectItem>)}
          </SelectContent>
        </Select>
      </div>
      <div className="flex items-center gap-2">
        {/* 页码按钮组：1..已加载最远页（全部已缓存，点击零请求）；isFetching 时禁用 */}
        <div className="hidden items-center gap-1 md:flex">
          {pageNumbers(page, Math.max(1, loadedPages)).map((p, i) =>
            p === 'ellipsis' ? (
              <span key={`ellipsis-${i}`} aria-hidden="true" className="px-0.5 text-xs text-muted-foreground">
                …
              </span>
            ) : (
              <Button
                key={p}
                variant={p === page ? 'default' : 'outline'}
                size="sm"
                className="h-7 min-w-7 px-2 text-xs tabular-nums"
                aria-current={p === page ? 'page' : undefined}
                aria-label={`${t('list.jumpTo')} ${p}${t('list.jumpPage')}`}
                disabled={isFetching}
                onClick={() => onGoToPage(p)}
              >
                {p}
              </Button>
            )
          )}
        </div>
        {/* 跳转输入：无 max 上限（未知 total）；Enter 或按钮确认 */}
        <div className="hidden items-center gap-1.5 sm:flex">
          <span className="text-xs">{t('list.jumpTo')}</span>
          <Input
            type="number"
            min={1}
            value={jumpTo}
            onChange={e => setJumpTo(e.target.value)}
            onKeyDown={e => {
              if (e.key === 'Enter') {
                e.preventDefault()
                jump()
              }
            }}
            aria-label={t('list.jumpToLabel')}
            disabled={isFetching}
            className="h-7 w-14 px-1.5 text-xs tabular-nums"
          />
          <span className="text-xs">{t('list.jumpPage')}</span>
          <Button variant="outline" size="sm" className="h-7 px-2 text-xs" disabled={isFetching} onClick={jump}>
            {t('list.jump')}
          </Button>
        </div>
        <Button variant="outline" size="sm" disabled={page <= 1 || isFetching} onClick={onGoPrev}>
          <ChevronLeft /> {t('list.prev')}
        </Button>
        <Button variant="outline" size="sm" disabled={!hasNext || isFetching} onClick={onGoNext}>
          <span className="hidden sm:inline">{t(`${ns}.next`)}</span> <ChevronRight />
        </Button>
        {page > 1 && (
          <Button variant="outline" size="sm" disabled={isFetching} onClick={onGoLatest}>
            <RotateCcw /> <span className="hidden sm:inline">{t(`${ns}.latest`)}</span>
          </Button>
        )}
      </div>
    </div>
  )
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 内嵌页面编辑器（spec 2026-11-16 内嵌页面 §7-6 / §9-U6）：设置页 customPages tab 的正文。
//
// 为什么它必须是**专用**编辑器而不是普通的 string 设置行：`custom_pages` 的值是一整条
// JSON 数组，普通行会拿一个单行 Input 去编辑它（本仓刚因单行控件吃掉 PEM 换行踩过），
// 而且顺序即侧栏顺序、id 是永久链接的一部分——这些语义单行输入框表达不了。
//
// 客户端校验是 UX，不是安全边界（§9-U7）：规则在 lib/custom-pages.ts，与服务端
// internal/domain/custompage.go 同一套；这里只负责"就地报错"和"整包保存"。
import { useMemo, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { ArrowDown, ArrowUp, Plus, Trash2, TriangleAlert } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { asPlainKey } from '@/lib/i18n'
import { api } from '@/App'
import {
  CUSTOM_PAGE_ICONS,
  CUSTOM_PAGE_MAX_ITEMS,
  CUSTOM_PAGES_QUERY_KEY,
  normalizeCustomPages,
  serializeCustomPages,
  tryParseCustomPages,
  validateCustomPages,
  type CustomPageConfig,
  type CustomPageIconKey,
  type CustomPageIssue,
  type CustomPageVisibilityKey,
} from '@/lib/custom-pages'
import { Button } from '@/components/ui/button'
import { Card, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { toast } from '@/components/ui/toast'

// 设置 key：全仓唯一一条以 JSON 承载的 string 设置（spec §14-5）。
const SETTING_KEY = 'custom_pages'
// 条目列表的 query key 单点定义在 lib/custom-pages.ts（§19-6）：保存后只失效这一个
// key——不得用 ['user'] 整前缀（会命中 ['user','me'] 与开关缓存，web/AGENTS.md 明令）。

// 编辑器行 = 配置条目 + 「是不是本次新增」。id 只在新行可填：改既有 id 等于换链接（§4.2）。
type EditorRow = CustomPageConfig & { isNew: boolean }

// 新增行的缺省（§9-U6）：id 留空由管理员填——**不自动生成**，它是永久链接的一部分。
const emptyRow = (): EditorRow => ({ id: '', label: '', url: '', visibility: 'user', icon: 'link', enabled: true, isNew: true })

// 设置原值 → 草稿行：解析（空形态 = 空列表）→ 归一（trim label + 补缺省）→ 全部标记为既有。
const rowsFrom = (raw: string): EditorRow[] => normalizeCustomPages(tryParseCustomPages(raw).pages).map(page => ({ ...page, isNew: false }))

export function CustomPagesEditor({ value }: { value: string }) {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const parsed = useMemo(() => tryParseCustomPages(value), [value])
  const [rows, setRows] = useState<EditorRow[]>(() => rowsFrom(value))
  // 挂载即对**已保存的**配置跑一次校验（§19-6）：能解析但违反 V2–V9 的配置
  // （javascript:、重复 id、21 条、总字节超限）在编辑器里看起来完全正常，管理员只会
  // 发现侧栏"莫名少了入口"。横幅只提示、不阻塞编辑——他可能正要逐条修。
  // 计算源是 value（已保存值）而不是草稿：草稿不合法由保存时的那条错误负责。
  const savedIssue = useMemo(
    () => validateCustomPages(normalizeCustomPages(tryParseCustomPages(value).pages)),
    [value],
  )
  // 只在提交过一次之后才显示校验错误：一进页面就红一片没有意义。
  const [attempted, setAttempted] = useState(false)
  // 外部值变化（首次挂载 / 保存成功回填）时重建草稿：new 标记清零、错误清空。
  // 用「渲染期调整」而不是 effect——effect 里的同步 setState 会多跑一轮渲染，
  // 而这里本来就是"prop 变了就重置派生状态"。保存失败不会改 value，草稿不会被冲掉。
  const [syncedValue, setSyncedValue] = useState(value)
  if (syncedValue !== value) {
    setSyncedValue(value)
    setRows(rowsFrom(value))
    setAttempted(false)
  }
  const [resetOpen, setResetOpen] = useState(false)

  const save = useMutation({
    mutationFn: (pages: readonly CustomPageConfig[]) => api.updateSetting({ key: SETTING_KEY, value: serializeCustomPages(pages) }),
    onSuccess: all => {
      // updateSetting 本就回全量设置：先写缓存，再定向失效条目列表（侧栏下一次渲染即可见）。
      qc.setQueryData(['settings'], all)
      void qc.invalidateQueries({ queryKey: CUSTOM_PAGES_QUERY_KEY })
      toast.add({ title: t('settings.saved'), type: 'success' })
    },
    onError: (e: Error) => toast.add({ title: e.message, type: 'error' }),
  })

  const issue: CustomPageIssue | null = attempted ? validateCustomPages(rows) : null
  // 解析层失败 = 配置损坏（语法错 / 不是数组）：结构都坏了，逐行编辑没有意义，只能清空重来。
  const corrupt = parsed.error !== null

  const update = (index: number, patch: Partial<CustomPageConfig>) =>
    setRows(prev => prev.map((row, i) => (i === index ? { ...row, ...patch } : row)))

  const move = (index: number, delta: number) =>
    setRows(prev => {
      const to = index + delta
      if (to < 0 || to >= prev.length) return prev
      const next = [...prev]
      const tmp = next[index]
      next[index] = next[to]
      next[to] = tmp
      return next
    })

  const remove = (index: number) => setRows(prev => prev.filter((_, i) => i !== index))

  const submit = () => {
    setAttempted(true)
    // 先归一（trim label）再校验：管理员看到的应当就是会被保存的那份内容。
    const normalized = normalizeCustomPages(rows)
    setRows(normalized.map((page, i) => ({ ...page, isNew: rows[i].isNew })))
    if (validateCustomPages(normalized)) return
    save.mutate(normalized)
  }

  const visibilityItems: Record<CustomPageVisibilityKey, string> = {
    admin: t('settings.customPages.visibilityAdmin'),
    user: t('settings.customPages.visibilityUser'),
  }
  const iconItems = Object.fromEntries(
    CUSTOM_PAGE_ICONS.map(key => [key, t(asPlainKey(`settings.customPages.icons.${key}`))]),
  ) as Record<CustomPageIconKey, string>

  const pending = save.isPending
  const atLimit = rows.length >= CUSTOM_PAGE_MAX_ITEMS
  // 一条问题的展示文案（条目级带「#序号 ·」，列表级只有文案）。行内错误与顶部横幅共用，
  // 免得两处措辞分叉。
  const issueText = (item: CustomPageIssue) =>
    `${item.index >= 0 ? `#${item.index + 1} · ` : ''}${t(asPlainKey(`settings.customPages.issue.${item.code}`))}`

  if (corrupt) {
    return (
      <Card className="space-y-3 p-4">
        <div className="space-y-1">
          <div className="font-medium">{t('settings.customPages.title')}</div>
          <p className="text-sm text-destructive">{t('settings.customPages.corrupt')}</p>
        </div>
        <Button variant="destructive" onClick={() => setResetOpen(true)} disabled={pending}>
          {t('settings.customPages.reset')}
        </Button>
        {/* 二次确认：清空会把所有条目一起删掉（含 admin 专属项），一次点击不该做到这件事。 */}
        <Dialog open={resetOpen} onOpenChange={o => { if (!o && !pending) setResetOpen(false) }}>
          <DialogContent className="sm:max-w-sm">
            <DialogHeader>
              <DialogTitle>{t('settings.customPages.reset')}</DialogTitle>
              <DialogDescription>{t('settings.customPages.resetConfirm')}</DialogDescription>
            </DialogHeader>
            <DialogFooter>
              <Button variant="outline" onClick={() => setResetOpen(false)} disabled={pending}>{t('common.cancel')}</Button>
              <Button
                variant="destructive"
                disabled={pending}
                onClick={() => save.mutate([], { onSuccess: () => setResetOpen(false) })}
              >
                {t('settings.customPages.reset')}
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      </Card>
    )
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('settings.customPages.title')}</CardTitle>
        <CardDescription>{t('settings.customPages.desc')}</CardDescription>
      </CardHeader>
      <div className="space-y-3 px-(--card-spacing) pb-(--card-spacing)">
        {savedIssue && (
          // 顶部横幅（§19-6）：已保存的配置不合法时先说出来，但**不**禁用任何输入——
          // 管理员往往正是要在这里逐条修。保存时仍会按同一套规则拦住（submit 里再校验）。
          <div className="flex items-start gap-2 rounded-lg border border-destructive/40 bg-destructive/5 px-3 py-2 text-destructive">
            <TriangleAlert className="mt-0.5 size-4 shrink-0" />
            <div className="space-y-0.5 text-xs">
              <p>{t('settings.customPages.invalidBanner')}</p>
              <p>{issueText(savedIssue)}</p>
            </div>
          </div>
        )}
        {rows.length === 0 && <p className="text-sm text-muted-foreground">{t('settings.customPages.empty')}</p>}
        {rows.map((row, i) => {
          const rowIssue = issue && issue.index === i ? issue : null
          const idInputId = `custom-page-${i}-id`
          const labelInputId = `custom-page-${i}-label`
          const urlInputId = `custom-page-${i}-url`
          return (
            <div key={i} className="space-y-2 rounded-lg border border-border/60 p-3">
              <div className="grid grid-cols-1 gap-2 lg:grid-cols-2">
                <div className="space-y-1">
                  <Label htmlFor={idInputId} className="text-xs">{t('settings.customPages.id')}</Label>
                  <Input
                    id={idInputId}
                    value={row.id}
                    // 既有条目的 id 只读：它是永久链接的一部分，改了等于换链接（§4.2 / §9-U6）。
                    readOnly={!row.isNew}
                    aria-readonly={!row.isNew}
                    className={row.isNew ? 'bg-black/[0.04] font-mono dark:bg-black/20' : 'bg-black/[0.04] font-mono text-muted-foreground dark:bg-black/20'}
                    onChange={e => { if (row.isNew) update(i, { id: e.target.value }) }}
                    disabled={pending}
                  />
                  {row.isNew && <p className="text-xs text-muted-foreground">{t('settings.customPages.idHint')}</p>}
                </div>
                <div className="space-y-1">
                  <Label htmlFor={labelInputId} className="text-xs">{t('settings.customPages.label')}</Label>
                  <Input
                    id={labelInputId}
                    value={row.label}
                    className="bg-black/[0.04] dark:bg-black/20"
                    onChange={e => update(i, { label: e.target.value })}
                    disabled={pending}
                  />
                </div>
              </div>
              <div className="space-y-1">
                <Label htmlFor={urlInputId} className="text-xs">{t('settings.customPages.url')}</Label>
                <Input
                  id={urlInputId}
                  value={row.url}
                  className="bg-black/[0.04] font-mono text-xs dark:bg-black/20"
                  onChange={e => update(i, { url: e.target.value })}
                  disabled={pending}
                />
              </div>
              <div className="flex flex-wrap items-end gap-3">
                <div className="space-y-1">
                  <span className="block text-xs text-muted-foreground">{t('settings.customPages.visibility')}</span>
                  <Select
                    items={visibilityItems}
                    value={row.visibility}
                    onValueChange={v => update(i, { visibility: v })}
                    disabled={pending}
                  >
                    <SelectTrigger className="w-36 bg-black/[0.04] border-black/10 dark:bg-black/20 dark:border-white/10" aria-label={t('settings.customPages.visibility')}>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {(Object.keys(visibilityItems) as CustomPageVisibilityKey[]).map(key => (
                        <SelectItem key={key} value={key} label={visibilityItems[key]}>{visibilityItems[key]}</SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
                <div className="space-y-1">
                  <span className="block text-xs text-muted-foreground">{t('settings.customPages.icon')}</span>
                  <Select
                    items={iconItems}
                    value={row.icon}
                    onValueChange={v => update(i, { icon: v })}
                    disabled={pending}
                  >
                    <SelectTrigger className="w-36 bg-black/[0.04] border-black/10 dark:bg-black/20 dark:border-white/10" aria-label={t('settings.customPages.icon')}>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {CUSTOM_PAGE_ICONS.map(key => (
                        <SelectItem key={key} value={key} label={iconItems[key]}>{iconItems[key]}</SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
                <div className="flex items-center gap-2 pb-1.5">
                  <Switch id={`custom-page-${i}-enabled`} checked={row.enabled} onCheckedChange={c => update(i, { enabled: c })} disabled={pending} aria-label={t('settings.customPages.enabled')} />
                  <Label htmlFor={`custom-page-${i}-enabled`} className="text-xs">{t('settings.customPages.enabled')}</Label>
                </div>
                <div className="ml-auto flex items-center gap-1 pb-1">
                  <Button variant="ghost" size="icon-sm" onClick={() => move(i, -1)} disabled={pending || i === 0} aria-label={t('settings.customPages.moveUp')} title={t('settings.customPages.moveUp')}>
                    <ArrowUp />
                  </Button>
                  <Button variant="ghost" size="icon-sm" onClick={() => move(i, 1)} disabled={pending || i === rows.length - 1} aria-label={t('settings.customPages.moveDown')} title={t('settings.customPages.moveDown')}>
                    <ArrowDown />
                  </Button>
                  <Button variant="ghost" size="icon-sm" onClick={() => remove(i)} disabled={pending} aria-label={t('settings.customPages.remove')} title={t('settings.customPages.remove')}>
                    <Trash2 />
                  </Button>
                </div>
              </div>
              {rowIssue && <p className="text-xs text-destructive">{issueText(rowIssue)}</p>}
            </div>
          )
        })}
        {issue && issue.index < 0 && <p className="text-xs text-destructive">{issueText(issue)}</p>}
        <div className="flex flex-wrap items-center gap-2">
          <Button variant="outline" onClick={() => setRows(prev => [...prev, emptyRow()])} disabled={pending || atLimit}>
            <Plus />{t('settings.customPages.add')}
          </Button>
          <Button onClick={submit} disabled={pending}>{pending ? t('settings.customPages.saving') : t('settings.customPages.save')}</Button>
          <span className="text-xs text-muted-foreground tabular-nums">{rows.length}/{CUSTOM_PAGE_MAX_ITEMS}</span>
        </div>
      </div>
    </Card>
  )
}

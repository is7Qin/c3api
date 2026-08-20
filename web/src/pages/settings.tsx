// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Settings as SettingsIcon } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
import { Button } from '@/components/ui/button'
import { Card, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Switch } from '@/components/ui/switch'
import { toast } from '@/components/ui/toast'
import type { components } from '@/lib/api/schema'

type Setting = components['schemas']['Setting']

// —— 键级语义表 ——
// 金额键（毫分）：仅 default_user_balance / default_user_temp_balance 两个键做
// USD 换算（展示 ÷100000、提交 ×100000），其余 number 键直传字符串。
const MILLI_PER_USD = 100_000
const USD_KEYS = new Set(['default_user_balance', 'default_user_temp_balance'])
// 枚举键：值域 passthrough/strip/reject，下拉选项显示翻译（不裸露枚举名）。
const TIER_KEYS = new Set(['service_tier_policy_priority', 'service_tier_policy_flex', 'service_tier_policy_fast'])
const TIER_VALUES = ['passthrough', 'strip', 'reject'] as const

// 分组卡片：注册 / 新用户默认资源 / 价格同步 / 服务档位策略 / 集群（固定顺序渲染）。
const GROUPS: { id: string; keys: string[] }[] = [
  { id: 'signup', keys: ['signup_enabled'] },
  { id: 'defaults', keys: ['default_user_max_concurrency', 'default_user_balance', 'default_user_temp_balance', 'default_user_temp_balance_ttl_days'] },
  { id: 'pricingSync', keys: ['price_source_url', 'price_sync_cron'] },
  { id: 'tierPolicy', keys: ['service_tier_policy_priority', 'service_tier_policy_flex', 'service_tier_policy_fast'] },
  { id: 'cluster', keys: ['cluster.instances'] },
]
// 清单内全部键（兜底卡判定用——服务器新增键不在清单内时渲染到「其他设置」）。
const GROUPED_KEYS = new Set(GROUPS.flatMap(g => g.keys))

// 非负整数（与服务端 strconv.ParseInt 的接受域对齐：普通 number 键直传字符串）。
const isPlainInt = (v: string) => /^\d+$/.test(v)
const isUsdText = (v: string) => v.trim() !== '' && Number.isFinite(Number(v)) && Number(v) >= 0

// 单行设置项：PUT 单键语义，一次保存一个键；成功后用返回的全量设置回写缓存。
function SettingRow({ setting }: { setting: Setting }) {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const key = setting.Key ?? ''
  const typ = setting.Type ?? 'string'
  const current = setting.Value ?? ''
  const isUsd = USD_KEYS.has(key)
  const isTier = TIER_KEYS.has(key)

  // 输入面草稿：金额键存 USD 文本（显示侧），其余存原样字符串。
  // 仅保存成功后回写服务端值——编辑期间不被其他行的保存刷新覆盖。
  const toInput = (v: string) => (isUsd ? String(Number(v) / MILLI_PER_USD) : v)
  const [draft, setDraft] = useState(() => toInput(current))
  // 提交值快照（评审 I1）：原 dirty 布尔在 blur 必发生于输入后导致恒 true，onSuccess/
  // onError 的回写/回滚分支永不执行。改为提交瞬间快照 + draftRef 镜像最新草稿
  // （onSuccess 闭包捕获的是提交时渲染值）——「已继续编辑」= 当前草稿 ≠ 快照。
  // 另：isPending 期间的新提交入队，成功后接力提交，不再被短路静默丢弃。
  const pendingRef = useRef<string | null>(null)
  const queuedRef = useRef<string | null>(null)
  const draftRef = useRef(draft)
  draftRef.current = draft
  const [err, setErr] = useState<string | null>(null)

  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)
  const save = useMutation({
    mutationFn: (value: string) => api.updateSetting({ key, value }),
    onSuccess: all => {
      // PUT 返回更新后的全部设置 → 直接回写查询缓存（免二次 GET）。
      qc.setQueryData(['settings'], all)
      // 未继续编辑（草稿 == 快照）→ 回写服务端值；已继续编辑 → 保留草稿由接力提交覆盖。
      if (draftRef.current === pendingRef.current) {
        setDraft(toInput(all.find(s => s.Key === key)?.Value ?? current))
      }
      pendingRef.current = null
      // 在途期间入队的新值接力提交（原 isPending 短路会静默丢弃第二次编辑）。
      if (queuedRef.current !== null) {
        const v = queuedRef.current
        queuedRef.current = null
        pendingRef.current = v
        save.mutate(v)
      }
      setErr(null)
      toast.add({ title: t('settings.saved'), type: 'success' })
    },
    onError: (e: Error) => {
      // 与 onSuccess 同规则：未继续编辑则回滚到服务端值；已继续编辑保留草稿
      // （排队值丢弃，错误就地展示，由用户重新提交）。
      if (draftRef.current === pendingRef.current) {
        setDraft(toInput(current)) // 回滚到服务端值
      }
      pendingRef.current = null
      queuedRef.current = null
      const m = errMsg(e)
      if (m) setErr(m) // 服务端校验错误就地展示
    },
  })

  // 提交前校验（switch/枚举值域恒合法）：number 键非负整数、金额键非负数字。
  const submitValue = (): string | null => {
    if (typ !== 'number') return draft.trim()
    if (isUsd) {
      if (!isUsdText(draft)) { setErr(t('settings.invalidUsd')); return null }
      return String(Math.round(Number(draft) * MILLI_PER_USD)) // 输入 USD ×100000 提交毫分
    }
    if (!isPlainInt(draft)) { setErr(t('settings.invalidNumber')); return null }
    return draft
  }
  // isPending 期间的新提交入队（在途编辑不丢，成功后接力提交）；同值跳过；
  // 其余快照提交值并发送（Enter 后同值 blur 触发会被 v === current 拦截）。
  const doSave = (v: string | null) => {
    if (v == null) return
    if (save.isPending) { queuedRef.current = v; return }
    if (v === current) return
    pendingRef.current = v
    save.mutate(v)
  }

  const onEnter = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key !== 'Enter') return
    e.preventDefault()
    doSave(submitValue())
  }

  // —— 按 Type 渲染控件：switch → 开关（切换即保存）；枚举 string → 下拉（选中即保存）；
  // number → 数字输入（失焦/回车保存）；其余 string → 文本输入（保存按钮 + 回车）——
  const control =
    typ === 'switch' ? (
      <Switch
        checked={draft === 'true'}
        disabled={save.isPending}
        onCheckedChange={c => { setDraft(String(c)); doSave(String(c)) }}
        aria-label={t(`settings.labels.${key}`)}
      />
    ) : isTier ? (
      <Select
        items={{
          passthrough: t('settings.policies.passthrough'),
          strip: t('settings.policies.strip'),
          reject: t('settings.policies.reject'),
        }}
        value={draft}
        onValueChange={v => { setDraft(v); doSave(v) }}
        disabled={save.isPending}
      >
        <SelectTrigger className="w-44 bg-black/[0.04] border-black/10 dark:bg-black/20 dark:border-white/10 dark:shadow-[inset_0_1px_2px_rgba(0,0,0,0.2)]" aria-label={t(`settings.labels.${key}`)}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {TIER_VALUES.map(v => (
            <SelectItem key={v} value={v} label={t(`settings.policies.${v}`)}>{t(`settings.policies.${v}`)}</SelectItem>
          ))}
        </SelectContent>
      </Select>
    ) : typ === 'number' ? (
      <Input
        type="number"
        min={0}
        step={isUsd ? 0.00001 : 1}
        className="w-48 bg-black/[0.04] border-black/10 dark:bg-black/20 dark:border-white/10 dark:shadow-[inset_0_1px_2px_rgba(0,0,0,0.2)] text-right tabular-nums"
        value={draft}
        onChange={e => { setDraft(e.target.value); setErr(null) }}
        onBlur={() => { if (draft !== current) doSave(submitValue()) }}
        onKeyDown={onEnter}
      />
    ) : (
      <Input
        type="text"
        className="w-96 max-w-full bg-black/[0.04] border-black/10 dark:bg-black/20 dark:border-white/10 dark:shadow-[inset_0_1px_2px_rgba(0,0,0,0.2)]"
        value={draft}
        onChange={e => { setDraft(e.target.value); setErr(null) }}
        onKeyDown={onEnter}
      />
    )

  return (
    <div className="flex items-start justify-between gap-5 py-3">
      <div className="min-w-0">
        <div className="flex items-center gap-2">
          <span className="text-sm font-medium">{t(`settings.labels.${key}`)}</span>
          <code className="font-mono text-xs text-muted-foreground">{key}</code>
        </div>
        <p className="text-xs text-muted-foreground">{t(`settings.descs.${key}`)}</p>
        {isUsd && !err && <p className="text-xs text-muted-foreground">{t('settings.usdHint')}</p>}
        {err && <p className="text-xs text-destructive">{err}</p>}
      </div>
      <div className="flex shrink-0 items-center gap-2">
        {control}
        {typ === 'string' && !isTier && (
          <Button variant="outline" size="sm" onClick={() => doSave(submitValue())} disabled={save.isPending}>
            {save.isPending ? t('common.saving') : t('common.save')}
          </Button>
        )}
      </div>
    </div>
  )
}

export default function SettingsPage() {
  const { t } = useTranslation()
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['settings'],
    queryFn: () => api.getSettings(),
  })
  const byKey = new Map((data ?? []).map(s => [s.Key ?? '', s]))
  // 兜底卡数据：GROUPS 清单外剩余键（服务器新增设置键不静默丢失）。
  const others = (data ?? []).filter(s => s.Key && !GROUPED_KEYS.has(s.Key))

  return (
    // 页面级进入动画（与 users/pricing 等页一致，一次挂载仅播放一次）。
    <motion.div
      className="space-y-6"
      initial={{ opacity: 0, y: 12 }}
      animate={{ opacity: 1, y: 0 }}
      transition={{ duration: 0.25 }}
    >
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t('settings.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('settings.subtitle')}</p>
      </div>
      {isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
      ) : isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : data?.length === 0 ? (
        <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
          <SettingsIcon className="size-10" />
          <p className="font-medium">{t('settings.emptyTitle')}</p>
        </Card>
      ) : (
        <div className="space-y-5">
          {GROUPS.map(g => {
            const rows = g.keys.map(k => byKey.get(k)).filter((s): s is Setting => !!s)
            if (rows.length === 0) return null
            return (
              <Card key={g.id}>
                <CardHeader>
                  <CardTitle>{t(`settings.groups.${g.id}`)}</CardTitle>
                </CardHeader>
                <div className="divide-y divide-border px-(--card-spacing)">
                  {rows.map(s => <SettingRow key={s.Key} setting={s} />)}
                </div>
              </Card>
            )
          })}
          {/* 兜底卡：GROUPS 清单外剩余键不静默丢失——服务器新增设置键可见（语义表
              按发布节奏维护，新增键在归组前统一渲染到「其他设置」） */}
          {others.length > 0 && (
            <Card>
              <CardHeader>
                <CardTitle>{t('settings.otherTitle')}</CardTitle>
                <CardDescription>{t('settings.otherDesc')}</CardDescription>
              </CardHeader>
              <div className="divide-y divide-border px-(--card-spacing)">
                {others.map(s => <SettingRow key={s.Key} setting={s} />)}
              </div>
            </Card>
          )}
        </div>
      )}
    </motion.div>
  )
}

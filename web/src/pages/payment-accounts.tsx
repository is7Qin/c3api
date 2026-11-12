// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 管理面支付账户页（spec 支付网关 v2 §11 / §10-3 / §18）：账户列表 + 新建/编辑抽屉 +
// 渠道子表。这是「v2 把凭据从 settings 搬进数据行」之后的唯一配置入口：
//
//   · **账户是数据**：key / provider / merchant_ref 创建后只读（key 是网关后台填过的
//     回调 URL 段，复用会让老回调打到新账户）；`config_error` 非空 = 该账户已被选路跳过。
//   · **DELETE 恒为软停**：置 enabled=false，行永不删除（幂等 200）——所以按钮文案是
//     "软停用"，不是"删除"。
//   · **密钥不回显**：响应里 secret 字段恒为 `***`；没改就原样回传 `***`（服务端视为
//     保持原值），改了才发新值。
//   · **协议元数据是唯一来源（§18）**：协议下拉 = `GET /payment/providers` 的
//     `providers[].name`，渠道 pay_type 下拉 = 选中协议的 `capabilities`，配置表单按
//     `config_schema.fields` 动态渲染。本文件**不保留**任何本地协议字段表：镜像的代价
//     不是多写几行，而是必然漂移（协议加字段 → 表单少一个框 → 严格 JSON 下必然 400；
//     字段改判为秘密 → 前端仍用明文框 → 密钥进浏览器历史）。P50 是这条的门禁。
//   · **三态门禁（N49 / P52）**：loading / failed 时配置表单不可打开、不可提交；
//     决策全部来自 lib/payment-form.ts 的 resolveFormState——组件里不出现第二套推导。
import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { AlertTriangle, CreditCard, Plus, RefreshCw, Save, Trash2, X } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import type { DynamicKey } from '@/lib/i18n'
import { api } from '@/App'
import { ApiError } from '@/lib/api/client'
import {
  controlFor,
  fieldLabel,
  metaStateOf,
  providerOf,
  resolveFormState,
  warnUnknownKinds,
  type MetaState,
  type PaymentProviderInfo,
} from '@/lib/payment-form'
import {
  canAddChannel,
  channelDeleteRequest,
  channelDraftOf,
  channelRowActions,
  channelUpsertRequest,
  newChannelDraft,
  payTypeEditable,
  rateText,
  type ChannelDraft,
} from '@/lib/payment-channels'
import { Alert, AlertAction, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Switch } from '@/components/ui/switch'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Textarea } from '@/components/ui/textarea'
import { toast } from '@/components/ui/toast'
import { formatDateTime } from '@/components/fmt'
import { cn } from '@/lib/utils'
import type { components } from '@/lib/api/schema'

type PaymentAccount = components['schemas']['PaymentAccount']
type PaymentChannel = components['schemas']['PaymentChannel']

// 渠道币种白名单：与 domain.paymentCurrencies 同集合（大写 ISO 代码；写错 = 渠道不可售）。
// 前端给下拉而不是自由输入（避免手打小写 "cny" 这类"存进去但不可售"的坑），
// 权威判定仍在服务端（不在白名单 → 渠道不可售）。
const CURRENCIES = ['CNY', 'USD', 'EUR', 'GBP', 'HKD', 'AUD', 'CAD', 'SGD', 'THB', 'MYR'] as const

// MICRO / rateText / 渠道草稿与判定全部来自 lib/payment-channels（P55 的决策层）：
// 汇率定点换算与"三态下渠道还能不能停用"必须与测试共用同一份实现。

// 汇率文本：正整数或最多 4 位小数（7 / 7.2 / 7.2000），且 > 0。
const RATE_RE = /^\d+(\.\d{1,4})?$/
const rateValid = (v: string) => RATE_RE.test(v.trim()) && Number(v) > 0
// USD 限额：非负整数（下限另要求 ≥ 1）。
const INT_RE = /^\d+$/

// channelIssue 渠道草稿的前端预校验（返回 i18n key 后缀；null = 通过）。
// 权威校验在服务端（币种白名单 / rate_micro > 0 / 1 ≤ min ≤ max → 400）。
function channelIssue(c: { rate: string; min: string; max: string }): string | null {
  if (!rateValid(c.rate)) return 'channelInvalidRate'
  if (!INT_RE.test(c.min.trim()) || !INT_RE.test(c.max.trim())) return 'channelInvalidInt'
  const min = Number(c.min)
  const max = Number(c.max)
  if (min < 1 || max < min) return 'channelInvalidRange'
  return null
}

// errMsg 统一取服务端 error 文案（401 由全局 handleAuthError 处理，页面不再弹）。
const errMsg = (e: unknown) => (e instanceof ApiError ? e.message : (e as Error)?.message)

export default function PaymentAccounts() {
  const { t } = useTranslation()
  const qc = useQueryClient()

  const accountsQ = useQuery({ queryKey: ['payment-accounts'], queryFn: () => api.listPaymentAccounts() })
  // useMemo 化：`data ?? []` 每次渲染都是新数组，直接进依赖会让下面的 useMemo 白算。
  const rows = useMemo(() => accountsQ.data ?? [], [accountsQ.data])

  // 协议元数据（§18-2）：数据源是进程内注册表，不查 DB、不随账户数量变化，故缓存久一点。
  const providersQ = useQuery({
    queryKey: ['payment-providers'],
    queryFn: () => api.listPaymentProviders(),
    staleTime: 5 * 60_000,
    // 焦点重取失败会把表单误降级成 failed（元数据本来就不随账户变化）：故障恢复走重试按钮。
    refetchOnWindowFocus: false,
  })
  const metaState = metaStateOf(providersQ)
  const providers = useMemo<readonly PaymentProviderInfo[]>(() => providersQ.data?.providers ?? [], [providersQ.data])
  // 新建账户要选协议、要按协议渲染凭据表单 → 元数据没起来（或注册表为空）时入口禁用：
  // 没有 providers[] 就没有协议可选，硬开抽屉只能让人对着一个空表单按保存。
  // 编辑入口只在 loading 时禁用：failed 态列表照常可读，抽屉里只关掉协议下拉与配置表单。
  const canCreate = metaState === 'ready' && providers.length > 0
  const createHint =
    metaState === 'failed'
      ? t('paymentAccounts.metaFailed')
      : metaState === 'ready' && providers.length === 0
        ? t('paymentAccounts.unknownProvider')
        : undefined

  // drawer 主体：'new' = 新建；string = 按 key 编辑（对象从列表快照派生，渠道保存后自动刷新）
  const [editing, setEditing] = useState<'new' | string | null>(null)
  const editingAccount = useMemo(
    () => (editing && editing !== 'new' ? rows.find(r => r.key === editing) ?? null : null),
    [editing, rows],
  )

  // 账户删除 = 软停用确认（行还在、可再启用；"删"只是 enabled=false 的运维叫法）
  const [deleting, setDeleting] = useState<PaymentAccount | null>(null)
  const del = useMutation({
    mutationFn: (key: string) => api.deletePaymentAccount(key),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['payment-accounts'] })
      setDeleting(null)
      toast.add({ title: t('paymentAccounts.deleted'), type: 'success' })
    },
    onError: (e: unknown) =>
      toast.add({ title: t('paymentAccounts.deleteFailed'), description: errMsg(e), type: 'error' }),
  })

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('paymentAccounts.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('paymentAccounts.subtitle')}</p>
        </div>
        <div className="flex items-center gap-2">
          <Button variant="ghost" onClick={() => accountsQ.refetch()} disabled={accountsQ.isFetching}>
            <RefreshCw className={cn(accountsQ.isFetching && 'animate-spin')} /> {t('paymentAccounts.refresh')}
          </Button>
          <Button
            onClick={() => setEditing('new')}
            disabled={!canCreate}
            title={createHint}
          >
            <Plus /> {t('paymentAccounts.new')}
          </Button>
        </div>
      </div>

      {/* 元数据故障：列表照常、只降级表单（§18-3）。给出错误 + 重试，绝不回退到本地镜像。 */}
      {metaState === 'failed' && (
        <Alert variant="destructive">
          <AlertTriangle />
          <AlertDescription>{t('paymentAccounts.metaFailed')}</AlertDescription>
          <AlertAction>
            <Button
              variant="outline"
              size="sm"
              onClick={() => providersQ.refetch()}
              disabled={providersQ.isFetching}
            >
              <RefreshCw className={cn(providersQ.isFetching && 'animate-spin')} /> {t('paymentAccounts.refresh')}
            </Button>
          </AlertAction>
        </Alert>
      )}

      {accountsQ.isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: errMsg(accountsQ.error) })}</p>
      ) : accountsQ.isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : rows.length === 0 ? (
        <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
          <CreditCard className="size-10" />
          <p className="font-medium">{t('paymentAccounts.emptyTitle')}</p>
          <p className="text-sm">{t('paymentAccounts.emptyDesc')}</p>
        </Card>
      ) : (
        <ScrollArea
          data-od-id="table-scroll-payment-accounts"
          className="rounded-[14px] border border-[rgba(19,45,83,0.26)] bg-[color:var(--glass-card-light)] shadow-[inset_0_1px_0_rgba(255,255,255,0.5),0_10px_36px_rgba(19,45,83,0.16)] backdrop-blur-[var(--glass-blur)] dark:bg-[color:var(--glass-card-dark)] dark:shadow-[inset_0_1px_0_rgba(255,255,255,0.07),0_10px_36px_rgba(2,6,14,0.5)] dark:border-[rgba(148,180,220,0.32)]"
          showHorizontal
        >
          <Table className="min-w-[1120px]" containerClassName="overflow-x-visible border-0 shadow-none rounded-none bg-transparent backdrop-blur-none">
            <TableHeader>
              <TableRow>
                <TableHead>{t('paymentAccounts.table.key')}</TableHead>
                <TableHead>{t('paymentAccounts.table.displayName')}</TableHead>
                <TableHead>{t('paymentAccounts.table.provider')}</TableHead>
                <TableHead>{t('paymentAccounts.table.merchantRef')}</TableHead>
                <TableHead className="text-right">{t('paymentAccounts.table.priority')}</TableHead>
                <TableHead>{t('paymentAccounts.table.enabled')}</TableHead>
                <TableHead className="text-right">{t('paymentAccounts.table.channels')}</TableHead>
                <TableHead>{t('paymentAccounts.table.configError')}</TableHead>
                <TableHead className="text-right">{t('paymentAccounts.table.actions')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody className="[&_td]:py-3">
              {rows.map(a => (
                <TableRow key={a.key} data-state={editing === a.key ? 'selected' : undefined}>
                  <TableCell><code className="font-mono text-sm">{a.key}</code></TableCell>
                  <TableCell className="max-w-56 truncate">{a.display_name || '—'}</TableCell>
                  <TableCell className="text-xs text-muted-foreground">{a.provider}</TableCell>
                  <TableCell className="font-mono text-xs">{a.merchant_ref}</TableCell>
                  <TableCell className="text-right tabular-nums">{a.priority}</TableCell>
                  <TableCell>
                    {a.enabled ? (
                      <Badge variant="secondary" className="gap-1.5 text-emerald-700 dark:text-emerald-400">
                        <span className="size-1.5 rounded-full bg-emerald-500" />
                        {t('paymentAccounts.enabledYes')}
                      </Badge>
                    ) : (
                      <Badge variant="secondary" className="gap-1.5 text-muted-foreground">
                        <span className="size-1.5 rounded-full bg-muted-foreground/60" />
                        {t('paymentAccounts.enabledNo')}
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">{a.channels.length}</TableCell>
                  {/* config_error 红标：该账户已被选路跳过（坏配置不静默，§17-1/§11） */}
                  <TableCell>
                    {a.config_error ? (
                      <span
                        className="inline-flex items-center gap-1 text-xs text-red-700 dark:text-red-400"
                        title={a.config_error}
                      >
                        <AlertTriangle className="size-3.5 shrink-0" />
                        <span className="max-w-48 truncate">{t('paymentAccounts.configErrorBadge')}</span>
                      </span>
                    ) : (
                      <span className="text-xs text-muted-foreground">—</span>
                    )}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-2">
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={() => setEditing(a.key)}
                        disabled={metaState === 'loading'}
                      >
                        {t('paymentAccounts.edit')}
                      </Button>
                      <Button variant="ghost" size="sm" onClick={() => setDeleting(a)}>
                        <Trash2 /> {t('paymentAccounts.deleteAction')}
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </ScrollArea>
      )}

      {/* —— 新建/编辑抽屉（右侧整高）：key 变化才重挂表单，渠道保存不清空草稿 —— */}
      <Dialog open={editing != null} onOpenChange={o => { if (!o) setEditing(null) }}>
        <DialogContent className="top-0 right-0 left-auto h-dvh max-h-dvh translate-x-0 translate-y-0 rounded-none rounded-l-[14px] sm:max-w-2xl">
          {editing != null && (
            <AccountDrawer
              key={editingAccount?.key ?? 'new'}
              account={editingAccount}
              isNew={editing === 'new'}
              metaState={metaState}
              providers={providers}
              onClose={() => setEditing(null)}
              onCreated={key => setEditing(key)}
            />
          )}
        </DialogContent>
      </Dialog>

      {/* —— 软停用确认 —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !del.isPending) setDeleting(null) }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t('paymentAccounts.deleteTitle')}</DialogTitle>
            <DialogDescription>
              {t('paymentAccounts.deleteDesc', { key: deleting?.key ?? '' })}
            </DialogDescription>
          </DialogHeader>
          <Alert>
            <AlertDescription>{t('paymentAccounts.deleteSoftHint')}</AlertDescription>
          </Alert>
          {del.isError && <p className="text-sm text-destructive">{errMsg(del.error)}</p>}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)} disabled={del.isPending}>
              {t('common.cancel')}
            </Button>
            <Button
              variant="destructive"
              onClick={() => deleting && del.mutate(deleting.key)}
              disabled={del.isPending}
            >
              {del.isPending ? t('paymentAccounts.deleting') : t('paymentAccounts.deleteConfirm')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

// AccountDrawer 账户表单 + 渠道子表。
//
// isNew：key/provider 可填（创建后只读）；编辑态两者只读展示。渠道子表只在编辑态出现
// （新建时账户还不存在，没有可挂渠道的行）。
function AccountDrawer({
  account,
  isNew,
  metaState,
  providers,
  onClose,
  onCreated,
}: {
  account: PaymentAccount | null
  isNew: boolean
  metaState: MetaState
  providers: readonly PaymentProviderInfo[]
  onClose: () => void
  onCreated: (key: string) => void
}) {
  const { t } = useTranslation()
  const qc = useQueryClient()

  // —— 账户字段（编辑态初值来自列表快照；抽屉按 key 重挂，故初值即状态）——
  const [key, setKey] = useState(account?.key ?? '')
  // 新建时默认选元数据里的第一个协议（服务端已按 name 升序，前端不再排序）。
  const [provider, setProvider] = useState(account?.provider ?? providers[0]?.name ?? '')
  const [displayName, setDisplayName] = useState(account?.display_name ?? '')
  const [priority, setPriority] = useState(String(account?.priority ?? 0))
  const [enabled, setEnabled] = useState(account?.enabled ?? true)
  // 本地版本号：只改元数据的请求也带上它（revision 未变时条件仍成立，不会误报 409）。
  const [revision, setRevision] = useState(account?.config_revision ?? 0)
  const [formErr, setFormErr] = useState<string | null>(null)

  // 协议名：新建取下拉，编辑取账户（创建后只读）。渠道 pay_type 与配置字段都从它派生。
  const providerName = isNew ? provider : (account?.provider ?? '')
  const meta = providerOf(providers, providerName)
  // schema 的**顺序即表单顺序**（§18-2：服务端不重排，前端也不许 sort）。
  const schema = useMemo(() => meta?.config_schema?.fields ?? [], [meta])
  const capabilities = useMemo(() => meta?.capabilities ?? [], [meta])

  // 三态决策（P52）在这里一次算完；下面所有"能不能打开 / 能不能提交"都只读 form.*。
  const form = useMemo(() => resolveFormState(metaState, account, schema), [metaState, account, schema])

  // 初值**只**来自 resolveFormState：绝不在 fields 还没到时自己拼一份"每个键都是空串"的
  // config——那会让每个字段都算改过，一次保存把密钥写成空串（N49，空串 ≠ `***`）。
  const [config, setConfig] = useState<Record<string, string>>(() => form.config)
  const fieldNames = form.fields.map(f => f.name).join('\u0000')
  // 字段集合变化（元数据从失败恢复、或换协议）→ 旧草稿已无意义，用服务端初值重建。
  // 依赖只能是 fieldNames：form.config 每次都是新对象，进依赖会让用户输入每次渲染被清掉。
  useEffect(() => {
    setConfig(form.config)
  }, [fieldNames])

  // 未知 kind 的 secret 感知降级必须可观测（§18-7）：warning 含 provider 名与 kind。
  useEffect(() => {
    warnUnknownKinds(providerName, form.fields)
  }, [providerName, schema])

  // 初始 config（secret 已掩码）：只有**改过**的字段才需要提交——全部没改 = 不发 config
  // （否则每次点保存都会 config_revision+1 并失效配置缓存）。
  const configDirty = useMemo(
    () => Object.keys(config).some(k => (config[k] ?? '') !== (form.config[k] ?? '')),
    [config, form.config],
  )

  // 保存可用性：配置可提交（ready）**或**这次根本没碰配置（只改展示名/优先级/启停）。
  // failed 态因此仍能改账户的展示字段——"只禁用协议下拉与配置表单"（§18-3）。
  // 新建必须等元数据齐（要选协议、要按 schema 填凭据）。
  const canSave = isNew ? form.submittable && providerName !== '' : form.submittable || !configDirty

  const applyView = (view: PaymentAccount) => {
    qc.setQueryData<PaymentAccount[]>(['payment-accounts'], prev => {
      if (!prev) return [view]
      const idx = prev.findIndex(p => p.key === view.key)
      if (idx === -1) return [...prev, view]
      const next = [...prev]
      next[idx] = view
      return next
    })
    qc.invalidateQueries({ queryKey: ['payment-accounts'] })
  }

  const save = useMutation({
    mutationFn: () => {
      if (isNew) {
        return api.createPaymentAccount({
          key: key.trim(),
          provider: providerName,
          display_name: displayName,
          priority: Number(priority),
          enabled,
          config,
        })
      }
      // 部分更新：只发改动过的字段；config 未改就整个省掉（密钥 `***` 回传同理）。
      // 配置不可提交（loading/failed）时同样省掉——绝不能把空表当成"用户清空"。
      // `expected_config_revision`：管理台**总是**带当前版本（§19-1 / P57）——
      // 两人同时改同一账户时，后提交者的旧表单会拿到 409 而不是把新密钥改回去。
      return api.updatePaymentAccount(account?.key ?? '', {
        display_name: displayName,
        priority: Number(priority),
        enabled,
        ...(revision > 0 ? { expected_config_revision: revision } : {}),
        ...(form.submittable && configDirty ? { config } : {}),
      })
    },
    onSuccess: view => {
      applyView(view)
      // 下一次保存要基于**服务端刚回的**版本：本地状态不像 account prop 那样
      // 跟随 query cache 重挂（抽屉按 key 挂载，key 不变就不会重建）。
      setRevision(view.config_revision)
      // 保存后把凭据表单回落到服务端**掩码视图**：密钥重新变回 `***`，明文不留在
      // 输入框里（下一次保存若没改就直接回传 `***` = 保持原值）。
      if (!isNew) setConfig(resolveFormState('ready', view, schema).config)
      toast.add({
        title: isNew ? t('paymentAccounts.created') : t('paymentAccounts.saved'),
        type: 'success',
      })
      if (isNew) onCreated(view.key)
    },
    onError: (e: unknown) => {
      // 409 = 别人先改了（§19-1）：把列表拉新，否则用户会拿着旧值反复重试。
      if (e instanceof ApiError && e.status === 409) {
        qc.invalidateQueries({ queryKey: ['payment-accounts'] })
        toast.add({ title: t('paymentAccounts.saveConflict'), description: errMsg(e), type: 'error' })
        return
      }
      toast.add({ title: t('paymentAccounts.saveFailed'), description: errMsg(e), type: 'error' })
    },
  })

  const submit = () => {
    setFormErr(null)
    if (!canSave) return
    if (isNew) {
      // 与服务端 pattern 同源（key 是回调 URL 段：小写字母开头，3–32 位）
      if (!/^[a-z][a-z0-9_]{2,31}$/.test(key.trim())) return setFormErr(t('paymentAccounts.keyInvalid'))
      // 协议必须来自元数据（未注册/元数据不可用 → 没有可提交的字段表）
      if (!meta) return setFormErr(t('paymentAccounts.unknownProvider'))
    }
    // 必填只是前端提示：权威仍以服务端 400 为准（§18-3）。只在真的会提交 config 时拦。
    if (form.submittable && (isNew || configDirty)) {
      const missing = form.fields
        .filter(f => f.required === true && (config[f.name] ?? '').trim() === '')
        .map(f => f.name)
      if (missing.length > 0) {
        return setFormErr(t('paymentAccounts.configRequired', { fields: missing.join(', ') }))
      }
    }
    if (!/^\d+$/.test(priority.trim())) return setFormErr(t('paymentAccounts.priorityInvalid'))
    save.mutate()
  }

  return (
    <>
      <DialogHeader>
        <DialogTitle>
          {isNew ? t('paymentAccounts.createTitle') : t('paymentAccounts.editTitle', { key: account?.key ?? '' })}
        </DialogTitle>
        <DialogDescription>
          {isNew ? t('paymentAccounts.createDesc') : t('paymentAccounts.editDesc')}
        </DialogDescription>
      </DialogHeader>

      <div className="space-y-5">
        {/* —— 身份（key/provider 创建后只读）—— */}
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div className="space-y-1.5">
            <Label htmlFor="pay-account-key">{t('paymentAccounts.keyLabel')}</Label>
            <Input
              id="pay-account-key"
              className="font-mono"
              value={key}
              onChange={e => setKey(e.target.value)}
              disabled={!isNew}
              placeholder="epay_main"
              spellCheck={false}
              autoComplete="off"
            />
            <p className="text-xs text-muted-foreground">{t('paymentAccounts.keyHint')}</p>
          </div>
          <div className="space-y-1.5">
            <Label>{t('paymentAccounts.providerLabel')}</Label>
            {isNew ? (
              // 选项 = 元数据 providers[].name（§18-3）；元数据不可用时表单不可打开，故禁用。
              <Select
                items={Object.fromEntries(providers.map(p => [p.name, p.name]))}
                value={provider}
                onValueChange={setProvider}
              >
                <SelectTrigger
                  className="w-full"
                  aria-label={t('paymentAccounts.providerLabel')}
                  disabled={!form.openable}
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {providers.map(p => (
                    <SelectItem key={p.name} value={p.name} label={p.name}>{p.name}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            ) : (
              <Input value={providerName} readOnly disabled />
            )}
            <p className="text-xs text-muted-foreground">{t('paymentAccounts.providerHint')}</p>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="pay-account-name">{t('paymentAccounts.displayNameLabel')}</Label>
            <Input
              id="pay-account-name"
              value={displayName}
              onChange={e => setDisplayName(e.target.value)}
              placeholder={t('paymentAccounts.displayNamePlaceholder')}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="pay-account-priority">{t('paymentAccounts.priorityLabel')}</Label>
            <Input
              id="pay-account-priority"
              inputMode="numeric"
              className="tabular-nums"
              value={priority}
              onChange={e => setPriority(e.target.value)}
            />
            <p className="text-xs text-muted-foreground">{t('paymentAccounts.priorityHint')}</p>
          </div>
        </div>

        <div className="flex items-start gap-2 rounded-[10px] border border-border/60 p-3">
          <Switch id="pay-account-enabled" checked={enabled} onCheckedChange={c => setEnabled(c)} />
          <div className="space-y-1">
            <Label htmlFor="pay-account-enabled">{t('paymentAccounts.enabledLabel')}</Label>
            <p className="text-xs text-muted-foreground">{t('paymentAccounts.enabledHint')}</p>
          </div>
        </div>

        {/* —— 协议凭据（按 config_schema.fields 动态渲染；secret 字段 password + 掩码回显）—— */}
        <div className="space-y-2">
          <p className="text-sm font-medium">{t('paymentAccounts.configSection')}</p>
          {form.openable ? (
            <>
              {form.fields.map(f => {
                const id = `pay-account-cfg-${f.name}`
                const control = controlFor(f)
                const value = config[f.name] ?? ''
                const label = fieldLabel(f, k => t(k as DynamicKey))
                const placeholder = f.example ?? ''
                const required = f.required === true
                const onChange = (v: string) => setConfig(prev => ({ ...prev, [f.name]: v }))
                return (
                  <div key={f.name} className="space-y-1.5">
                    <Label htmlFor={id}>
                      {label}
                      {required && <span className="ml-1 text-destructive" aria-hidden="true">*</span>}
                      {label !== f.name && (
                        <code className="ml-2 font-mono text-xs text-muted-foreground">{f.name}</code>
                      )}
                    </Label>
                    {/* secret 字段一律 password（§11 / §18-2）：掩码回显 ***，不改就原样回传。
                        未知 kind 的降级已在 controlFor 里做过 secret 感知判定。 */}
                    {control === 'password' ? (
                      <Input
                        id={id}
                        type="password"
                        className="font-mono"
                        value={value}
                        onChange={e => onChange(e.target.value)}
                        placeholder={placeholder}
                        spellCheck={false}
                        autoComplete="new-password"
                        aria-required={required || undefined}
                      />
                    ) : control === 'multiline' ? (
                      <Textarea
                        id={id}
                        className="min-h-24 font-mono text-xs"
                        value={value}
                        onChange={e => onChange(e.target.value)}
                        placeholder={placeholder}
                        spellCheck={false}
                        autoComplete="off"
                        aria-required={required || undefined}
                      />
                    ) : (
                      <Input
                        id={id}
                        className="font-mono"
                        value={value}
                        onChange={e => onChange(e.target.value)}
                        placeholder={placeholder}
                        spellCheck={false}
                        autoComplete="off"
                        aria-required={required || undefined}
                      />
                    )}
                  </div>
                )
              })}
              <p className="text-xs text-muted-foreground">{t('paymentAccounts.configSecretHint')}</p>
            </>
          ) : metaState === 'failed' ? (
            <Alert variant="destructive">
              <AlertDescription>{t('paymentAccounts.metaFailed')}</AlertDescription>
            </Alert>
          ) : (
            // loading：不画任何输入框（画了就是"可提交的空配置"的入口）。
            <Skeleton className="h-16" />
          )}
        </div>

        {formErr && <p className="text-sm text-destructive">{formErr}</p>}
        {save.isError && <p className="text-sm text-destructive">{errMsg(save.error)}</p>}

        {/* —— 渠道子表（仅编辑态：新建账户还没有可挂渠道的行）——
            渠道子表**不受**元数据缺失影响（§18-3 / N50）：渠道 enabled 是该账户"能不能卖"
            的唯一开关，故障期必须留着停用/删除这条生路。 */}
        {!isNew && account && (
          <ChannelSection
            account={account}
            capabilities={capabilities}
            metaState={metaState}
            onView={applyView}
          />
        )}
        {isNew && (
          <Alert>
            <AlertDescription>{t('paymentAccounts.createFirstHint')}</AlertDescription>
          </Alert>
        )}
      </div>

      <DialogFooter className="sm:justify-between">
        <Button variant="outline" onClick={onClose} disabled={save.isPending}>
          {t('common.close')}
        </Button>
        <Button onClick={submit} disabled={save.isPending || !canSave}>
          <Save /> {save.isPending ? t('paymentAccounts.saving') : t('paymentAccounts.save')}
        </Button>
      </DialogFooter>
    </>
  )
}

// ChannelSection 渠道子表 + 行内编辑器（按 pay_type upsert / 单独删）。
function ChannelSection({
  account,
  capabilities,
  metaState,
  onView,
}: {
  account: PaymentAccount
  capabilities: readonly string[]
  metaState: MetaState
  onView: (view: PaymentAccount) => void
}) {
  const { t } = useTranslation()
  const qc = useQueryClient()
  // "新增渠道"需要 capabilities（§18-3）：元数据没就绪就没有取值域 → 隐藏按钮并给原因；
  // 已有行的启用/停用/删除**全部照旧**（P55）：渠道 enabled 是该账户能不能卖的唯一开关，
  // 故障期必须留着这条生路。
  const canAdd = canAddChannel(metaState, capabilities)
  // 既有行的动作可用性：这个判定与 metaState **无关**（函数签名里就没有它）。
  const actions = channelRowActions()

  // 新建渠道的默认值：优先挑一个"还没被该账户占用"的 pay_type（省一次手改）。
  const startNewDraft = (): ChannelDraft =>
    newChannelDraft(capabilities, account.channels.map(c => c.pay_type))
  const [draft, setDraft] = useState<ChannelDraft | null>(null)
  const [rowErr, setRowErr] = useState<string | null>(null)

  const upsert = useMutation({
    // 停用就是这条路径（`enabled:false` 的同一条 upsert），请求体不读元数据（P55）。
    mutationFn: (d: ChannelDraft) => {
      const req = channelUpsertRequest(account.key, d)
      return api.putPaymentAccountChannel(req.key, req.payType, req.body)
    },
    onSuccess: view => {
      onView(view)
      setDraft(null)
      setRowErr(null)
      toast.add({ title: t('paymentAccounts.channelSaved'), type: 'success' })
    },
    onError: (e: unknown) => setRowErr(errMsg(e)),
  })

  const remove = useMutation({
    // 删除是独立端点（不是 upsert）；同样不需要元数据。
    mutationFn: (payType: string) => {
      const req = channelDeleteRequest(account.key, payType)
      return api.deletePaymentAccountChannel(req.key, req.payType)
    },
    onSuccess: () => {
      // 204 无 body：拿不到新视图，失效列表由抽屉重新派生（渠道行已消失）
      qc.invalidateQueries({ queryKey: ['payment-accounts'] })
      toast.add({ title: t('paymentAccounts.channelDeleted'), type: 'success' })
    },
    onError: (e: unknown) =>
      toast.add({ title: t('paymentAccounts.deleteFailed'), description: errMsg(e), type: 'error' }),
  })

  const submitDraft = () => {
    if (!draft) return
    const issue = channelIssue(draft)
    if (issue) {
      setRowErr(t(`paymentAccounts.${issue}` as DynamicKey))
      return
    }
    upsert.mutate(draft)
  }

  return (
    <div className="space-y-3 border-t border-border/60 pt-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <p className="text-sm font-medium">{t('paymentAccounts.channelSection')}</p>
          <p className="text-xs text-muted-foreground">{t('paymentAccounts.channelHint')}</p>
        </div>
        {!draft && (canAdd ? (
          <Button variant="outline" size="sm" onClick={() => { setDraft(startNewDraft()); setRowErr(null) }}>
            <Plus /> {t('paymentAccounts.channelNew')}
          </Button>
        ) : (
          <p className="text-xs text-muted-foreground">{t('paymentAccounts.channelNewUnavailable')}</p>
        ))}
      </div>

      {account.channels.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t('paymentAccounts.channelEmpty')}</p>
      ) : (
        <ScrollArea className="rounded-[10px] border border-border/60" showHorizontal>
          <Table className="min-w-[760px]" containerClassName="border-0 shadow-none rounded-none bg-transparent backdrop-blur-none">
            <TableHeader>
              <TableRow>
                <TableHead>pay_type</TableHead>
                <TableHead>{t('paymentAccounts.table.enabled')}</TableHead>
                <TableHead>{t('paymentAccounts.channelCurrency')}</TableHead>
                <TableHead className="text-right">{t('paymentAccounts.channelRate')}</TableHead>
                <TableHead className="text-right">{t('paymentAccounts.channelMin')}</TableHead>
                <TableHead className="text-right">{t('paymentAccounts.channelMax')}</TableHead>
                <TableHead className="text-right">{t('paymentAccounts.table.actions')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody className="[&_td]:py-2">
              {account.channels.map((ch: PaymentChannel) => (
                <TableRow key={ch.pay_type}>
                  <TableCell><code className="font-mono text-sm">{ch.pay_type}</code></TableCell>
                  <TableCell className="text-xs">
                    {ch.enabled ? (
                      <span className="text-emerald-700 dark:text-emerald-400">{t('paymentAccounts.enabledYes')}</span>
                    ) : (
                      <span className="text-muted-foreground">{t('paymentAccounts.enabledNo')}</span>
                    )}
                  </TableCell>
                  <TableCell className="text-xs">{ch.currency}</TableCell>
                  <TableCell className="text-right tabular-nums text-xs">{rateText(ch.rate_micro)}</TableCell>
                  <TableCell className="text-right tabular-nums text-xs">{ch.min_usd}</TableCell>
                  <TableCell className="text-right tabular-nums text-xs">{ch.max_usd}</TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-1">
                      <Button
                        variant="ghost"
                        size="sm"
                        disabled={!actions.edit}
                        onClick={() => {
                          setRowErr(null)
                          setDraft(channelDraftOf(ch))
                        }}
                      >
                        {t('paymentAccounts.edit')}
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => remove.mutate(ch.pay_type)}
                        disabled={!actions.del || remove.isPending}
                      >
                        <Trash2 /> {t('paymentAccounts.channelDelete')}
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </ScrollArea>
      )}

      {/* —— 行内编辑器（按 pay_type upsert）—— */}
      {draft && (
        <div className="space-y-3 rounded-[10px] border border-border/60 bg-black/[0.02] p-3 dark:bg-white/[0.03]">
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label>{t('paymentAccounts.channelPayType')}</Label>
              {payTypeEditable(draft) ? (
                <Select
                  items={Object.fromEntries(capabilities.map(c => [c, c]))}
                  value={draft.payType}
                  onValueChange={v => setDraft({ ...draft, payType: v })}
                >
                  <SelectTrigger className="w-full" aria-label={t('paymentAccounts.channelPayType')}>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {capabilities.map(c => (
                      <SelectItem key={c} value={c} label={c}>{c}</SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              ) : (
                // 既有渠道的 pay_type 是寻址键：只读展示（元数据缺失时也不许改）。
                <Input value={draft.payType} readOnly disabled className="font-mono" />
              )}
              <p className="text-xs text-muted-foreground">{t('paymentAccounts.channelPayTypeHint')}</p>
            </div>
            <div className="space-y-1.5">
              <Label>{t('paymentAccounts.channelCurrency')}</Label>
              <Select
                items={Object.fromEntries(CURRENCIES.map(c => [c, c]))}
                value={draft.currency}
                onValueChange={v => setDraft({ ...draft, currency: v })}
              >
                <SelectTrigger className="w-full" aria-label={t('paymentAccounts.channelCurrency')}>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {CURRENCIES.map(c => (
                    <SelectItem key={c} value={c} label={c}>{c}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="channel-rate">{t('paymentAccounts.channelRate')}</Label>
              <Input
                id="channel-rate"
                inputMode="decimal"
                className="tabular-nums"
                value={draft.rate}
                onChange={e => { setDraft({ ...draft, rate: e.target.value }); setRowErr(null) }}
                placeholder="7.2000"
              />
              <p className="text-xs text-muted-foreground">{t('paymentAccounts.channelRateHint')}</p>
            </div>
            <div className="flex items-start gap-2 pt-6">
              <Switch
                id="channel-enabled"
                checked={draft.enabled}
                onCheckedChange={c => setDraft({ ...draft, enabled: c })}
              />
              <Label htmlFor="channel-enabled">{t('paymentAccounts.channelEnabled')}</Label>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="channel-min">{t('paymentAccounts.channelMin')}</Label>
              <Input
                id="channel-min"
                inputMode="numeric"
                className="tabular-nums"
                value={draft.min}
                onChange={e => { setDraft({ ...draft, min: e.target.value }); setRowErr(null) }}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="channel-max">{t('paymentAccounts.channelMax')}</Label>
              <Input
                id="channel-max"
                inputMode="numeric"
                className="tabular-nums"
                value={draft.max}
                onChange={e => { setDraft({ ...draft, max: e.target.value }); setRowErr(null) }}
              />
              <p className="text-xs text-muted-foreground">{t('paymentAccounts.channelLimitHint')}</p>
            </div>
          </div>
          {rowErr && <p className="text-sm text-destructive">{rowErr}</p>}
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onClick={() => { setDraft(null); setRowErr(null) }} disabled={upsert.isPending}>
              <X /> {t('paymentAccounts.channelCancel')}
            </Button>
            <Button onClick={submitDraft} disabled={upsert.isPending}>
              <Save /> {upsert.isPending ? t('paymentAccounts.channelSaving') : t('paymentAccounts.channelSave')}
            </Button>
          </div>
        </div>
      )}

      {/* 账户行的代际信息：每次保存 config 都会 config_revision+1（缓存的键之一） */}
      <p className="text-xs text-muted-foreground">
        {t('paymentAccounts.meta', {
          revision: account.config_revision,
          updatedAt: formatDateTime(account.updated_at),
        })}
      </p>
    </div>
  )
}

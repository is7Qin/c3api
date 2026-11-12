// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 管理面支付账户页（spec 支付网关 v2 §11 / §10-3）：账户列表 + 新建/编辑抽屉 +
// 渠道子表。这是「v2 把凭据从 settings 搬进数据行」之后的唯一配置入口：
//
//   · **账户是数据**：key / provider / merchant_ref 创建后只读（key 是网关后台填过的
//     回调 URL 段，复用会让老回调打到新账户）；`config_error` 非空 = 该账户已被选路跳过。
//   · **DELETE 恒为软停**：置 enabled=false，行永不删除（幂等 200）——所以按钮文案是
//     "软停用"，不是"删除"。
//   · **密钥不回显**：响应里 secret 字段恒为 `***`；没改就原样回传 `***`（服务端视为
//     保持原值），改了才发新值。
//   · **渠道按 pay_type upsert**：同一账户下多渠道互不覆盖；单渠道可单独删。
import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { AlertTriangle, CreditCard, Plus, RefreshCw, Save, Trash2, X } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import type { DynamicKey } from '@/lib/i18n'
import { api } from '@/App'
import { ApiError } from '@/lib/api/client'
import { Alert, AlertDescription } from '@/components/ui/alert'
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

// MICRO 汇率定点：rate_micro = 汇率 × 1e6（契约面整数，禁 float 传输）。
const MICRO = 1_000_000

// 渠道币种白名单：与 domain.paymentCurrencies 同集合（大写 ISO 代码；写错 = 渠道不可售）。
// 前端给下拉而不是自由输入（避免手打小写 "cny" 这类"存进去但不可售"的坑），
// 权威判定仍在服务端（不在白名单 → 渠道不可售）。
const CURRENCIES = ['CNY', 'USD', 'EUR', 'GBP', 'HKD', 'AUD', 'CAD', 'SGD', 'THB', 'MYR'] as const

// 密钥字段的掩码：服务端响应恒为它；提交时原值回传 = "保持不变"（§10-3）。
const MASK = '***'

// 协议目录 = 前端镜像。契约面（PaymentAccount）不回传 capabilities，也没有"协议元数据"
// 端点，故 epay_v2 的 config 字段与 capabilities 只能在这里留一份副本：新增协议时同步
// 此表（服务端仍是权威：pay_type ∉ capabilities → 400，config 非法 → 400）。
type ProviderField = { name: string; secret: boolean; multiline?: boolean; placeholder?: string }
type ProviderSpec = { fields: readonly ProviderField[]; capabilities: readonly string[] }

const PROVIDERS: Record<string, ProviderSpec> = {
  epay_v2: {
    // epay.ConfigField*：base_url / pid / private_key / public_key。只有 private_key 在
    // SecretFields() 里（public_key 是平台公钥，不是秘密）。
    fields: [
      { name: 'base_url', secret: false, placeholder: 'https://pay.example.com' },
      { name: 'pid', secret: false, placeholder: '1001' },
      { name: 'private_key', secret: true, placeholder: '-----BEGIN PRIVATE KEY-----' },
      { name: 'public_key', secret: false, multiline: true, placeholder: '-----BEGIN PUBLIC KEY-----' },
    ],
    // epay.Provider.Capabilities()：渠道 pay_type 的唯一取值域。
    capabilities: ['alipay', 'wxpay'],
  },
}

const providerSpec = (provider: string): ProviderSpec | undefined => PROVIDERS[provider]

// rateText 整数除法渲染 4 位小数（7200000 → "7.2000"）：不经 float 往返，
// 与 settings 页的 rate_micro 显示同一口径。4 位小数是本页的编辑粒度。
const rateText = (micro: number) => {
  const sign = micro < 0 ? '-' : ''
  const abs = Math.abs(micro)
  const frac = String(abs % MICRO).padStart(6, '0').slice(0, 4)
  return `${sign}${Math.trunc(abs / MICRO)}.${frac}`
}

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

// initialConfig 表单初值：config 里 secret 字段已由服务端掩码成 `***`，原样放进输入框
// （type=password 只显示点），不改则原值提交 = 保持原值。
function initialConfig(account: PaymentAccount | null): Record<string, string> {
  const spec = account ? providerSpec(account.provider) : undefined
  const raw = (account?.config ?? {}) as Record<string, unknown>
  const out: Record<string, string> = {}
  for (const f of spec?.fields ?? []) {
    const v = raw[f.name]
    out[f.name] = typeof v === 'string' ? v : ''
  }
  return out
}

export default function PaymentAccounts() {
  const { t } = useTranslation()
  const qc = useQueryClient()

  const accountsQ = useQuery({ queryKey: ['payment-accounts'], queryFn: () => api.listPaymentAccounts() })
  // useMemo 化：`data ?? []` 每次渲染都是新数组，直接进依赖会让下面的 useMemo 白算。
  const rows = useMemo(() => accountsQ.data ?? [], [accountsQ.data])

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
          <Button onClick={() => setEditing('new')}>
            <Plus /> {t('paymentAccounts.new')}
          </Button>
        </div>
      </div>

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
                      <Button variant="outline" size="sm" onClick={() => setEditing(a.key)}>
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
  onClose,
  onCreated,
}: {
  account: PaymentAccount | null
  isNew: boolean
  onClose: () => void
  onCreated: (key: string) => void
}) {
  const { t } = useTranslation()
  const qc = useQueryClient()

  // —— 账户字段（编辑态初值来自列表快照；抽屉按 key 重挂，故初值即状态）——
  const [key, setKey] = useState(account?.key ?? '')
  const [provider, setProvider] = useState(account?.provider ?? Object.keys(PROVIDERS)[0])
  const [displayName, setDisplayName] = useState(account?.display_name ?? '')
  const [priority, setPriority] = useState(String(account?.priority ?? 0))
  const [enabled, setEnabled] = useState(account?.enabled ?? true)
  const [config, setConfig] = useState<Record<string, string>>(() => initialConfig(account))
  const [formErr, setFormErr] = useState<string | null>(null)

  const spec = providerSpec(provider)
  // 初始 config（secret 已掩码）：只有**改过**的字段才需要提交——全部没改 = 不发 config
  // （否则每次点保存都会 config_revision+1 并失效配置缓存）。
  const baseline = useMemo(() => initialConfig(account), [account])
  const configDirty = useMemo(
    () => Object.keys(config).some(k => (config[k] ?? '') !== (baseline[k] ?? '')),
    [config, baseline],
  )

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
          provider,
          display_name: displayName,
          priority: Number(priority),
          enabled,
          config,
        })
      }
      // 部分更新：只发改动过的字段；config 未改就整个省掉（密钥 `***` 回传同理）。
      return api.updatePaymentAccount(account?.key ?? '', {
        display_name: displayName,
        priority: Number(priority),
        enabled,
        ...(configDirty ? { config } : {}),
      })
    },
    onSuccess: view => {
      applyView(view)
      // 保存后把凭据表单回落到服务端**掩码视图**：密钥重新变回 `***`，明文不留在
      // 输入框里（下一次保存若没改就直接回传 `***` = 保持原值）。
      if (!isNew) setConfig(initialConfig(view))
      toast.add({
        title: isNew ? t('paymentAccounts.created') : t('paymentAccounts.saved'),
        type: 'success',
      })
      if (isNew) onCreated(view.key)
    },
    onError: (e: unknown) =>
      toast.add({ title: t('paymentAccounts.saveFailed'), description: errMsg(e), type: 'error' }),
  })

  const submit = () => {
    setFormErr(null)
    if (isNew) {
      // 与服务端 pattern 同源（key 是回调 URL 段：小写字母开头，3–32 位）
      if (!/^[a-z][a-z0-9_]{2,31}$/.test(key.trim())) return setFormErr(t('paymentAccounts.keyInvalid'))
      if (!spec) return setFormErr(t('paymentAccounts.unknownProvider'))
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
              <Select
                items={Object.fromEntries(Object.keys(PROVIDERS).map(p => [p, p]))}
                value={provider}
                onValueChange={setProvider}
              >
                <SelectTrigger className="w-full" aria-label={t('paymentAccounts.providerLabel')}>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {Object.keys(PROVIDERS).map(p => (
                    <SelectItem key={p} value={p} label={p}>{p}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            ) : (
              <Input value={provider} readOnly disabled />
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

        {/* —— 协议凭据（动态表单；secret 字段 password + 掩码回显）—— */}
        <div className="space-y-2">
          <p className="text-sm font-medium">{t('paymentAccounts.configSection')}</p>
          {spec ? (
            <>
              {spec.fields.map(f => (
                <div key={f.name} className="space-y-1.5">
                  <Label htmlFor={`pay-account-cfg-${f.name}`}>
                    {t(`paymentAccounts.field.${f.name}` as DynamicKey)}
                    <code className="ml-2 font-mono text-xs text-muted-foreground">{f.name}</code>
                  </Label>
                  {/* secret 字段一律 type=password（§11）：掩码回显 ***，不改就原样回传。
                      非 secret 的长文本（PEM 公钥）用 Textarea 方便校对。 */}
                  {f.secret ? (
                    <Input
                      id={`pay-account-cfg-${f.name}`}
                      type="password"
                      className="font-mono"
                      value={config[f.name] ?? ''}
                      onChange={e => setConfig(prev => ({ ...prev, [f.name]: e.target.value }))}
                      placeholder={isNew ? f.placeholder : MASK}
                      spellCheck={false}
                      autoComplete="new-password"
                    />
                  ) : f.multiline ? (
                    <Textarea
                      id={`pay-account-cfg-${f.name}`}
                      className="min-h-24 font-mono text-xs"
                      value={config[f.name] ?? ''}
                      onChange={e => setConfig(prev => ({ ...prev, [f.name]: e.target.value }))}
                      placeholder={f.placeholder}
                      spellCheck={false}
                      autoComplete="off"
                    />
                  ) : (
                    <Input
                      id={`pay-account-cfg-${f.name}`}
                      className="font-mono"
                      value={config[f.name] ?? ''}
                      onChange={e => setConfig(prev => ({ ...prev, [f.name]: e.target.value }))}
                      placeholder={f.placeholder}
                      spellCheck={false}
                      autoComplete="off"
                    />
                  )}
                </div>
              ))}
              <p className="text-xs text-muted-foreground">{t('paymentAccounts.configSecretHint')}</p>
            </>
          ) : (
            <Alert>
              <AlertDescription>{t('paymentAccounts.unknownProvider')}</AlertDescription>
            </Alert>
          )}
        </div>

        {formErr && <p className="text-sm text-destructive">{formErr}</p>}
        {save.isError && <p className="text-sm text-destructive">{errMsg(save.error)}</p>}

        {/* —— 渠道子表（仅编辑态：新建账户还没有可挂渠道的行）—— */}
        {!isNew && account && <ChannelSection account={account} onView={applyView} />}
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
        <Button onClick={submit} disabled={save.isPending}>
          <Save /> {save.isPending ? t('paymentAccounts.saving') : t('paymentAccounts.save')}
        </Button>
      </DialogFooter>
    </>
  )
}

// 渠道草稿：payType 在"编辑既有渠道"时不可改（它就是 upsert 的寻址键）。
type ChannelDraft = {
  payType: string
  enabled: boolean
  currency: string
  rate: string
  min: string
  max: string
  isNew: boolean
}

// ChannelSection 渠道子表 + 行内编辑器（按 pay_type upsert / 单独删）。
function ChannelSection({
  account,
  onView,
}: {
  account: PaymentAccount
  onView: (view: PaymentAccount) => void
}) {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const spec = providerSpec(account.provider)
  const capabilities = spec?.capabilities ?? []

  // 新建渠道的默认值：优先挑一个"还没被该账户占用"的 pay_type
  const startNewDraft = (): ChannelDraft => {
    const used = new Set(account.channels.map(c => c.pay_type))
    const free = capabilities.find(c => !used.has(c)) ?? capabilities[0] ?? ''
    return { payType: free, enabled: true, currency: 'CNY', rate: '', min: '1', max: '1000', isNew: true }
  }
  const [draft, setDraft] = useState<ChannelDraft | null>(null)
  const [rowErr, setRowErr] = useState<string | null>(null)

  const upsert = useMutation({
    mutationFn: (d: ChannelDraft) =>
      api.putPaymentAccountChannel(account.key, d.payType, {
        enabled: d.enabled,
        currency: d.currency,
        // 汇率 ×1e6 落库：契约面是整数（禁 float 传输），前端在这里一次性换算。
        rate_micro: Math.round(Number(d.rate) * MICRO),
        min_usd: Number(d.min),
        max_usd: Number(d.max),
      }),
    onSuccess: view => {
      onView(view)
      setDraft(null)
      setRowErr(null)
      toast.add({ title: t('paymentAccounts.channelSaved'), type: 'success' })
    },
    onError: (e: unknown) => setRowErr(errMsg(e)),
  })

  const remove = useMutation({
    mutationFn: (payType: string) => api.deletePaymentAccountChannel(account.key, payType),
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
        {!draft && capabilities.length > 0 && (
          <Button variant="outline" size="sm" onClick={() => { setDraft(startNewDraft()); setRowErr(null) }}>
            <Plus /> {t('paymentAccounts.channelNew')}
          </Button>
        )}
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
                        onClick={() => {
                          setRowErr(null)
                          setDraft({
                            payType: ch.pay_type,
                            enabled: ch.enabled,
                            currency: ch.currency,
                            rate: rateText(ch.rate_micro),
                            min: String(ch.min_usd),
                            max: String(ch.max_usd),
                            isNew: false,
                          })
                        }}
                      >
                        {t('paymentAccounts.edit')}
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => remove.mutate(ch.pay_type)}
                        disabled={remove.isPending}
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
              {draft.isNew ? (
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

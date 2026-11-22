// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 出口代理池页面（spec proxy-pool §7）：**管理面与供应商面复用同一组件**，
// 差异只由作用域（lib/api/scope 的 kind）决定——与账号页（pages/accounts.tsx）同款惯例，
// 不复制页面、不建平行字段清单：
// - 管理面（/app/proxies，base /api/admin）：供应商功能开启时多一列 `owner_uid`（null = 平台代理），
//   删除文案按归属分流（平台代理解绑所有引用；私池代理只解绑该供应商自己的账号）。
// - 供应商面（/user/supplier/proxies，base /api/user/supplier）：没有归属列（都是自己的）、
//   没有「分配给账号」动作；出口地址旁只挂一句**固定说明**（不得用内网/回环/链路本地，
//   后端会拒绝），前端**不判定**某个地址是不是私网——判据只有后端一份（C3）。
//   失败时把后端 400 的 message 原样透出（错误体没有字段级错误清单）。
//
// 三态纪律（与后端同口径，唯一实现在 lib/proxy-form 的 buildProxyPayload）：
// 未触碰的 username/password/enabled/remark 一律不出现在请求体里；「清除」是显式 null。
//
// 批量导入（spec-proxy-import）也在这一个组件里：两面复用同一批 API 方法，切 base 即切
// 作用域；粘贴文本的解析是 lib/proxy-form 的 parseProxyImportLines（纯函数、可单测），
// 本文件只管状态与展示。
//
// 批量探测（spec batch-probe §1）同理：多选列 + 有界并发（PROBE_CONCURRENCY）逐条打**既有**
// 单条探测端点，逐行回填、末尾只刷一次列表——不加后端批量端点（探测不触发任何失效，
// 没有「N 次单条 POST = N 次调度全量重载」那个理由）。
import { useMemo, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Plus, Pencil, Trash2, Radar, Lock, Network, CircleCheck, Ban, Upload } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import type { TFunction } from 'i18next'
import { asPlainKey } from '@/lib/i18n'
import { useAccountSurface } from '@/lib/api/scope'
import { useFeatures } from '@/lib/use-features'
import { ApiError, ApiUnauthorized } from '@/lib/api/client'
import { mapWithConcurrency } from '@/lib/concurrency'
import {
  applyOutcome,
  dropIds,
  pickLive,
  probeView,
  pruneForRound,
  withTimeout,
  type ProbeLive,
  type ProbeOutcome,
} from '@/lib/probe-live'
import {
  PROBE_CALL_TIMEOUT_MS,
  PROBE_CONCURRENCY,
  PROXY_IMPORT_MAX_ROWS,
  PROXY_SCHEMES,
  buildProxyPayload,
  emptyProxyDraft,
  parseProxyImportLines,
  proxyDraftOf,
  proxyEndpoint,
  validateProxyForm,
  type ProxyConfig,
  type ProxyField,
  type ProxyFormErrors,
  type ProxyImportRowResult,
  type ProxyScheme,
  type ProxyView,
} from '@/lib/proxy-form'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Textarea } from '@/components/ui/textarea'
import { toast } from '@/components/ui/toast'
import { formatDateTime, truncate } from '@/components/fmt'

// 失败行只按 reason 分流文案：conflict 有本地化说法，其余（invalid/internal）透出后端
// 原文——那才是能定位问题的信息（行内错误带字段级说明，见 spec-proxy-import §4）。
function importReasonText(t: TFunction, r: ProxyImportRowResult) {
  return r.reason === 'conflict' ? t('proxies.nameConflict') : (r.error ?? t('proxies.importRowFailed'))
}

// 探测结论只显示在行内：拨不通 = 200 + last_probe_ok=false，不是请求失败。
// ok 只表示「TCP 拨得通」，出口 IP 与错误文本各自独立呈现（spec §3.3）：错误文本与 ok 无关
// （ok=true 且非空 ⇔ TCP 通、取 IP 失败），且是**后端原文**——中英混杂是有意的，不要二次包装。
// 三个入参各管一种状态：`p` = 列表快照或服务端结论（`probeView` 已合并），`pending` = 本轮
// 正在跑还没结论，`live` = 本轮结论（只有它是 `requestFailed` 时才渲染**第四态**）。
// 第四态与「不可达」必须分开：前者是「浏览器↔服务器」失败（4xx/5xx/网络/超时），后者是
// 服务端给出的探测结论；把前者写成 last_probe_ok=false 就等于伪造结论（§3.3 三态矩阵之外的态）。
function ProbeCell({ p, pending = false, live }: { p: ProxyView; pending?: boolean; live?: ProbeOutcome }) {
  const { t } = useTranslation()
  if (pending) return <span className="text-muted-foreground">{t('proxies.batchProbeInFlight')}</span>
  if (live?.kind === 'requestFailed') {
    return (
      <div className="space-y-0.5">
        <div className="text-destructive">{t('proxies.batchProbeRequestFailed', { message: live.message })}</div>
        <div className="text-muted-foreground">{formatDateTime(live.at)}</div>
      </div>
    )
  }
  if (p.last_probe_at == null) return <span className="text-muted-foreground">{t('proxies.probeNever')}</span>
  return (
    <div className="space-y-0.5">
      <div className={p.last_probe_ok ? 'text-emerald-600 dark:text-emerald-400' : 'text-destructive'}>
        {p.last_probe_ok ? t('proxies.probeOK') : t('proxies.probeFailed')}
        <span className="ml-1.5 text-muted-foreground">{formatDateTime(p.last_probe_at)}</span>
      </div>
      {p.last_probe_ip && (
        <div className="flex items-center gap-1 select-text">
          <span className="text-muted-foreground">{t('proxies.probeEgressIP')}</span>
          <span className="font-mono tabular-nums">{p.last_probe_ip}</span>
        </div>
      )}
      {p.last_probe_error && (
        <div className="truncate text-muted-foreground" title={p.last_probe_error}>{truncate(p.last_probe_error, 80)}</div>
      )}
    </div>
  )
}

export default function ProxyPoolPage() {
  const { kind, api } = useAccountSurface()
  const isSupplier = kind === 'supplier'
  // 「归属」列只在管理面、且供应商功能开启时展示（加载中 / 失败按关闭）：关闭时供应商面
  // 不挂载，归属是供应商概念，与菜单、账号表单同一开关收口。
  const { supplier } = useFeatures()
  const showOwner = !isSupplier && supplier
  const { t } = useTranslation()
  const qc = useQueryClient()

  // 查询键带作用域：同一浏览器里管理面/供应商面切换不得互相污染缓存。
  const q = useQuery({ queryKey: ['proxies', kind], queryFn: () => api.listProxies() })
  const rows = q.data?.rows ?? []
  const invalidate = () => qc.invalidateQueries({ queryKey: ['proxies', kind] })
  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)

  // —— 探测相关状态（单条与批量共用一个 live 表）——
  // `probing === null` 即未在跑：按钮文案、行内占位、行内动作的禁用都从它派生。
  const [probing, setProbing] = useState<{ done: number; total: number } | null>(null)
  const [live, setLive] = useState<ProbeLive>(() => new Map())
  const running = probing !== null
  // 轮次令牌（B）：每次跑批 +1。上一轮的收尾（还在等列表刷新）只允许动**自己那一轮**的条目，
  // 否则它会把新一轮刚回填的结果删掉（`探测中 x/N` 会跳着走）。
  const runRef = useRef(0)
  // 批次快照：跑批期间 rows 会被刷新/编辑换掉（保存一条代理就会 invalidate），所以「谁在跑、
  // 结果归谁」一律按**开始那一刻**的 id 判定，而不是每帧从 rows/selected 重算——否则在跑的
  // 行会突然丢掉占位、或结果落到别的行上。`selectedIds` 只负责按钮计数与全选态。
  const [batchIds, setBatchIds] = useState<number[] | null>(null)
  const batchSet = batchIds === null ? null : new Set(batchIds)
  const inBatch = (id: number) => batchSet !== null && batchSet.has(id)
  // 行内即时结果的归属：**跑批期间只认批次快照**；跑完 batchIds 清空后，留下的只为
  // 「请求失败」那种服务端没有的结论（成功结论由刷新后的列表取代）。「服务端是否已有更新
  // 结论」的取舍在 pickLive（渲染时按行判定，纯函数、有单测）。
  const liveFor = (id: number): ProbeOutcome | undefined => (running && !inBatch(id) ? undefined : live.get(id))

  // —— 对话框（创建/编辑共用一个）——
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<ProxyView | null>(null)
  const [draft, setDraft] = useState(emptyProxyDraft())
  const [clearPassword, setClearPassword] = useState(false)
  const [clearUsername, setClearUsername] = useState(false)
  const [errors, setErrors] = useState<ProxyFormErrors>({})
  const [deleting, setDeleting] = useState<ProxyView | null>(null)

  const openCreate = () => {
    setEditing(null)
    setDraft(emptyProxyDraft())
    setClearPassword(false)
    setClearUsername(false)
    setErrors({})
    setDialogOpen(true)
  }
  const openEdit = (p: ProxyView) => {
    setEditing(p)
    setDraft(proxyDraftOf(p))
    setClearPassword(false)
    setClearUsername(false)
    setErrors({})
    setDialogOpen(true)
  }

  const save = useMutation({
    mutationFn: () => {
      const payload = buildProxyPayload(editing, draft, { clearPassword, clearUsername })
      return editing ? api.updateProxy(editing.id, payload) : api.createProxy(payload)
    },
    onSuccess: () => {
      void invalidate()
      // 这一行已被服务端重新决定（改名/改端点/清除凭据）⇒ 本地探测快照立刻作废。
      if (editing) setLive(prev => dropIds(prev, [editing.id]))
      setDialogOpen(false)
      toast.add({ title: t(editing ? 'proxies.updateSuccess' : 'proxies.createSuccess'), type: 'success' })
    },
  })
  const remove = useMutation({
    mutationFn: (id: number) => api.deleteProxy(id),
    onSuccess: (_res, id) => {
      void invalidate()
      setDeleting(null)
      // 行没了 ⇒ 结果与选中都要跟着走（否则统计/全选会把幽灵 id 算进来）。
      setLive(prev => dropIds(prev, [id]))
      setSelected(s => s.filter(x => x !== id))
      toast.add({ title: t('proxies.deleteSuccess'), type: 'success' })
    },
  })
  // 探测：拨不通**不是** API 错误（200 + last_probe_ok=false），所以这里不按失败处理——
  // 只把服务端写回的结论刷新到行内。
  const probe = useMutation({
    mutationFn: (id: number) => api.probeProxy(id),
    onSuccess: (_row, id) => {
      void invalidate()
      // 服务端刚给出更新的结论 ⇒ 批量那一轮留在行内的本地结论（含请求失败）不再需要。
      setLive(prev => dropIds(prev, [id]))
    },
  })

  // —— 批量探测（spec batch-probe §1）：前端有界并发扇出 ——
  // 选中集按 id 保存，跑完**不清空**（失败的常常要重试）；行集合随刷新变化时按当前 rows
  // 过滤（本页无分页，rows 即全集），已消失的 id 不会被送进请求。
  // 不套 useMemo：rows 本身每次渲染都可能换身份，缓存不了，反而让每次渲染多一层。
  const [selected, setSelected] = useState<number[]>([])
  const selectedIds = rows.filter(p => selected.includes(p.id)).map(p => p.id)
  const selectedSet = new Set(selectedIds)
  const allChecked = rows.length > 0 && rows.every(p => selectedSet.has(p.id))
  const someChecked = rows.some(p => selectedSet.has(p.id))
  const toggleRow = (id: number) => setSelected(s => (s.includes(id) ? s.filter(x => x !== id) : [...s, id]))
  const toggleAll = (c: boolean) =>
    setSelected(s => {
      const ids = rows.map(p => p.id)
      return c ? Array.from(new Set([...s, ...ids])) : s.filter(x => !ids.includes(x))
    })

  // running 与进度、live 表、轮次令牌、批次快照都在上方声明（单条探测也要用它们）。
  // 单条探测：调用 + 页面侧上限；**永不 reject**（一切失败都转成 requestFailed 结论）。
  // 超时也归这一支：悬挂的 fetch 没有上限，`running` 会永久为真 ⇒ 按钮全锁。
  const probeOnce = async (id: number): Promise<ProbeOutcome> => {
    try {
      const row = await withTimeout(
        api.probeProxy(id),
        PROBE_CALL_TIMEOUT_MS,
        t('proxies.batchProbeTimedOut', { seconds: PROBE_CALL_TIMEOUT_MS / 1000 }),
      )
      return { kind: 'row', row, at: row.last_probe_at ?? new Date().toISOString() }
    } catch (e) {
      return { kind: 'requestFailed', message: (e as Error)?.message ?? '', at: new Date().toISOString() }
    }
  }

  const runBatchProbe = async () => {
    const ids = selectedIds
    if (ids.length === 0 || running) return
    const myRun = runRef.current + 1
    runRef.current = myRun
    setLive(new Map()) // 新一轮从干净的表开始（旧结论由列表快照/本轮结果提供）
    setBatchIds(ids)
    setProbing({ done: 0, total: ids.length })
    // 三档结论各自计数（§3.3：ok=true 但 last_probe_error 非空 ⇔ TCP 通、取 IP 失败，
    // 不能算「成功」）；浏览器↔服务器的失败（4xx/5xx/网络/超时）是第四类，单独计。
    let reachable = 0
    let unreachable = 0
    let egressFailed = 0
    let requestFailed = 0
    try {
      // 调用方回调**不可能** reject：probeOnce 自带 try/catch（探测调用、响应字段、t() 全在
      // 里面），计数与 applyOutcome 都是纯算术/不可变 Map 操作，两个 setState 的 updater 也不抛。
      // ⇒ mapWithConcurrency 不会因单条失败整体拒绝，finally 只负责收尾，不是错误兜底。
      await mapWithConcurrency(ids, PROBE_CONCURRENCY, async id => {
        const outcome = await probeOnce(id)
        if (outcome.kind === 'requestFailed') requestFailed++
        else if (!outcome.row.last_probe_ok) unreachable++
        else if (outcome.row.last_probe_error) egressFailed++
        else reachable++
        setLive(prev => applyOutcome(prev, id, outcome))
        setProbing(prev => (prev ? { done: prev.done + 1, total: prev.total } : prev))
      })
    } finally {
      setProbing(null)
      setBatchIds(null)
      // **只在这里刷一次**：单条 probe mutation 的 onSuccess 是「每条都刷」，批量路径不得复用它。
      await invalidate()
      // `invalidateQueries` 在 refetch 失败时**仍然 resolve**（query-core 对不抛错的重取吞掉
      // rejection）⇒ 判据只能是 query 的 error 状态。刷新失败时列表还是旧数据，这时清掉本轮
      // 结论会让行退回「从未探测」——结论比旧快照新，整份保留。
      const refreshError = qc.getQueryState(['proxies', kind])?.error
      if (refreshError == null) {
        // 服务端已把 last_probe_* 落库 ⇒ 成功结论由列表取代；请求失败服务端没有，留在行内。
        // 轮次令牌兜底：若这期间又起了新一轮，这次收尾**不得动**它的条目。
        setLive(prev => pruneForRound(prev, myRun, runRef.current))
      }
      toast.add({
        title: t('proxies.batchProbeOutcomes', { reachable, unreachable, egressFailed, requestFailed }),
        // 刷新失败不新造文案：复用页面既有的「加载失败：{{message}}」告诉用户行内是旧列表 + 新结论。
        description: refreshError == null ? undefined : t('common.loadFailed', { message: (refreshError as Error)?.message ?? '' }),
        type: reachable === ids.length ? 'success' : 'error',
      })
    }
  }

  // —— 批量导入（spec-proxy-import）——
  // `submitted` 是**提交那一刻**的行快照（行号 + 派生名）：响应的 index 是请求下标，
  // 只有靠它才能把结果标回原文那一行。失败行留在框里让人改完再提，故结果面板不清空。
  const [importOpen, setImportOpen] = useState(false)
  const [importText, setImportText] = useState('')
  const [importScheme, setImportScheme] = useState<ProxyScheme>('http')
  const [importEnabled, setImportEnabled] = useState(true)
  const [submitted, setSubmitted] = useState<{ line: number; name: string }[]>([])

  const parsed = useMemo(() => parseProxyImportLines(importText, importScheme), [importText, importScheme])
  const importable = useMemo(() => parsed.filter(r => r.config !== undefined), [parsed])
  const broken = parsed.length - importable.length
  const overCap = importable.length > PROXY_IMPORT_MAX_ROWS

  const importMut = useMutation({
    mutationFn: (rows: ProxyConfig[]) => api.importProxies(rows.map(r => ({ ...r, enabled: importEnabled }))),
    onSuccess: res => {
      void invalidate()
      if (res.failed === 0) {
        toast.add({ title: t('proxies.importSuccess', { count: res.created }), type: 'success' })
        setImportOpen(false)
        setImportText('')
        setSubmitted([])
      }
    },
  })

  const openImport = () => {
    setImportText('')
    setSubmitted([])
    setImportScheme('http')
    setImportEnabled(true)
    importMut.reset()
    setImportOpen(true)
  }

  const submitImport = () => {
    const rows = importable.map(r => r.config as ProxyConfig)
    setSubmitted(importable.map(r => ({ line: r.line, name: (r.config as ProxyConfig).name })))
    importMut.mutate(rows)
  }

  const submit = () => {
    const errs = validateProxyForm(draft)
    setErrors(errs)
    if (Object.keys(errs).length > 0) return
    save.mutate()
  }

  const fieldErr = (k: ProxyField) => {
    const code = errors[k]
    if (!code) return null
    return <p className="text-xs text-destructive">{t(asPlainKey(`proxies.errors.${code}`))}</p>
  }
  const schemeItems = useMemo(() => Object.fromEntries(PROXY_SCHEMES.map(s => [s, s])), [])
  const schemaErr = save.isError && save.error instanceof ApiError && save.error.status === 409

  // 行内探测单元格：`p` 是列表快照（`probeView` 会合上「本轮的服务端结论」），`live` 供第四态
  // （请求失败）单独渲染，`pending` 只给「本轮在跑、还没结论」的行。
  const probeCell = (p: ProxyView) => {
    const outcome = pickLive(p, liveFor(p.id))
    return <ProbeCell p={probeView(p, outcome)} live={outcome} pending={running && inBatch(p.id) && outcome === undefined} />
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div className="min-w-0">
          <h1 className="text-2xl font-semibold tracking-tight">{t('proxies.title')}</h1>
          <p className="text-sm text-muted-foreground text-pretty">
            {t(isSupplier ? 'proxies.subtitleSupplier' : supplier ? 'proxies.subtitleAdmin' : 'proxies.subtitleAdminPlatform')}
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Button
            data-od-id="proxy-batch-probe"
            variant="outline"
            onClick={() => void runBatchProbe()}
            disabled={running || selectedIds.length === 0}
          >
            <Radar />
            {running
              ? t('proxies.batchProbeRunning', { done: probing?.done ?? 0, total: probing?.total ?? 0 })
              : t('proxies.batchProbe', { count: selectedIds.length })}
          </Button>
          <Button variant="outline" onClick={openImport}><Upload /> {t('proxies.import')}</Button>
          <Button onClick={openCreate}><Plus /> {t('proxies.new')}</Button>
        </div>
      </div>

      {q.isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: (q.error as Error).message })}</p>
      ) : q.isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : rows.length === 0 ? (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <Network className="size-10" />
            <p className="font-medium">{t('proxies.emptyTitle')}</p>
            <p className="text-sm text-center text-pretty">{t(isSupplier ? 'proxies.emptyDescSupplier' : 'proxies.emptyDescAdmin')}</p>
            <Button className="mt-2" onClick={openCreate}><Plus /> {t('proxies.new')}</Button>
          </Card>
        </motion.div>
      ) : (
        <ScrollArea data-od-id="table-scroll-proxies" className="rounded-[14px] border border-[rgba(19,45,83,0.26)] bg-[color:var(--glass-card-light)] shadow-[inset_0_1px_0_rgba(255,255,255,0.5),0_10px_36px_rgba(19,45,83,0.16)] backdrop-blur-[var(--glass-blur)] dark:bg-[color:var(--glass-card-dark)] dark:shadow-[inset_0_1px_0_rgba(255,255,255,0.07),0_10px_36px_rgba(2,6,14,0.5)] dark:border-[rgba(148,180,220,0.32)]" showHorizontal>
          <Table className="min-w-[980px]" containerClassName="overflow-x-visible border-0 shadow-none rounded-none bg-transparent backdrop-blur-none">
            <TableHeader>
              <TableRow>
                <TableHead className="w-10">
                  <Checkbox
                    checked={allChecked}
                    indeterminate={someChecked && !allChecked}
                    onCheckedChange={c => toggleAll(c === true)}
                    aria-label={t('proxies.batchProbeSelected', { count: selectedIds.length })}
                  />
                </TableHead>
                <TableHead className="w-12">ID</TableHead>
                <TableHead className="w-48">{t('proxies.table.name')}</TableHead>
                <TableHead className="w-64">{t('proxies.table.endpoint')}</TableHead>
                <TableHead className="w-44">{t('proxies.table.auth')}</TableHead>
                <TableHead className="w-20">{t('proxies.table.enabled')}</TableHead>
                <TableHead className="w-64">{t('proxies.table.probe')}</TableHead>
                {showOwner && <TableHead className="w-28">{t('proxies.table.owner')}</TableHead>}
                <TableHead className="w-28 text-right">{t('proxies.table.actions')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody className="[&_td]:py-3">
              {rows.map(p => (
                <TableRow key={p.id} data-state={selectedSet.has(p.id) ? 'selected' : undefined}>
                  <TableCell>
                    <Checkbox checked={selectedSet.has(p.id)} onCheckedChange={() => toggleRow(p.id)} />
                  </TableCell>
                  <TableCell className="tabular-nums">{p.id}</TableCell>
                  <TableCell className="max-w-48">
                    <div className="truncate" title={p.name}>{p.name}</div>
                    {p.remark ? <div className="truncate text-xs text-muted-foreground" title={p.remark}>{truncate(p.remark, 60)}</div> : null}
                  </TableCell>
                  <TableCell className="font-mono text-xs">{proxyEndpoint(p)}</TableCell>
                  <TableCell>
                    {p.username
                      ? (
                        <span className="inline-flex items-center gap-1.5 text-xs">
                          {p.has_password && <Lock className="size-3.5 text-muted-foreground" aria-label={t('proxies.authPasswordSet')} />}
                          <span className="truncate" title={p.username}>{p.username}</span>
                        </span>
                      )
                      : <span className="text-xs text-muted-foreground">{t('proxies.authNone')}</span>}
                  </TableCell>
                  <TableCell>
                    <span className="inline-flex items-center gap-1.5 text-xs">
                      {p.enabled
                        ? <CircleCheck className="size-4 text-emerald-500" aria-label={t('accounts.state.enabled')} />
                        : <Ban className="size-4 text-muted-foreground" aria-label={t('accounts.state.disabled')} />}
                      {p.enabled ? t('accounts.state.enabled') : t('accounts.state.disabled')}
                    </span>
                  </TableCell>
                  <TableCell className="text-xs">{probeCell(p)}</TableCell>
                  {showOwner && (
                    <TableCell className="text-xs">
                      {p.owner_uid == null
                        ? <Badge variant="outline">{t('proxies.ownerPlatform')}</Badge>
                        : <span className="tabular-nums">{p.owner_uid}</span>}
                    </TableCell>
                  )}
                  <TableCell className="text-right">
                    <div className="inline-flex items-center gap-1">
                      {/* 跑批期间禁止改/删：批次快照里的行在途中被编辑或删除，结论归属就说不清了。 */}
                      <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(p)} disabled={running}>
                        <Pencil />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        title={t('proxies.probeAction')}
                        onClick={() => probe.mutate(p.id)}
                        disabled={probe.isPending || running}
                      >
                        <Radar />
                      </Button>
                      <Button variant="ghost" size="icon-sm" title={t('common.delete')} onClick={() => setDeleting(p)} disabled={running}>
                        <Trash2 className="text-destructive" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </ScrollArea>
      )}

      {probe.isError && errMsg(probe.error) && (
        <p className="text-sm text-destructive">{errMsg(probe.error)}</p>
      )}

      {/* —— 创建/编辑对话框 —— */}
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{editing ? t('proxies.editTitle', { id: editing.id }) : t('proxies.newTitle')}</DialogTitle>
            <DialogDescription>{t('proxies.dialogDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="px-name">{t('proxies.nameLabel')}</Label>
              <Input id="px-name" value={draft.name} placeholder={t('proxies.namePlaceholder')} onChange={e => setDraft(d => ({ ...d, name: e.target.value }))} />
              {fieldErr('name')}
            </div>
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label>{t('proxies.schemeLabel')}</Label>
                <Select items={schemeItems} value={draft.scheme} onValueChange={v => setDraft(d => ({ ...d, scheme: v }))}>
                  <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    {PROXY_SCHEMES.map(s => <SelectItem key={s} value={s} label={s}>{s}</SelectItem>)}
                  </SelectContent>
                </Select>
                {fieldErr('scheme')}
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="px-port">{t('proxies.portLabel')}</Label>
                <Input id="px-port" inputMode="numeric" value={draft.port} placeholder="1080" onChange={e => setDraft(d => ({ ...d, port: e.target.value }))} />
                {fieldErr('port')}
              </div>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="px-host">{t('proxies.hostLabel')}</Label>
              <Input id="px-host" value={draft.host} placeholder={t('proxies.hostPlaceholder')} onChange={e => setDraft(d => ({ ...d, host: e.target.value }))} />
              {fieldErr('host')}
              {/* 固定说明，不随输入变化：地址是否可用由后端判（见下方 save 错误位）。 */}
              {isSupplier && (
                <p className="text-xs text-muted-foreground">{t('proxies.hostPolicyHint')}</p>
              )}
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="px-user">{t('proxies.usernameLabel')}</Label>
              <Input
                id="px-user"
                value={draft.username}
                disabled={clearUsername}
                placeholder={t('proxies.usernamePlaceholder')}
                onChange={e => setDraft(d => ({ ...d, username: e.target.value }))}
              />
              {editing && (
                <label className="flex cursor-pointer items-center gap-2.5 py-0.5">
                  <Checkbox checked={clearUsername} onCheckedChange={c => setClearUsername(c === true)} />
                  <span className="text-sm">{t('proxies.clearUsername')}</span>
                </label>
              )}
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="px-pass">{t('proxies.passwordLabel')}</Label>
              <Input
                id="px-pass"
                type="password"
                value={draft.password}
                disabled={clearPassword}
                placeholder={t('proxies.passwordPlaceholder')}
                onChange={e => setDraft(d => ({ ...d, password: e.target.value }))}
              />
              <p className="text-xs text-muted-foreground">{t(editing && editing.has_password ? 'proxies.passwordKeepHint' : 'proxies.passwordHint')}</p>
              {editing && (
                <label className="flex cursor-pointer items-center gap-2.5 py-0.5">
                  <Checkbox checked={clearPassword} onCheckedChange={c => setClearPassword(c === true)} />
                  <span className="text-sm">{t('proxies.clearPassword')}</span>
                </label>
              )}
            </div>
            <label className="flex cursor-pointer items-center gap-2.5 py-0.5">
              <Checkbox checked={draft.enabled} onCheckedChange={c => setDraft(d => ({ ...d, enabled: c === true }))} />
              <span className="text-sm">{t('proxies.enabledLabel')}</span>
            </label>
            <p className="text-xs text-muted-foreground">{t('proxies.enabledHint')}</p>
            <div className="space-y-1.5">
              <Label htmlFor="px-remark">{t('proxies.remarkLabel')}</Label>
              <Input id="px-remark" value={draft.remark} placeholder={t('proxies.remarkPlaceholder')} onChange={e => setDraft(d => ({ ...d, remark: e.target.value }))} />
            </div>
            {/* 提交失败：409 有专门的冲突文案；其余（含 400 的内网/回环地址拒绝）直接
                展示后端 message —— 错误体没有字段级错误清单，前端不猜是哪个字段。 */}
            {save.isError && (
              <p className="text-sm text-destructive">
                {schemaErr ? t('proxies.nameConflict') : errMsg(save.error)}
              </p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)}>{t('common.cancel')}</Button>
            <Button onClick={submit} disabled={save.isPending}>
              {save.isPending ? t('common.saving') : editing ? t('common.saveChanges') : t('common.create')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 批量导入（spec-proxy-import）：粘贴 → 预览 → 逐行结果 —— */}
      <Dialog open={importOpen} onOpenChange={setImportOpen}>
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>{t('proxies.importTitle')}</DialogTitle>
            <DialogDescription>{t('proxies.importDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <Textarea
              data-od-id="proxy-import-text"
              className="min-h-32 font-mono text-xs"
              placeholder={t('proxies.importPlaceholder')}
              value={importText}
              onChange={e => setImportText(e.target.value)}
            />
            <div className="grid grid-cols-1 items-end gap-3 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label>{t('proxies.importDefaultScheme')}</Label>
                <Select items={schemeItems} value={importScheme} onValueChange={v => setImportScheme(v as ProxyScheme)}>
                  <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    {PROXY_SCHEMES.map(s => <SelectItem key={s} value={s} label={s}>{s}</SelectItem>)}
                  </SelectContent>
                </Select>
              </div>
              <label className="flex cursor-pointer items-center gap-2.5 py-2">
                <Checkbox checked={importEnabled} onCheckedChange={c => setImportEnabled(c === true)} />
                <span className="text-sm">{t('proxies.importEnabled')}</span>
              </label>
            </div>

            {parsed.length > 0 && (
              <div className="max-h-52 overflow-auto rounded-md border">
                <table className="w-full text-xs">
                  <tbody>
                    {parsed.map(r => (
                      <tr key={r.line} className="border-t first:border-t-0">
                        <td className="w-12 px-2 py-1 tabular-nums text-muted-foreground">L{r.line}</td>
                        <td className="max-w-[16rem] truncate px-2 py-1 font-mono" title={r.raw}>{r.raw}</td>
                        <td className="px-2 py-1">
                          {r.error
                            ? <span className="text-destructive">{t(asPlainKey(`proxies.importErrors.${r.error}`))}</span>
                            : <span className="text-muted-foreground">{r.config?.name}</span>}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}

            <p className="text-xs text-muted-foreground">
              {t('proxies.importParsed', { ok: importable.length, bad: broken })}
              {overCap ? ` · ${t('proxies.importOverCap', { max: PROXY_IMPORT_MAX_ROWS })}` : ''}
            </p>

            {importMut.isError && errMsg(importMut.error) && (
              <p className="text-sm text-destructive">{errMsg(importMut.error)}</p>
            )}

            {/* 逐行结果：下标是**请求下标**，行号来自提交时的快照（submitted）。 */}
            {importMut.data && (
              <div className="max-h-40 space-y-0.5 overflow-auto rounded-md border p-2 text-xs">
                {importMut.data.rows.map(r => {
                  const at = submitted[r.index]
                  return (
                    <div key={r.index} className="flex items-center gap-2">
                      <span className="w-10 shrink-0 tabular-nums text-muted-foreground">L{at?.line ?? r.index + 1}</span>
                      <span className={r.ok ? 'shrink-0 text-emerald-600 dark:text-emerald-400' : 'shrink-0 text-destructive'}>
                        {r.ok ? t('proxies.importRowOK') : importReasonText(t, r)}
                      </span>
                      <span className="truncate font-mono text-muted-foreground" title={at?.name ?? r.name}>{at?.name ?? r.name}</span>
                    </div>
                  )
                })}
              </div>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setImportOpen(false)}>{t('common.cancel')}</Button>
            <Button onClick={submitImport} disabled={importMut.isPending || importable.length === 0 || overCap}>
              {importMut.isPending ? t('common.saving') : t('proxies.importSubmit', { count: importable.length })}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 删除确认：文案随归属分流（spec §3.6 的连坐范围） —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !remove.isPending) setDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('proxies.deleteTitle')}</DialogTitle>
            <DialogDescription>
              {isSupplier || deleting?.owner_uid != null
                ? t('proxies.deleteDescPrivate', { name: deleting?.name ?? '' })
                : t('proxies.deleteDescPlatform', { name: deleting?.name ?? '' })}
            </DialogDescription>
          </DialogHeader>
          {remove.isError && errMsg(remove.error) && (
            <p className="text-sm text-destructive">{errMsg(remove.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)} disabled={remove.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.id)} disabled={remove.isPending}>
              {remove.isPending ? t('common.deleting') : t('common.confirmDelete')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 用户端个人中心页：账户信息卡（me()——Email/Role/Status/MaxConcurrency/CreatedAt）
// + 临时额度区块（/user/temp-balances——合计 USD + 有效额度列表，FEFO 序即响应序；
// 空结果显示"无临时额度"）+ 余额预警（BalanceWarningCard）+ 修改密码表单（/user/auth/change-password）。
// 单位语义：me().Balance 与 temp-balances 的 amount_usd/total_usd 均为 API 边界
// 已换算的 USD 直显（formatUSD，勿用毫分语义的 formatCost）。
import { useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { KeyRound, Timer, UserRound } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { ApiError, ApiUnauthorized, userApi } from '@/lib/api/client'
import { BalanceWarningCard } from '@/components/balance-warning-card'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { StatusBadge } from '@/components/status-badge'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { toast } from '@/components/ui/toast'
import { formatDateTime, formatUSD } from '@/components/fmt'

const fadeUp = {
  initial: { opacity: 0, y: 12 },
  animate: { opacity: 1, y: 0 },
}

function InfoRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-3">
      <span className="text-sm text-muted-foreground">{label}</span>
      <span className="text-sm">{children}</span>
    </div>
  )
}

export default function UserProfile() {
  const { t } = useTranslation()

  const meQ = useQuery({ queryKey: ['user', 'me'], queryFn: () => userApi.me() })
  const tempQ = useQuery({ queryKey: ['user', 'temp-balances'], queryFn: () => userApi.getTempBalances() })

  const [oldPwd, setOldPwd] = useState('')
  const [newPwd, setNewPwd] = useState('')
  const [confirm, setConfirm] = useState('')
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(false)

  const submit = async () => {
    if (!oldPwd || !newPwd || !confirm) { setErr(t('user.profile.pwdRequired')); return }
    if (newPwd !== confirm) { setErr(t('user.profile.pwdMismatch')); return }
    setErr('')
    setLoading(true)
    try {
      await userApi.changePassword({ old_password: oldPwd, new_password: newPwd })
      setOldPwd(''); setNewPwd(''); setConfirm('')
      toast.add({ title: t('user.profile.successTitle'), description: t('user.profile.successDesc'), type: 'success' })
    } catch (e) {
      if (e instanceof ApiUnauthorized) setErr(t('user.profile.oldPasswordWrong'))
      else if (e instanceof ApiError && e.status === 400) setErr(t('user.profile.newPasswordInvalid'))
      else setErr(e instanceof ApiError ? e.message : t('user.common.error'))
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t('user.profile.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('user.profile.subtitle')}</p>
      </div>

      <div className="grid grid-cols-1 gap-5 xl:grid-cols-2">
        <motion.div {...fadeUp} transition={{ duration: 0.25 }}>
          <Card className="h-full">
            <CardHeader>
              <CardDescription className="flex items-center gap-1.5">
                <UserRound className="size-4" /> {t('user.profile.accountTitle')}
              </CardDescription>
            </CardHeader>
            <CardContent className="space-y-3">
              {meQ.isError ? (
                <p className="text-sm text-destructive">{t('common.loadFailed', { message: (meQ.error as Error).message })}</p>
              ) : meQ.isLoading ? (
                <div className="space-y-2">
                  {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-5" />)}
                </div>
              ) : (
                <>
                  <InfoRow label={t('user.auth.email')}>{meQ.data?.Email ?? '—'}</InfoRow>
                  <InfoRow label={t('users.roleLabel')}>
                    {meQ.data?.Role ? t(`users.role.${meQ.data.Role}`) : '—'}
                  </InfoRow>
                  <InfoRow label={t('users.statusLabel')}>
                    <StatusBadge status={meQ.data?.Status} />
                  </InfoRow>
                  <InfoRow label={t('user.overview.maxConcurrency')}>
                    {meQ.data?.MaxConcurrency == null ? '—' : meQ.data.MaxConcurrency === 0 ? t('user.overview.unlimited') : meQ.data.MaxConcurrency}
                  </InfoRow>
                  <InfoRow label={t('user.overview.createdAt')}>
                    {meQ.data?.CreatedAt ? formatDateTime(meQ.data.CreatedAt) : '—'}
                  </InfoRow>
                </>
              )}
            </CardContent>
          </Card>
        </motion.div>

        <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.06 }}>
          <Card className="h-full">
            <CardHeader>
              <CardDescription className="flex items-center gap-1.5">
                <Timer className="size-4" /> {t('user.profile.tempTitle')}
              </CardDescription>
              {tempQ.data && tempQ.data.rows.length > 0 && (
                <CardTitle className="text-2xl font-semibold tabular-nums">
                  {formatUSD(tempQ.data.total_usd)}
                </CardTitle>
              )}
            </CardHeader>
            <CardContent>
              {tempQ.isError ? (
                <p className="text-sm text-destructive">{t('common.loadFailed', { message: (tempQ.error as Error).message })}</p>
              ) : tempQ.isLoading ? (
                <div className="space-y-2">
                  {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-8" />)}
                </div>
              ) : tempQ.data.rows.length === 0 ? (
                <p className="py-6 text-center text-sm text-muted-foreground">{t('user.profile.tempEmpty')}</p>
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>{t('user.profile.tempAmount')}</TableHead>
                      <TableHead>{t('user.profile.tempExpiresAt')}</TableHead>
                      <TableHead>{t('user.profile.tempNote')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody className="[&_td]:py-3">
                    {tempQ.data.rows.map(r => (
                      <TableRow key={r.id}>
                        <TableCell className="tabular-nums">{formatUSD(r.amount_usd)}</TableCell>
                        <TableCell className="text-xs">{r.expires_at ? formatDateTime(r.expires_at) : t('user.profile.tempPermanent')}</TableCell>
                        <TableCell className="max-w-40 truncate text-xs" title={r.note ?? undefined}>{r.note || '—'}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
            </CardContent>
          </Card>
        </motion.div>
      </div>

      <BalanceWarningCard />

      <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.12 }}>
        <Card className="max-w-xl">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <KeyRound className="size-4" /> {t('user.profile.pwdTitle')}
            </CardTitle>
            <CardDescription>{t('user.profile.pwdSubtitle')}</CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="up-old">{t('user.profile.oldPassword')}</Label>
              <Input id="up-old" type="password" autoComplete="current-password" value={oldPwd} onChange={e => { setOldPwd(e.target.value); setErr('') }} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="up-new">{t('user.profile.newPassword')}</Label>
              <Input id="up-new" type="password" autoComplete="new-password" value={newPwd} onChange={e => { setNewPwd(e.target.value); setErr('') }} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="up-confirm">{t('user.profile.confirmPassword')}</Label>
              <Input id="up-confirm" type="password" autoComplete="new-password" value={confirm} onChange={e => { setConfirm(e.target.value); setErr('') }} onKeyDown={e => { if (e.key === 'Enter') submit() }} />
            </div>
            {err && <p className="text-sm text-destructive">{err}</p>}
            <Button disabled={loading} onClick={submit}>
              {loading ? t('user.profile.submitting') : t('user.profile.submit')}
            </Button>
          </CardContent>
        </Card>
      </motion.div>
    </div>
  )
}

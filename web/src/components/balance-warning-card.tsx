// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { BellRing } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { ApiError, userApi } from '@/lib/api/client'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { toast } from '@/components/ui/toast'

const fadeUp = {
  initial: { opacity: 0, y: 12 },
  animate: { opacity: 1, y: 0 },
}

export function BalanceWarningCard() {
  const { t } = useTranslation()
  const qc = useQueryClient()

  const thresholdQ = useQuery({
    queryKey: ['user', 'balance-warning-threshold'],
    queryFn: () => userApi.getBalanceWarningThreshold(),
  })

  const [thresholdInput, setThresholdInput] = useState('')
  const [thresholdErr, setThresholdErr] = useState('')

  useEffect(() => {
    if (thresholdQ.data) {
      setThresholdInput(String(thresholdQ.data.balance_warning_threshold))
    }
  }, [thresholdQ.data])

  const thresholdMut = useMutation({
    mutationFn: (v: number) => userApi.updateBalanceWarningThreshold({ balance_warning_threshold: v }),
    onSuccess: data => {
      qc.setQueryData(['user', 'balance-warning-threshold'], data)
      toast.add({ title: t('user.profile.balanceWarningSaved'), type: 'success' })
      setThresholdErr('')
      void qc.invalidateQueries({ queryKey: ['user', 'balance-warning-threshold'] })
    },
    onError: (e: unknown) => {
      if (e instanceof ApiError) setThresholdErr(e.message)
      else setThresholdErr(t('user.common.error'))
    },
  })

  const handleSaveThreshold = () => {
    if (thresholdMut.isPending) return
    const raw = thresholdInput.trim()
    if (raw === '') {
      setThresholdErr(t('user.profile.balanceWarningInvalid'))
      return
    }
    const n = Number(raw)
    if (!Number.isFinite(n) || n < 0) {
      setThresholdErr(t('user.profile.balanceWarningInvalid'))
      return
    }
    setThresholdErr('')
    thresholdMut.mutate(n)
  }

  const thresholdVal = thresholdQ.data?.balance_warning_threshold
  const thresholdIsDisabled = thresholdVal === 0

  return (
    <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.09 }}>
      <Card className="max-w-xl">
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <BellRing className="size-4" /> {t('user.profile.balanceWarningTitle')}
          </CardTitle>
          <CardDescription>{t('user.profile.balanceWarningDesc')}</CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          {thresholdQ.isError ? (
            <p className="text-sm text-destructive">{t('common.loadFailed', { message: (thresholdQ.error as Error).message })}</p>
          ) : thresholdQ.isLoading ? (
            <div className="space-y-2">
              <Skeleton className="h-8 w-full" />
              <Skeleton className="h-4 w-32" />
            </div>
          ) : (
            <>
              <div className="space-y-1.5">
                <Label htmlFor="bw-threshold">{t('user.profile.balanceWarningLabel')}</Label>
                <div className="flex min-w-0 items-center gap-2">
                  <div className="relative min-w-0 flex-1">
                    <span className="pointer-events-none absolute inset-y-0 left-2.5 flex items-center text-sm text-muted-foreground" aria-hidden="true">
                      $
                    </span>
                    <Input
                      id="bw-threshold"
                      type="number"
                      inputMode="decimal"
                      step="0.01"
                      min="0"
                      className="pl-6 tabular-nums"
                      value={thresholdInput}
                      disabled={thresholdMut.isPending}
                      onChange={e => {
                        setThresholdInput(e.target.value)
                        setThresholdErr('')
                      }}
                      onKeyDown={e => {
                        if (e.key === 'Enter') handleSaveThreshold()
                      }}
                      aria-describedby={thresholdErr ? 'bw-hint bw-error' : 'bw-hint'}
                      aria-invalid={thresholdErr ? true : undefined}
                    />
                  </div>
                  <Button disabled={thresholdMut.isPending} onClick={handleSaveThreshold} className="shrink-0">
                    {thresholdMut.isPending ? t('user.profile.balanceWarningSaving') : t('user.profile.balanceWarningSave')}
                  </Button>
                </div>
                <p id="bw-hint" className="text-xs text-muted-foreground">
                  {t('user.profile.balanceWarningHint')}
                  {thresholdVal != null && (
                    <span className="ml-1.5">
                      ·{' '}
                      {thresholdIsDisabled ? t('user.profile.balanceWarningDisabled') : t('user.profile.balanceWarningActive', { value: thresholdVal.toString() })}
                    </span>
                  )}
                </p>
              </div>
              {thresholdErr && (
                <p id="bw-error" className="text-sm text-destructive" role="alert">
                  {thresholdErr}
                </p>
              )}
            </>
          )}
        </CardContent>
      </Card>
    </motion.div>
  )
}

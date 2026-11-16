// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 内嵌页路由页（spec 2026-11-16 内嵌页面 §7-4 / §9-U4–U5）：
// `/app/custom/:id` 与 `/user/custom/:id` 共用同一组件，用 `scope` 区分「当前控制台
// 可见列表」。命中 → iframe 容器；未命中（不存在 / 已停用 / 可见范围不符）→ **内联**
// 空态卡，**不复用** `pages/not-found.tsx`（它是 min-h-screen，在 Shell 内会撑出双倍
// 高度，§9-U4 明文）。
import { Link, useParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { FileQuestion, RefreshCw } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { userApi } from '@/lib/api/client'
import { userAuth } from '@/lib/auth'
import { CUSTOM_PAGES_QUERY_KEY, pagesForConsole } from '@/lib/custom-pages'
import { cn } from '@/lib/utils'
import { CustomPageFrame } from '@/components/custom-page-frame'
import { Button, buttonVariants } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'

// 条目列表的 query key（spec §7.2）单点定义在 `lib/custom-pages.ts`（§19-6）：
// 设置页保存后定向失效的就是它，AppShell 的侧栏注入与面包屑读的是同一份缓存——
// 两个控制台共享，切换 /app ↔ /user 不重复请求。
export default function CustomPage({ scope }: { scope: 'admin' | 'user' }) {
  const { t } = useTranslation()
  const { id } = useParams<{ id: string }>()
  const pagesQ = useQuery({
    queryKey: CUSTOM_PAGES_QUERY_KEY,
    queryFn: () => userApi.customPages(),
    staleTime: 60_000,
    // 无 token 时不发请求（§7.2 原文要求）：本页在 RequireAuth 之内，但没有 token 的
    // 瞬时态（登出竞态）不该打出一个必然 401 的请求。
    enabled: Boolean(userAuth.getToken()),
  })

  // id 不存在、条目已停用、或可见范围不属于当前控制台都归到同一个空态：
  // 不区分「没有」与「越权」，避免把管理台专属条目的存在暗示给普通用户（§8-S5）。
  // `pagesForConsole` 再兜一次 enabled/visibility（防手改 DB / 旧缓存）。
  const page = pagesForConsole(pagesQ.data?.Pages ?? [], scope).find(p => p.ID === id)

  // 取数中 → 骨架（形状与真正的内容区一致，避免加载完成后高度跳动）。
  // 高度与 frame 的三处同值（§9-U1）：11rem 会让收起骨架的瞬间内容区高度变化 36px。
  if (pagesQ.isLoading) {
    return (
      <div className="space-y-3">
        <Skeleton className="h-7 w-48" />
        <Skeleton className="h-[calc(100dvh-13.5rem)] min-h-[420px] w-full" />
      </div>
    )
  }

  // 取数失败 → 内联错误卡 + 重试，**不走 404**（§9-U5）：404 会把「后端挂了」
  // 说成「页面不存在」，管理员会去删配置。
  if (pagesQ.isError) {
    return (
      <Card>
        <CardContent className="flex flex-col items-center gap-3 py-10 text-center">
          <p className="text-sm text-destructive">
            {t('common.loadFailed', { message: (pagesQ.error as Error).message })}
          </p>
          <Button variant="outline" size="sm" onClick={() => void pagesQ.refetch()} disabled={pagesQ.isFetching}>
            <RefreshCw className={cn(pagesQ.isFetching && 'animate-spin')} />
            {t('user.models.retry')}
          </Button>
        </CardContent>
      </Card>
    )
  }

  if (!page) {
    return (
      <Card className="items-center py-12 text-center">
        <CardHeader className="items-center">
          <CardTitle className="flex items-center gap-2">
            <FileQuestion className="size-5" />
            {t('customPage.notFoundTitle')}
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col items-center gap-4">
          <p className="text-sm text-muted-foreground">{t('customPage.notFoundDesc')}</p>
          {/* 回到**所属控制台**首页而不是站点首页 `/`：空态发生在 Shell 内，
              把用户丢出控制台只会让他再登录一次。 */}
          <Link to={scope === 'admin' ? '/app/dashboard' : '/user'} className={cn(buttonVariants({ variant: 'outline' }))}>
            {t('customPage.backHome')}
          </Link>
        </CardContent>
      </Card>
    )
  }

  // key = page.ID：换条目时重挂 frame，`onLoad`/提示行的状态不会带到下一页。
  return <CustomPageFrame key={page.ID} page={page} />
}

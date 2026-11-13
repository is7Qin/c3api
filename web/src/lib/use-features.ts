// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useQuery } from '@tanstack/react-query'
import { userApi } from '@/lib/api/client'
import { userAuth } from '@/lib/auth'

// 部署级功能开关（GET /api/user/features）的唯一读取入口：AppShell、页面与路由闸门
// 共用同一 query key，由 react-query 去重，一次会话只请求一次。值是启动配置的投影
// （改动需重启），故 staleTime 无限、不落 localStorage（持久化会读到旧值）。
// 未登录不发请求；失败沿用全局 retry: 0 与 401 拦截，不在这里另写重试或 401 分支。
//
// supplier = 开关已确定为开启；resolved = 已有确定结论（成功或失败）。加载中与失败
// 时 supplier 恒为 false——内容面据此 fail-closed，路由闸门再按 resolved 区分「加载中
// 渲染空」与「已确定关闭渲染 404」。
export function useFeatures(): { supplier: boolean; resolved: boolean } {
  const q = useQuery({
    queryKey: ['user', 'features'],
    queryFn: () => userApi.features(),
    staleTime: Infinity,
    enabled: Boolean(userAuth.getToken()),
  })
  return { supplier: q.data?.supplier === true, resolved: q.isSuccess || q.isError }
}

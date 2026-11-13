// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useQuery } from '@tanstack/react-query'
import { userApi } from '@/lib/api/client'
import { userAuth } from '@/lib/auth'

// 功能开关查询键（GET /api/user/features）：hook 与「改了开关后要重取」的调用方
// （设置页保存 payment.enabled）共用这一处字面量。
export const FEATURES_QUERY_KEY = ['user', 'features'] as const

// 功能开关（GET /api/user/features）的唯一读取入口：AppShell、页面与路由闸门
// 共用同一 query key，由 react-query 去重。staleTime 与 gcTime 都无限：成功值在页面
// 生命周期内只取一次（AppShell 卸载后回来也不重取，刷新页面才重取）；不落
// localStorage（持久化会读到旧值）。
//   - supplier：启动配置投影，改动需重启，一次会话内不会变。
//   - payment：运行时设置 payment.enabled。设置页保存该键后按 FEATURES_QUERY_KEY
//     失效重取，管理员当前页面即时生效；其余已打开的会话刷新页面后生效（服务端
//     建单仍看同一开关，旧页面不会因此卖出东西）。
// 未登录不发请求；失败沿用全局 retry: 0 与 401 拦截，不在这里另写重试或 401 分支。
// 失败不会被当作成功值缓存：新观察者挂载时重新请求一次（自愈）。
//
// supplier / payment = 开关已确定为开启；resolved = 已有确定结论（成功或失败）。加载中
// 与失败时两者恒为 false——内容面据此 fail-closed，路由闸门再按 resolved 区分「加载中
// 渲染空」与「已确定关闭渲染 404」。
export function useFeatures(): { supplier: boolean; payment: boolean; resolved: boolean } {
  const q = useQuery({
    queryKey: FEATURES_QUERY_KEY,
    queryFn: () => userApi.features(),
    staleTime: Infinity,
    gcTime: Infinity,
    enabled: Boolean(userAuth.getToken()),
  })
  return {
    supplier: q.data?.supplier === true,
    payment: q.data?.payment === true,
    resolved: q.isSuccess || q.isError,
  }
}

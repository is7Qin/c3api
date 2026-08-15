// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useEffect, useRef, useState } from 'react'

// keyset 游标分页的游标链状态机（日志页无 total/offset，替代 useQuery + 自计页号）。
// chain[i] = 第 i+1 页的游标（chain[0] = null = 首页）；chain.length = 已加载页数 + 1
// （每取回一页 push 一次 next_cursor）；pages 为已加载页行缓存（回退/重访零请求，补链逐页写入）。
// 无淘汰策略（已知取舍）：缓存行数 = 已访问页数 × limit，长会话大页数内存线性增长，自托管可接受。
export interface CursorPage<T> {
  rows: T[]
  next_cursor: number | null
}

export function useCursorLogs<T>(
  deps: unknown[],
  fetchPage: (cursor: number | null) => Promise<CursorPage<T>>,
) {
  // 参数变化触发重置：deps（filters/limit/tab 派生值）序列化做键，内容不变不重置。
  const key = JSON.stringify(deps)
  const [page, setPage] = useState(1)
  const [isLoading, setIsLoading] = useState(true)
  const [isFetching, setIsFetching] = useState(false)
  const [isError, setIsError] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const chainRef = useRef<(number | null)[]>([null])
  const pagesRef = useRef(new Map<number, T[]>())
  // 代际守卫：参数变化/卸载时 ++，所有 fetchPage 结果写入路径（首页/goNext/补链）校验捕获值，
  // 变化即丢弃在途结果——react-query 的 queryKey 隔离被替换后无此机制，统一覆盖。
  const genRef = useRef(0)
  // fetchPage 由调用方注入（hook 不感知 API 层）；ref 兜底让重置 effect 恒取最新闭包。
  const fetchPageRef = useRef(fetchPage)
  fetchPageRef.current = fetchPage
  // isFetching 的再入锁：按钮禁用与状态更新间可能同 tick 连点，锁兜底防并发请求。
  const fetchingRef = useRef(false)

  // 参数变化 → 重置链并请求首页。过滤栏不因 isFetching 禁用——这里立即重置 + 在途请求自动失效。
  useEffect(() => {
    const gen = ++genRef.current
    fetchingRef.current = false
    chainRef.current = [null]
    pagesRef.current = new Map()
    setPage(1)
    setIsLoading(true)
    setIsFetching(false)
    setIsError(false)
    setError(null)
    fetchPageRef.current(null)
      .then(res => {
        if (genRef.current !== gen) return
        pagesRef.current.set(1, res.rows)
        chainRef.current.push(res.next_cursor)
        setIsLoading(false)
      })
      .catch(e => {
        if (genRef.current !== gen) return
        setIsLoading(false)
        setIsError(true)
        setError(e)
      })
    // 卸载/参数变化：代际 +1，在途结果丢弃。
    return () => { genRef.current++ }
  }, [key])

  // 下一页：已加载页直接切（零请求）；未加载页 = 已加载最远页 + 1，取回后切。
  const goNext = () => {
    if (fetchingRef.current) return
    const next = page + 1
    if (pagesRef.current.has(next)) { setPage(next); return }
    const cursor = chainRef.current[next - 1]
    if (cursor == null) return // 已到真实末页（next_cursor null）
    const gen = genRef.current
    fetchingRef.current = true
    setIsFetching(true)
    setIsError(false)
    setError(null)
    fetchPageRef.current(cursor)
      .then(res => {
        if (genRef.current !== gen) return
        pagesRef.current.set(next, res.rows)
        chainRef.current.push(res.next_cursor)
        setPage(next)
      })
      .catch(e => {
        if (genRef.current !== gen) return
        setIsError(true)
        setError(e)
      })
      .finally(() => {
        if (genRef.current === gen) {
          fetchingRef.current = false
          setIsFetching(false)
        }
      })
  }

  // 上一页：游标链顺序加载保证上一页必在缓存，零请求。
  const goPrev = () => {
    if (fetchingRef.current) return
    if (page <= 1) return
    setPage(page - 1)
  }

  // 回最新：首页必在缓存，零请求。
  const goLatest = () => {
    if (fetchingRef.current) return
    setPage(1)
  }

  // 跳转：已加载页直接切；未加载页顺序补链（每页 1 次 keyset 查询，单次成本与现状翻页一致）。
  // 补链期间当前页不切换（停留原页 + loading 态），完成后一次跳转；
  // 超出真实末页停在末页；中途失败停在已加载页 + error。
  const goToPage = (target: number) => {
    if (fetchingRef.current) return
    if (!Number.isInteger(target) || target < 1) return
    if (target === page) return
    if (target <= chainRef.current.length - 1) { setPage(target); return }
    const gen = genRef.current
    fetchingRef.current = true
    setIsFetching(true)
    setIsError(false)
    setError(null)
    ;(async () => {
      try {
        let next = chainRef.current.length // 下一页页码（chain.length = 已加载页数 + 1）
        while (next <= target) {
          const cursor = chainRef.current[next - 1] // 第 next 页游标
          if (cursor == null) break // 真实末页（next_cursor null），停在末页
          const res = await fetchPageRef.current(cursor)
          if (genRef.current !== gen) return
          pagesRef.current.set(next, res.rows)
          chainRef.current.push(res.next_cursor)
          next++
        }
        if (genRef.current !== gen) return
        // Math.max(1, …) 防御：chain.length==1（首页在途未 push）时 next-1=0，
        // 直接 setPage(0) 会让页码按钮组指向空页（实际不可达——组件仅 isLoading=false 后渲染）。
        setPage(Math.max(1, Math.min(next - 1, target)))
      } catch (e) {
        if (genRef.current !== gen) return
        setIsError(true)
        setError(e)
      } finally {
        if (genRef.current === gen) {
          fetchingRef.current = false
          setIsFetching(false)
        }
      }
    })()
  }

  return {
    page,
    rows: pagesRef.current.get(page) ?? [],
    // 已加载最远页（= 已访问页数）；chain.length - 1 = 去掉首页游标占位。
    loadedPages: chainRef.current.length - 1,
    // 当前页 next_cursor 非 null = 还有下一页（chain[page] = 第 page+1 页游标）。
    hasNext: chainRef.current[page] != null,
    isLoading,
    isFetching,
    isError,
    error,
    goNext,
    goPrev,
    goLatest,
    goToPage,
  }
}

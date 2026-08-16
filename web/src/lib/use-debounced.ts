// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useEffect, useState } from 'react'

// 输入防抖：值停顿 ms 毫秒后才更新返回（避免每键一次查询/请求）。
// 原为 admin logs.tsx 文件内私有（候选搜索），抽出供 admin/user 两页共用——
// 候选搜索与筛选输入同频（300ms）；字符串/数字原值比较，未变化不触发重渲染。
export function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = useState(value)
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms)
    return () => clearTimeout(t)
  }, [value, ms])
  return v
}

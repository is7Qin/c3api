// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 有界并发的 async 映射（本项目唯一实现点；不引 p-limit 之类的运行时依赖）。
//
// 不变量：
// - **同时在飞**的 `fn` 调用数 ≤ `min(limit, items.length)`：worker 池大小即上限，
//   不存在「先全部启动再排队」的形态。
// - 结果按**输入下标**落位 ⇒ 与完成顺序无关，`R[]` 与 `items` 一一对应。
// - `limit` 非正数 ⇒ 抛错（调用方 bug，不静默当 1 用，否则并发预算会静默失效）。
// - **不做错误包装**：单条 `fn` 抛错 ⇒ 整个 Promise 拒绝（其余 worker 会自己跑完，结果被丢弃）。
//   要「单条失败只影响该条」的调用方必须在 `fn` 内部 catch —— 错误处理只有一处，在调用方。
export async function mapWithConcurrency<T, R>(
  items: readonly T[],
  limit: number,
  fn: (item: T, index: number) => Promise<R>,
): Promise<R[]> {
  if (!(limit >= 1)) throw new RangeError(`mapWithConcurrency: limit must be >= 1 (got ${limit})`)
  const out = new Array<R>(items.length)
  let next = 0
  // `next++` 与 `await` 之间没有让出点 ⇒ 同一项不会被两个 worker 取走（单线程即可靠）。
  const worker = async () => {
    for (;;) {
      const i = next++
      if (i >= items.length) return
      out[i] = await fn(items[i], i)
    }
  }
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, () => worker()))
  return out
}

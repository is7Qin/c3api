import type { components } from '@/lib/api/schema'

export function chunk<T>(items: T[], size = 100): T[][] {
  const result: T[][] = []
  for (let i = 0; i < items.length; i += size) result.push(items.slice(i, i + size))
  return result
}

export async function importSequential<T extends components['schemas']['CodexOAuthImportItem'] | components['schemas']['CodexPATImportItem']>(items: T[], templateId: number, groupId: number | undefined, send: (body: { items: T[]; template_id: number; group_id?: number }) => Promise<components['schemas']['ImportResult']>, onProgress?: (done: number, total: number) => void) {
  const batches = chunk(items, 100)
  const aggregate: components['schemas']['ImportResult'] = { imported: 0, updated: 0, failed: [] }
  for (let i = 0; i < batches.length; i++) {
    const result = await send({ items: batches[i], template_id: templateId, ...(groupId == null ? {} : { group_id: groupId }) })
    aggregate.imported += result.imported
    aggregate.updated += result.updated
    aggregate.failed.push(...result.failed.map(f => ({ ...f, index: f.index + i * 100 })))
    onProgress?.(i + 1, batches.length)
  }
  return aggregate
}

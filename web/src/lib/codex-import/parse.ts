export function parseRawText(rawText: string): { rows: unknown[]; error?: string } {
  const text = rawText.replace(/^\uFEFF/, '').trim()
  if (!text) return { rows: [], error: 'empty' }
  try {
    const value: unknown = JSON.parse(text)
    if (Array.isArray(value)) return { rows: value }
    if (value && typeof value === 'object') return { rows: [value] }
    return { rows: [], error: 'invalid' }
  } catch {
    if (!text.includes('\n')) return { rows: [], error: 'invalid' }
    const rows: unknown[] = []
    for (const [i, line] of text.split(/\r?\n/).entries()) {
      const trimmed = line.trim()
      if (!trimmed) continue
      try {
        const value = JSON.parse(trimmed)
        if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('object')
        rows.push(value)
      } catch {
        return { rows: [], error: `line:${i + 1}` }
      }
    }
    return rows.length ? { rows } : { rows: [], error: 'empty' }
  }
}

export async function readImportFile(file: File): Promise<string> {
  if (file.size > 5 * 1024 * 1024) throw new Error('fileTooLarge')
  return file.text()
}

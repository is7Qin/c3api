import { ScrollArea } from '@/components/ui/scroll-area'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { useTranslation } from 'react-i18next'
import type { NormalizedRow } from '@/lib/codex-import/normalize'

const mask = (value: string) => value.length <= 6 ? '***' : `${value.slice(0, 3)}***`
export function PreviewTable({ rows, kind }: { rows: NormalizedRow[]; kind: 'codex-oauth' | 'codex-pat' }) {
  const { t } = useTranslation()
  return <ScrollArea className="max-h-[360px] rounded-lg border bg-card" showHorizontal data-od-id="import-preview-scroll">
    <Table containerClassName="overflow-x-visible rounded-none border-0 bg-transparent shadow-none" className="min-w-[640px]">
      <TableHeader className="sticky top-0 z-10">
        <TableRow className="bg-muted/50">
          <TableHead className="w-12">{t('accounts.import.preview.col.index')}</TableHead>
          <TableHead>{t('accounts.import.preview.col.email')}</TableHead>
          <TableHead>{t('accounts.import.preview.col.accountId')}</TableHead>
          <TableHead>{t('accounts.import.preview.col.cred')}</TableHead>
          <TableHead>{t('accounts.import.preview.col.expires')}</TableHead>
          <TableHead className="min-w-[160px]">{t('accounts.import.preview.col.error')}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>{rows.length === 0 ? <TableRow><TableCell colSpan={6} className="py-10 text-center text-sm text-muted-foreground">{t('accounts.import.noValidRows')}</TableCell></TableRow> : rows.map(row => {
    const item = row.item as unknown as Record<string, string> | undefined
    const cred = kind === 'codex-oauth' ? item?.codex_oauth_token : item?.codex_pat_key
    const hasError = !!row.error
    return <TableRow key={row.index} className={hasError ? 'bg-destructive/5' : undefined}>
      <TableCell className="font-mono text-xs text-muted-foreground">{row.index + 1}</TableCell>
      <TableCell className="max-w-[160px] truncate text-sm" title={item?.codex_email}>{item?.codex_email ?? '—'}</TableCell>
      <TableCell className="max-w-[120px] truncate font-mono text-xs" title={item?.codex_account_id}>{item?.codex_account_id ?? '—'}</TableCell>
      <TableCell className="font-mono text-xs">{cred ? mask(cred) : '—'}</TableCell>
      <TableCell className="max-w-[180px] truncate font-mono text-xs" title={item?.codex_oauth_expires_at}>{item?.codex_oauth_expires_at ?? '—'}</TableCell>
      <TableCell className={`max-w-[220px] whitespace-normal break-words text-xs ${hasError ? 'font-medium text-destructive' : 'text-muted-foreground'}`}>{row.error ?? '—'}</TableCell>
    </TableRow>
  })}</TableBody>
    </Table>
  </ScrollArea>
}

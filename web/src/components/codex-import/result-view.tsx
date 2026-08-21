import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { useTranslation } from 'react-i18next'
import type { components } from '@/lib/api/schema'

export function ResultView({ result }: { result: components['schemas']['ImportResult'] }) {
  const { t } = useTranslation()
  return <div className="space-y-4" data-od-id="import-result"><Alert><AlertTitle>{t('accounts.import.result.title')}</AlertTitle><AlertDescription>{t('accounts.import.result.summary', { imported: result.imported, updated: result.updated, failed: result.failed.length })}</AlertDescription></Alert>{result.failed.length > 0 && <div><h3 className="mb-2 text-sm font-medium">{t('accounts.import.result.failedTitle')}</h3><Table><TableHeader><TableRow><TableHead>{t('accounts.import.preview.col.index')}</TableHead><TableHead>{t('accounts.import.preview.col.error')}</TableHead></TableRow></TableHeader><TableBody>{result.failed.map((f, i) => <TableRow key={`${f.index}-${i}`}><TableCell>{f.index + 1}</TableCell><TableCell className="text-destructive">{f.error}</TableCell></TableRow>)}</TableBody></Table><p className="mt-2 text-xs text-muted-foreground">{t('accounts.import.result.retryHint')}</p></div>}</div>
}

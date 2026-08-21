import { Badge } from '@/components/ui/badge'
import { Card } from '@/components/ui/card'
import { cn } from '@/lib/utils'
import { useTranslation } from 'react-i18next'
import type { CredentialKind } from '@/lib/codex-import/normalize'
import type { SourceId } from '@/lib/codex-import/adapters'

const sources: { id: SourceId; labelKey: string; ready: boolean }[] = [
  { id: 'cpa', labelKey: 'cpa', ready: true }, { id: 'c3api', labelKey: 'c3api', ready: false },
  { id: 'sub2api', labelKey: 'sub2api', ready: false }, { id: 'cockpit', labelKey: 'cockpit', ready: false },
  { id: '9router', labelKey: '9router', ready: false }, { id: 'codex', labelKey: 'codex', ready: false },
  { id: 'axonhub', labelKey: 'axonhub', ready: false }, { id: 'codex-manager', labelKey: 'codex-manager', ready: false },
]

export function SourceSelect({ value, kind, onChange }: { value: SourceId; kind: CredentialKind; onChange: (id: SourceId) => void }) {
  const { t } = useTranslation()
  return <div className="grid grid-cols-2 gap-3 sm:grid-cols-4" data-od-id="import-source-grid">
    {sources.map(source => <button key={source.id} type="button" disabled={!source.ready} title={!source.ready ? t('accounts.import.source.comingSoon') : undefined} onClick={() => onChange(source.id)} className="text-left disabled:cursor-not-allowed">
      <Card className={cn('relative flex min-h-[92px] flex-col justify-between p-4 pt-6 transition-all', value === source.id ? 'border-primary bg-primary/5 ring-1 ring-primary/20' : 'hover:border-border hover:shadow-sm', !source.ready && 'opacity-50 bg-muted/20')}>
        {source.id === 'cpa' && <Badge variant="secondary" className="absolute top-2 right-2 z-10 rounded-full px-2 py-0 text-xs shadow-xs">{t('accounts.import.source.recommended')}</Badge>}
        {!source.ready && <Badge variant="outline" className="absolute top-2 right-2 z-10 rounded-full bg-card px-2 py-0 text-xs shadow-xs">{t('accounts.import.source.comingSoon')}</Badge>}
        <span className="pr-6 text-sm font-semibold">{t(`accounts.import.source.${source.labelKey}` as any)}</span>
        <p className="mt-2 pr-6 text-xs leading-relaxed text-muted-foreground">{source.ready ? (kind === 'codex-oauth' ? 'OAuth' : 'PAT') : t('accounts.import.source.comingSoon')}</p>
      </Card>
    </button>)}
  </div>
}

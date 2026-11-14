// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 时间范围选择器：单入口 Popover——react-day-picker range 日历选起止日期 +
// from/to 各一个原生 time input（官方 calendar-time 同款，指示器隐藏）。
// 值格式与 DateTimePicker 一致：'YYYY-MM-DDTHH:mm'（本地时区），'' = 未设置。
// 交互：日历选范围保留原时间（无则 00:00）；改时间只动对应端；点外部关闭。
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { CalendarIcon } from 'lucide-react'
import type { DateRange } from 'react-day-picker'
import { DATE_PRESET_KEYS, presetRange } from '@/lib/date-presets'
import { cn } from '@/lib/utils'
import { Button } from '@/components/ui/button'
import { Calendar } from '@/components/ui/calendar'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'

const pad2 = (n: number) => String(n).padStart(2, '0')

function localDateStr(d: Date): string {
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`
}
function timeOf(v: string): string {
  return /T(\d{2}:\d{2})/.exec(v)?.[1] ?? '00:00'
}
function withTime(v: string, time: string): string {
  return `${v.slice(0, 10)}T${time}`
}
// 'YYYY-MM-DDTHH:mm' → 'YYYY-MM-DD HH:mm'（trigger 展示，数字格式双语通用）
function fmt(v: string): string {
  return v.replace('T', ' ')
}

export interface DateRangeValue {
  from: string
  to: string
}

export function DateRangePicker({
  value,
  onChange,
  className,
  minDate,
}: {
  value: DateRangeValue
  onChange: (v: DateRangeValue) => void
  className?: string
  /** 可选起点下界（含）：来自 /stats/capabilities——早于它的窗口在任何存储上都会
   *  被服务端拒绝，故日历直接禁选（裁剪规则见 lib/stats-capabilities.ts）。
   *  undefined = 能力未知 ⇒ 不裁剪（不假装有上限）。 */
  minDate?: Date
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  // draft：日历交互中间态（第一次点击 from 已定、to 未定——受控 selected 需
  // 表达"未完成"，否则补 to=from 会令第二次点击被视为新范围起点，两日范围
  // 永远选不出——2026-08-14 修复）；range 完成提交后清空。
  const [draft, setDraft] = useState<DateRange | undefined>(undefined)
  // 预设按钮的 disabled 判定用挂载时刻固定一次（与 stats 页 minDate 的 mountedMs
  // 同源），渲染期不读时钟：否则每次 render 的 minDate 比较基准都在漂。
  const [mountedNow] = useState(() => new Date())

  const selected: DateRange | undefined =
    draft ??
    (value.from && value.to
      ? { from: new Date(`${value.from.slice(0, 10)}T00:00:00`), to: new Date(`${value.to.slice(0, 10)}T00:00:00`) }
      : undefined)

  // 日历选范围：日期替换 + 保留两端原时间（无则 00:00）；range 未完成（无 to）
  // 仅推进 draft，提交（有 to）才 onChange。
  // 端点锚定：点击范围外日期只动对应端点（from 左侧 → from，to 右侧 → to），
  // 另一端点不动；点击范围内部 → 对称收缩——把较近的端点拉过来
  // （15-21 点 17 → 17-21 收 from，点 19 → 15-19 收 to），单次点击直接生效。
  const onSelect = (r: DateRange | undefined, click?: Date) => {
    let next = r
    const cur = selected
    if (cur?.from && cur.to && click) {
      const t = click.getTime()
      if (t > cur.from.getTime() && t < cur.to.getTime()) {
        next =
          Math.abs(t - cur.from.getTime()) < Math.abs(t - cur.to.getTime())
            ? { from: click, to: cur.to }
            : { from: cur.from, to: click }
      }
    }
    setDraft(next)
    if (!next?.from || !next.to) return
    onChange({ from: withTime(localDateStr(next.from), timeOf(value.from)), to: withTime(localDateStr(next.to), timeOf(value.to)) })
    setDraft(undefined)
  }

  // 时间变更：只动对应端，日期沿用当前值（无日期则用 from 的日期或今天）。
  const setTime = (which: 'from' | 'to', time: string) => {
    const base = value[which] || value.from
    const date = base ? base.slice(0, 10) : localDateStr(new Date())
    onChange({ ...value, [which]: `${date}T${time || '00:00'}` })
  }

  const timeInputCls =
    'h-8 rounded-md border border-input bg-transparent px-2 text-sm text-foreground outline-none focus-visible:border-ring [&::-webkit-calendar-picker-indicator]:hidden [&::-webkit-calendar-picker-indicator]:appearance-none'

  return (
    <Popover
      open={open}
      onOpenChange={o => {
        setOpen(o)
        if (!o) setDraft(undefined)
      }}
    >
      <PopoverTrigger
        render={
          <Button variant="outline" size="lg" className={cn('w-full justify-start gap-2 font-normal', className)} />
        }
      >
        <CalendarIcon className="size-4 text-muted-foreground" />
        {value.from || value.to ? (
          <span className="truncate">
            {value.from ? fmt(value.from) : '…'} — {value.to ? fmt(value.to) : '…'}
          </span>
        ) : (
          <span className="text-muted-foreground">{t('dateRange.placeholder')}</span>
        )}
      </PopoverTrigger>
      <PopoverContent align="start" className="w-auto p-0">
        {/* 快捷预设：区间语义全在 lib/date-presets.ts，这里只渲染 + 提交。
            点击时现取此刻（不复用 mountedNow）——区间是"从今天起算"的，
            停用判定的单调性保证挂载时 enabled 蕴含点击时仍 enabled。

            disabled 用**瞬时**比较（from < minDate），与下方 Calendar 的
            `{ before: minDate }`（react-day-picker 按**日**粒度比较）有意不同：
            minDate = now − 能力上限跨度，是"窗口起点不得早于此刻这个瞬间"的闸门；
            日历放行 minDate 当天，是既有行为（那天 00:00 可能早于闸门 <24h，选择后
            服务端按 window_too_long / raw_horizon 400，由 StatsWindowNotice 呈现）。
            预设宁严不宽——不能让一个已知会 400 的窗口一键填入。 */}
        <div className="flex flex-wrap gap-1 border-b p-2">
          {DATE_PRESET_KEYS.map(key => (
            <Button
              key={key}
              type="button"
              variant="ghost"
              size="xs"
              disabled={minDate ? new Date(presetRange(key, mountedNow).from) < minDate : false}
              onClick={() => {
                const r = presetRange(key)
                onChange(r)
                setDraft(undefined)
                setOpen(false)
              }}
            >
              {t(`dateRange.preset.${key}`)}
            </Button>
          ))}
        </div>
        <Calendar
          mode="range"
          numberOfMonths={2}
          selected={selected}
          onSelect={onSelect}
          defaultMonth={selected?.from}
          disabled={minDate ? { before: minDate } : undefined}
        />
        <div className="flex items-center gap-3 border-t p-3">
          <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
            {t('dateRange.from')}
            <input type="time" step={300} value={timeOf(value.from)} onChange={e => setTime('from', e.target.value)} className={timeInputCls} />
          </label>
          <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
            {t('dateRange.to')}
            <input type="time" step={300} value={timeOf(value.to)} onChange={e => setTime('to', e.target.value)} className={timeInputCls} />
          </label>
        </div>
      </PopoverContent>
    </Popover>
  )
}

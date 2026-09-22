// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 日期+时间选择器：shadcn 官方按钮式形态（参考 ui/apps/v4/examples/base/date-picker-time.tsx）。
// Trigger 为 outline Button（宽约 212px，justify-between），空值时显示占位文案（muted），
// 右侧 ChevronDown；Popover 内为 Calendar（mode=single + captionLayout="dropdown" 年月下拉
// 导航）+ 小时/分钟两个下拉选择（5 分钟步进）。
// 值格式与 datetime-local 一致：'YYYY-MM-DDTHH:mm'（本地时区），'' = 未设置；
// 页面侧沿用 fmt.toRFC3339 转 RFC3339，不改过滤数据流。
// 交互：点按钮打开 → 选日期（保留原时间，无时间则补 00:00，不关闭——还需选时间）
// → 改小时/分钟 → 点外部关闭；选中后可点右侧 X 快速清除，Popover 底部也有清除按钮。
// 手输能力移除（官方无此功能）。
import { useState } from "react"
import { useTranslation } from "react-i18next"
import { ChevronDownIcon, X } from "lucide-react"

import { cn } from "@/lib/utils"
import { Button } from "@/components/ui/button"
import { Calendar } from "@/components/ui/calendar"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"

const pad2 = (n: number) => String(n).padStart(2, "0")

// 小时 00-23（24 项）；分钟 5 分钟步进 00/05/.../55（12 项）——管理台过滤粒度足够。
const HOURS = Array.from({ length: 24 }, (_, i) => pad2(i))
const MINUTES = Array.from({ length: 12 }, (_, i) => pad2(i * 5))
const HOUR_ITEMS = Object.fromEntries(HOURS.map((h) => [h, h]))
const MINUTE_ITEMS = Object.fromEntries(MINUTES.map((m) => [m, m]))

function parseValue(v: string): { date: Date | undefined; time: string } {
  const m = /^(\d{4}-\d{2}-\d{2})T(\d{2}:\d{2})$/.exec(v)
  if (!m) return { date: undefined, time: "" }
  const date = new Date(`${m[1]}T00:00:00`)
  if (Number.isNaN(date.getTime())) return { date: undefined, time: "" }
  return { date, time: m[2] }
}

function toValue(d: Date, time: string): string {
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${time || "00:00"}`
}

export interface DateTimePickerProps {
  /** 'YYYY-MM-DDTHH:mm'（本地时区），'' = 未设置 */
  value: string
  onChange: (v: string) => void
  id?: string
  className?: string
}

export function DateTimePicker({ value, onChange, id, className }: DateTimePickerProps) {
  const { t } = useTranslation()
  const { date, time } = parseValue(value)
  const hh = time ? time.slice(0, 2) : "00"
  const mm = time ? time.slice(3, 5) : "00"
  const [open, setOpen] = useState(false)
  // Trigger 选中文本：'YYYY-MM-DD HH:mm'（无时间值时只显日期）
  const display = value ? value.replace("T", " ") : ""

  const clear = () => {
    onChange("")
    setOpen(false)
  }

  return (
    <div className={cn("flex items-center gap-1", className)}>
      <Popover open={open} onOpenChange={setOpen}>
        <PopoverTrigger
          render={
            <Button
              variant="outline"
              id={id}
              data-empty={!value}
              aria-label={t("datePicker.selectDate")}
              className="w-[212px] justify-between text-left font-normal data-[empty=true]:text-muted-foreground"
            />
          }
        >
          {value ? display : <span>{t("datePicker.selectDate")}</span>}
          <ChevronDownIcon className="size-4" />
        </PopoverTrigger>
        <PopoverContent align="start" className="w-auto p-0">
          <Calendar
            mode="single"
            selected={date}
            defaultMonth={date}
            captionLayout="dropdown"
            onSelect={(d) => d && onChange(toValue(d, time))}
            className="p-2"
          />
          <div className="flex items-center gap-2 border-t p-3">
            <Select
              items={HOUR_ITEMS}
              value={hh}
              disabled={!date}
              onValueChange={(v) => {
                if (v != null && date) onChange(toValue(date, `${v}:${mm}`))
              }}
            >
              <SelectTrigger className="w-16" aria-label={t("datePicker.hour")}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent className="max-h-60">
                {HOURS.map((h) => (
                  <SelectItem key={h} value={h} label={h}>
                    {h}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <span className="select-none text-sm text-muted-foreground">:</span>
            <Select
              items={MINUTE_ITEMS}
              value={mm}
              disabled={!date}
              onValueChange={(v) => {
                if (v != null && date) onChange(toValue(date, `${hh}:${v}`))
              }}
            >
              <SelectTrigger className="w-16" aria-label={t("datePicker.minute")}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent className="max-h-60">
                {MINUTES.map((m) => (
                  <SelectItem key={m} value={m} label={m}>
                    {m}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <div className="flex-1" />
            <Button variant="ghost" size="sm" onClick={clear}>
              {t("datePicker.clear")}
            </Button>
          </div>
        </PopoverContent>
      </Popover>
      {/* 快速清除（值非空时显示）：独立按钮避免与日历按钮的事件耦合 */}
      {value && (
        <Button
          variant="ghost"
          size="icon-sm"
          className="shrink-0 text-muted-foreground"
          title={t("datePicker.clear")}
          onClick={() => onChange("")}
        >
          <X />
        </Button>
      )}
    </div>
  )
}

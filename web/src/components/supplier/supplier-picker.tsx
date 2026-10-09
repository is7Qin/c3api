// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 供应商选择器（spec 2026-10-09 §2.3）：按邮箱模糊搜索「供应商面」用户
// （supplier_surface=true → {supplier, platform_admin}，单一事实源由后端
// domain.SupplierSurfaceRoles 决定），选中即写入数值 user id，彻底替换
// 「手查 ID 再手输」。复用 ui/combobox.tsx，范式对齐 logs.tsx 的 FilterCombobox。
import { useEffect, useRef, useState } from 'react'
import { keepPreviousData, useQuery } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { Combobox, ComboboxContent, ComboboxEmpty, ComboboxInput, ComboboxItem, ComboboxList } from '@/components/ui/combobox'
import { useDebounced } from '@/lib/use-debounced'

type SupplierPickerProps = {
  id: string
  /** 当前数值 user id（字符串）；'' = 未选 */
  value: string
  /** 选中/清除回调；清除传 '' */
  onChange: (id: string) => void
  placeholder?: string
  disabled?: boolean
  /** true → Status!=='active' 的候选渲染为不可选 + 徽标（账号归属用） */
  disableInactive?: boolean
}

export function SupplierPicker({ id, value, onChange, placeholder, disabled = false, disableInactive = false }: SupplierPickerProps) {
  const { t } = useTranslation()
  const [term, setTerm] = useState('')
  const [open, setOpen] = useState(false)
  const debounced = useDebounced(term, 250)
  // id → label 缓存：候选刷新后已选项仍能显示「邮箱 · #ID」（而非退化 #id）。
  const labelCache = useRef(new Map<string, string>())

  const { data: opts = [], isFetching, isError, refetch } = useQuery({
    queryKey: ['supplier-picker', { term: debounced }],
    queryFn: () => api.listUsers({ email: debounced || undefined, supplier_surface: true, limit: 20 }),
    enabled: open && !disabled, // 关闭或禁用时不查
    placeholderData: keepPreviousData, // 旧 key 结果进旧 cache 槽，迟到响应不覆盖新键
    staleTime: 60_000,
    select: r => r.rows.map(u => ({ id: u.ID!, label: u.Email ? `${u.Email} · #${u.ID}` : `#${u.ID}`, active: u.Status === 'active' })),
  })

  if (opts.length) for (const o of opts) labelCache.current.set(String(o.id), o.label)

  // 仅清理已存储的展开态（disabled/错误生效时收起）；渲染时不变量见 popupOpen。
  useEffect(() => {
    if (disabled || isError) setOpen(false)
  }, [disabled, isError])

  // 派生实际展开值：错误/禁用态下拉不可开（比仅在 handler 里拒绝更强）。
  const popupOpen = open && !disabled && !isError

  return (
    <Combobox
      items={opts.map(o => String(o.id))}
      filter={() => true}
      // 受控 API 是 value/onValueChange（不是 selectedValue——base-ui 已 Omit）。
      value={value || null}
      onValueChange={next => {
        if (disabled) return // 过渡期守卫：防残余选值回调改写保留值
        onChange(next ?? '')
        setTerm('')
      }}
      // fallback 产出 #<id>（非裸 id）；保留 value 原值（disabled 时不因 effect 清空）。
      itemToStringLabel={v => labelCache.current.get(v) ?? `#${v}`}
      open={popupOpen}
      onOpenChange={next => setOpen(next && !disabled && !isError)}
    >
      <ComboboxInput
        id={id}
        placeholder={placeholder}
        showClear={!disabled}
        disabled={disabled}
        onChange={e => setTerm(e.target.value)}
      />
      {/* 错误条在 input 之下、Popup 之外（base-ui Popup 会为输入焦点吞点击）。 */}
      {isError && !disabled && (
        <div className="mt-1 flex items-center justify-between gap-2 text-xs text-destructive">
          <span>{t('supplierPicker.error')}</span>
          <button type="button" onClick={() => refetch()}>{t('supplierPicker.retry')}</button>
        </div>
      )}
      <ComboboxContent>
        {opts.length === 0 && !isError && (
          <ComboboxEmpty>{isFetching ? t('supplierPicker.searching') : t('supplierPicker.noMatch')}</ComboboxEmpty>
        )}
        {disableInactive && opts.length > 0 && opts.every(o => !o.active) && (
          <div className="px-2 py-1.5 text-xs text-muted-foreground">{t('supplierPicker.allInactive')}</div>
        )}
        <ComboboxList>
          {opts.map(o => (
            <ComboboxItem key={o.id} value={String(o.id)} disabled={disabled || (disableInactive && !o.active)}>
              <span className="min-w-0 truncate">{o.label}</span>
              {disableInactive && !o.active && (
                <span className="ml-auto text-xs text-muted-foreground">{t('supplierPicker.inactive')}</span>
              )}
            </ComboboxItem>
          ))}
        </ComboboxList>
      </ComboboxContent>
    </Combobox>
  )
}

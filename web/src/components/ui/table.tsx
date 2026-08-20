// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import * as React from "react"

import { cn } from "@/lib/utils"

// forwardRef：accounts 页视口懒加载需 table 元素引用（IO 观察数据行）。
const Table = React.forwardRef<HTMLTableElement, React.ComponentProps<"table"> & { containerClassName?: string }>(
  function Table({ containerClassName, className, ...props }, ref) {
    return (
      <div
        data-slot="table-container"
        className={cn("relative w-full overflow-x-auto rounded-[14px] border border-[rgba(19,45,83,0.26)] bg-[color:var(--glass-card-light)] shadow-[inset_0_1px_0_rgba(255,255,255,0.5),0_10px_36px_rgba(19,45,83,0.16)] backdrop-blur-[var(--glass-blur)] dark:border-[rgba(148,180,220,0.32)] dark:bg-[color:var(--glass-card-dark)] dark:shadow-[inset_0_1px_0_rgba(255,255,255,0.07),0_10px_36px_rgba(2,6,14,0.5)]", containerClassName)}
      >
        <table
          ref={ref}
          data-slot="table"
          className={cn("w-full caption-bottom text-sm", className)}
          {...props}
        />
      </div>
    )
  }
)

function TableHeader({ className, ...props }: React.ComponentProps<"thead">) {
  return (
    <thead
      data-slot="table-header"
      className={cn("!bg-white/20 text-foreground backdrop-blur-[var(--glass-blur)] dark:!bg-white/6 [&_tr]:border-b [&_tr]:border-[rgba(19,45,83,0.16)] dark:[&_tr]:border-[rgba(148,180,220,0.2)]", className)}
      {...props}
    />
  )
}

function TableBody({ className, ...props }: React.ComponentProps<"tbody">) {
  return (
    <tbody
      data-slot="table-body"
      className={cn("[&_tr:last-child]:border-0", className)}
      {...props}
    />
  )
}

function TableFooter({ className, ...props }: React.ComponentProps<"tfoot">) {
  return (
    <tfoot
      data-slot="table-footer"
      className={cn(
        "border-t border-[rgba(19,45,83,0.16)] bg-white/16 font-medium backdrop-blur-[var(--glass-blur)] dark:border-[rgba(148,180,220,0.2)] dark:bg-white/6 [&>tr]:last:border-b-0",
        className
      )}
      {...props}
    />
  )
}

function TableRow({ className, ...props }: React.ComponentProps<"tr">) {
  return (
    <tr
      data-slot="table-row"
      className={cn(
        "border-b border-[rgba(19,45,83,0.12)] transition-colors hover:bg-white/22 has-aria-expanded:bg-white/22 data-[state=selected]:bg-[color:color-mix(in_srgb,#0071e3_10%,transparent)] dark:border-[rgba(148,180,220,0.14)] dark:hover:bg-white/8 dark:has-aria-expanded:bg-white/8 dark:data-[state=selected]:bg-[color:color-mix(in_srgb,#2997ff_14%,transparent)]",
        className
      )}
      {...props}
    />
  )
}

function TableHead({ className, ...props }: React.ComponentProps<"th">) {
  return (
    <th
      data-slot="table-head"
      className={cn(
        "h-10 px-3 text-left align-middle font-medium whitespace-nowrap text-foreground first:pl-4 last:pr-4 [&:has([role=checkbox])]:pr-0",
        className
      )}
      {...props}
    />
  )
}

function TableCell({ className, ...props }: React.ComponentProps<"td">) {
  return (
    <td
      data-slot="table-cell"
      className={cn(
        "px-3 py-2 align-middle whitespace-nowrap first:pl-4 last:pr-4 [&:has([role=checkbox])]:pr-0",
        className
      )}
      {...props}
    />
  )
}

function TableCaption({
  className,
  ...props
}: React.ComponentProps<"caption">) {
  return (
    <caption
      data-slot="table-caption"
      className={cn("mt-4 text-sm text-muted-foreground", className)}
      {...props}
    />
  )
}

export {
  Table,
  TableHeader,
  TableBody,
  TableFooter,
  TableHead,
  TableRow,
  TableCell,
  TableCaption,
}

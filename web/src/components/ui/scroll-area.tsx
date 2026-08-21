// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 自定义滚动条（base-ui ScrollArea 封装，shadcn 官方 scroll-area 同构）：
// 原生滚动隐藏 + 自绘 thumb（bg-border + rounded-full），视觉统一（深色模式
// 不再出现浏览器默认浅色粗滚动条）。参数提取自 ui 参考仓库 base 变体
// （style-luma.css）：竖条 w-2.5（10px）、thumb rounded-full。
// 需要显式高度约束（父级 flex 或 max-h）——Viewport size-full 填充。
// Content 部件不可省：它自带内容 ResizeObserver，内容异步加载（viewport 自身
// 尺寸不变、仅 scrollHeight 增长）时负责触发重新测量——缺它会导致滚动条在
// 内容加载完成后永不出现（hiddenState 锁定初始值，Scrollbar 直接 return null）。
import { ScrollArea as ScrollAreaPrimitive } from '@base-ui/react/scroll-area'
import { cn } from '@/lib/utils'

function ScrollArea({
  className,
  children,
  showHorizontal = false,
  ...props
}: ScrollAreaPrimitive.Root.Props & {
  showHorizontal?: boolean
}) {
  return (
    <ScrollAreaPrimitive.Root
      data-slot="scroll-area"
      // min-h-0 必需：flex 容器（flex-1 等高约束）中默认 min-height:auto 会被
      // 内容撑开导致无法滚动（main/侧边栏场景踩坑）
      // flex-col + Viewport flex-1：Viewport size-full 的 height:100% 在只有
      // max-h 无显式 height 的父级下解析为 auto（=内容高度），会把页面撑破；
      // flex 布局让 Viewport 收缩到可用高度
      // showHorizontal 时 pb-2.5：横向滚动条 absolute 定位在 Root 底部，padding
      // 把 Viewport 顶上去，滚动条落在 padding 区不遮挡表格最后一行
      className={cn('relative flex min-h-0 flex-col', showHorizontal && 'pb-2.5', className)}
      {...props}
    >
      <ScrollAreaPrimitive.Viewport
        data-slot="scroll-area-viewport"
        className="min-h-0 flex-1 rounded-[inherit] outline-none focus-visible:ring-3 focus-visible:ring-ring/50"
      >
        <ScrollAreaPrimitive.Content
          data-slot="scroll-area-content"
          // 默认（无横向滚动）时 minWidth: 0：让 Table/表单等 w-full 子元素
          // 撑满容器而非按内容固有宽度撑开（base-ui 默认 fit-content 会让
          // 容器取内容宽度，破坏自适应）
          // showHorizontal 时保持 fit-content：容器覆盖整个内容宽度（表格
          // 1630px），否则容器只有视口宽，向右滚动后表格溢出容器右缘，
          // 底部出现色差带
          style={showHorizontal ? undefined : { minWidth: 0 }}
        >
          {children}
        </ScrollAreaPrimitive.Content>
      </ScrollAreaPrimitive.Viewport>
      <ScrollBar />
      {showHorizontal && <ScrollBar orientation="horizontal" />}
      <ScrollAreaPrimitive.Corner />
    </ScrollAreaPrimitive.Root>
  )
}

function ScrollBar({
  className,
  orientation = 'vertical',
  ...props
}: ScrollAreaPrimitive.Scrollbar.Props) {
  return (
    <ScrollAreaPrimitive.Scrollbar
      data-slot="scroll-area-scrollbar"
      data-orientation={orientation}
      orientation={orientation}
      className={cn(
        'flex touch-none p-px transition-colors select-none',
        orientation === 'vertical' && 'h-full w-2.5 border-l border-l-transparent',
        orientation === 'horizontal' && 'h-2.5 flex-col border-t border-t-transparent',
        className
      )}
      {...props}
    >
      <ScrollAreaPrimitive.Thumb
        data-slot="scroll-area-thumb"
        className="relative flex-1 rounded-full bg-border"
      />
    </ScrollAreaPrimitive.Scrollbar>
  )
}

export { ScrollArea, ScrollBar }

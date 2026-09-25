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
      // showHorizontal 时 pb-2.5 只在**确实发生横向溢出**时才加：横向滚动条
      // absolute 定位在 Root 底部，padding 把 Viewport 顶上去，滚动条落在 padding
      // 区不遮挡表格最后一行。但 base-ui 只在有溢出时才设置 data-has-overflow-x
      // 并渲染滚动条——无条件加 padding 会让内容不超宽时卡片底部空出 10px 裸玻璃
      // （表格与玻璃边框之间出现一条带，卡片不再贴合表格）。
      // 玻璃卡片外框必须是**真实 border**（页面 className 传 border-<颜色>），
      // 不能用 border-transparent + after:border 画框：border 透明时元素自身
      // 背景（bg-[color:var(--glass-card-light)]，background-clip 默认 border-box）
      // 会铺满整个 border box，在 ::after 框**外侧**露出 0.8px 亮环；而 ::after
      // 与元素同用 14px 半径却整体内缩 0.8px，亮环在四角张开到 ~2.8px。结果边框
      // 看起来浮在卡片内部、卡片四角溢出边框，liquid glass 的清晰边光被模糊亮弧
      // 取代（ListToolbar 用真实 border，所以同一页上搜索卡片一直是正常的）。
      className={cn('relative flex min-h-0 flex-col', showHorizontal && 'data-[has-overflow-x]:pb-2.5', className)}
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

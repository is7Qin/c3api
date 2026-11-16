// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 内嵌页 iframe 容器（spec 2026-11-16 内嵌页面 §7-3 / §9-U1–U3 / §11-T9）。
//
// 全仓**唯一**持有 `referrerPolicy` / `allow` 的组件。三条属性都不是可调参数：
//   - referrerPolicy 固定 no-referrer：目标页面内部的后续请求若带 Referer，会把含 token
//     的 iframe URL 传给**第三个域**（§8-S1②）。T9 的源码形状断言钉的就是这一行。
//   - 外链锚点的 rel 固定 noopener noreferrer：新窗口不能拿到 `window.opener`，也不能回传路径。
//   - allow 只开 fullscreen：能力最小化（§8-S4）。
// 另：**不加 sandbox**（§8-S6 的明确取舍——目标站点需要正常 cookie/存储，收紧会大面积
// 白屏；配置者 = 平台管理员，URL 视为可信）。
import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ExternalLink, Info, X } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { userApi } from '@/lib/api/client'
import { userAuth } from '@/lib/auth'
import { buildEmbeddedUrl, resolveTheme } from '@/lib/custom-pages'
import { useTheme } from '@/components/theme-provider'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import type { components } from '@/lib/api/schema'

// 条目形状取自生成物（`GET /api/user/custom-pages` 的 CustomPage）：只有
// id/label/url/visibility/icon，不含 enabled（服务端已过滤，§4.4-D10）。
type ConsolePage = components['schemas']['CustomPage']

export function CustomPageFrame({ page }: { page: ConsolePage }) {
  const { t, i18n } = useTranslation()
  const { theme } = useTheme()
  // 提示行关一次就够：同一页面实例内不再出现，切到别的条目由父组件的 key 重置状态。
  const [hintOpen, setHintOpen] = useState(true)
  // `onLoad` = 「浏览器触发了加载事件」，**不是**「内嵌成功」：X-Frame-Options 拒绝时
  // 同样触发、`onError` 也不可靠（跨域读不到响应头，§9-U3）。所以骨架只负责首屏占位，
  // 不做任何失败探测；失败提示交给下面那行恒定文案。
  const [loaded, setLoaded] = useState(false)

  // me.ID 作为 `user_id` 随 URL 下发；staleTime 与 AppShell 的同键查询同档（60s），
  // 命中缓存时不会为 iframe 再发一次请求。无 token 时不发（§7.2 同一要求）。
  const { data: me } = useQuery({
    queryKey: ['user', 'me'],
    queryFn: () => userApi.me(),
    staleTime: 60_000,
    enabled: Boolean(userAuth.getToken()),
  })

  // scheme 白名单在**写入**时校验（§5-V6），这里是渲染侧兜底（§8-S3 防 DB 被手改）。
  // 大小写不敏感与 Go 的 `strings.ToLower` 判定同口径（`HTTP://` 合法）。
  const lowered = page.URL.toLowerCase()
  const isHttp = lowered.startsWith('http://') || lowered.startsWith('https://')

  // iframe 只在 `me` 就绪后渲染（§19-6）：src 里带 me.ID，先渲染再变值会让浏览器
  // **二次加载**（首屏空白闪一下，目标站点还会看到两次请求）。骨架因此延后撤：
  // me 未就绪 = 骨架，me 就绪且 iframe onLoad = 内容。
  const meReady = me !== undefined

  // token 只在渲染期从 userAuth 取（§8-S2）：永不写入配置、永不写日志、永不进我们
  // 返回的 JSON。主题优先用 ThemeProvider 的值；`system` 时读 `<html>` 上的实际类。
  const src = isHttp && meReady
    ? buildEmbeddedUrl(page.URL, {
        userId: me.ID,
        token: userAuth.getToken(),
        theme: theme === 'system' ? resolveTheme() : theme,
        lang: i18n.resolvedLanguage ?? 'zh-CN',
        srcHost: location.origin,
        srcUrl: location.href,
      })
    : ''

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h1 className="min-w-0 truncate text-xl font-semibold tracking-tight">{page.Label}</h1>
        {/* 地址非法时不渲染外链：空 href 会把当前页当目标打开，比没有入口更糟。
            合法时 href 与 iframe 用**同一份** buildEmbeddedUrl 结果（§9-U2）。
            **注意：href 里带 JWT（§8-S1 的能力本身）——不要给它加任何复制入口**
            （"复制链接"按钮、可全选的只读输入框、右键菜单扩展都算）：一键进剪贴板
            = 主动把凭据扩散出去，§8-S7 明确否掉过这个建议。 */}
        {isHttp && meReady && (
          <a
            href={src}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex shrink-0 items-center gap-1.5 text-sm text-muted-foreground transition-colors hover:text-foreground"
          >
            <ExternalLink className="size-4" />
            {t('customPage.openInNewTab')}
          </a>
        )}
      </div>

      {/* 恒显示的可关闭提示（§9-U3②）：提示文案可验证，内嵌失败探测不可验证。 */}
      {hintOpen && (
        <div className="flex items-start gap-2 rounded-[10px] border border-[rgba(19,45,83,0.18)] bg-[color:var(--glass-card-light)] px-3 py-2 backdrop-blur-[var(--glass-blur)] dark:border-[rgba(148,180,220,0.25)] dark:bg-[color:var(--glass-card-dark)]">
          <Info className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
          <p className="flex-1 text-sm text-muted-foreground">{t('customPage.blockedHint')}</p>
          <Button
            type="button"
            variant="ghost"
            size="icon-xs"
            aria-label={t('customPage.dismissHint')}
            onClick={() => setHintOpen(false)}
            className="shrink-0 text-muted-foreground"
          >
            <X />
          </Button>
        </div>
      )}

      {isHttp ? (
        // 高度按视口算（§9-U1）：iframe 自己滚动，外层不再出现第二条滚动条。
        // 数值：顶栏 56 + pt-9 36 + 标题行 28 + 两处 gap 24 + 提示行 36 + pb-8 32 ≈ 212px
        // = 13.25rem，取 13.5rem 留 4px 余量。iframe / 骨架 / 非法态三处**必须同值**，
        // 否则切换状态时内容区高度会跳（写 11rem 会溢出 36px → 双滚动条）。
        // min-h 兜住小屏（420px 是可用下限）。
        !meReady ? (
          <Skeleton className="h-[calc(100dvh-13.5rem)] min-h-[420px] w-full rounded-[14px]" />
        ) : (
          <div className="relative overflow-hidden rounded-[14px] border border-[rgba(19,45,83,0.26)] bg-[color:var(--glass-card-light)] shadow-[inset_0_1px_0_rgba(255,255,255,0.5),0_10px_36px_rgba(19,45,83,0.16)] dark:border-[rgba(148,180,220,0.32)] dark:bg-[color:var(--glass-card-dark)] dark:shadow-[inset_0_1px_0_rgba(255,255,255,0.07),0_10px_36px_rgba(2,6,14,0.5)]">
            {!loaded && <Skeleton className="absolute inset-0 z-10 h-full w-full rounded-none" />}
            <iframe
              src={src}
              title={page.Label}
              referrerPolicy="no-referrer"
              allow="fullscreen"
              onLoad={() => setLoaded(true)}
              className="block h-[calc(100dvh-13.5rem)] min-h-[420px] w-full border-0 bg-white dark:bg-[#0b0b0c]"
            />
          </div>
        )
      ) : (
        // 非 http(s)：不渲染 iframe（浏览器会用当前页的 origin 去解析相对地址），
        // 直接给可行动的空态——去设置页修 URL。高度与 iframe/骨架同值（§9-U1）。
        <Card className="h-[calc(100dvh-13.5rem)] min-h-[420px] items-center justify-center text-center">
          <p className="px-6 text-sm text-muted-foreground">{t('customPage.invalidUrl')}</p>
        </Card>
      )}
    </div>
  )
}

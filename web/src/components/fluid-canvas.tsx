// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// WebGL 流体背景：复用原版 fluid-shader，
// 挂在 data-glass-ambient 下，深浅主题各一套色板，随 html.dark 自适应。
import { useEffect, useRef } from 'react'
import { attachFluidShader, type FluidParams } from '@/lib/fluid-shader'

const LIGHT_PARAMS: FluidParams = {
  mouseRadius: 0.22,
  mouseStrength: 1.1,
  decay: 0.96,
  distortBoost: 1.35,
  noiseBoost: 0,
  swirlBoost: 0.45,
  speed: 18,
  distortion: 22,
  swirl: 14,
  swirlIterations: 8,
  scale: 0.5,
  rotation: -5,
  proportion: 50,
  softness: 100,
  shapeScale: 10,
  offsetX: 0,
  offsetY: 65,
  color1: '#4A9E8E',
  color2: '#7EC8BB',
  color3: '#E0F2EF',
}

const DARK_PARAMS: FluidParams = {
  mouseRadius: 0.22,
  mouseStrength: 1.1,
  decay: 0.96,
  distortBoost: 1.35,
  noiseBoost: 0,
  swirlBoost: 0.45,
  speed: 18,
  distortion: 22,
  swirl: 14,
  swirlIterations: 8,
  scale: 0.5,
  rotation: -5,
  proportion: 50,
  softness: 100,
  shapeScale: 10,
  offsetX: 0,
  offsetY: 65,
  color1: '#1E3A5F',
  color2: '#2A4A7A',
  color3: '#0C121B',
}

function isDark(): boolean {
  return document.documentElement.classList.contains('dark')
}

export function FluidCanvas() {
  const ref = useRef<HTMLCanvasElement>(null)
  const handleRef = useRef<ReturnType<typeof attachFluidShader> | null>(null)

  useEffect(() => {
    const canvas = ref.current
    if (!canvas) return

    // 初始按当前主题挂载
    const initial = isDark() ? DARK_PARAMS : LIGHT_PARAMS
    const handle = attachFluidShader(canvas, initial)
    handleRef.current = handle

    // 主题切换时原地换色（不重建 canvas）
    const observer = new MutationObserver(() => {
      handle.setParams(isDark() ? DARK_PARAMS : LIGHT_PARAMS)
    })
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ['class'] })

    // 系统主题跟随时也需响应（theme=system 场景）
    const media = window.matchMedia('(prefers-color-scheme: dark)')
    const onMediaChange = () => handle.setParams(isDark() ? DARK_PARAMS : LIGHT_PARAMS)
    media.addEventListener('change', onMediaChange)

    return () => {
      observer.disconnect()
      media.removeEventListener('change', onMediaChange)
      handle.dispose()
    }
  }, [])

  return (
    <canvas
      ref={ref}
      data-glass-fluid-canvas
      aria-hidden="true"
      style={{ position: 'absolute', inset: 0, width: '100%', height: '100%' }}
    />
  )
}

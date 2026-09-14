import { useEffect, useRef, useState } from 'react'
import type { CSSProperties, KeyboardEvent, PointerEvent } from 'react'

type Panel = 'sidebar' | 'swarm'
const minimum = { sidebar: 176, swarm: 260 }, maximum = { sidebar: 360, swarm: 560 }
const folded = { sidebar: 52, swarm: 0 }, threshold = { sidebar: 112, swarm: 136 }
const defaults = (width: number) => ({ sidebar: width <= 1100 ? 200 : 224, swarm: width >= 1600 ? 390 : width <= 1100 ? 320 : 344 })

export function usePanelLayout() {
  const [viewport, setViewport] = useState(window.innerWidth)
  const [sizes, setSizes] = useState<Partial<Record<Panel, number>>>({})
  const [collapsed, setCollapsed] = useState({ sidebar: false, swarm: false })
  const [dragging, setDragging] = useState<Panel | null>(null)
  const active = useRef<{ key: Panel; x: number; width: number; pointer: number; handle: HTMLDivElement } | null>(null)
  const widths = { ...defaults(viewport), ...sizes }
  const occupied = (key: Panel) => collapsed[key] ? folded[key] : widths[key]
  const limit = (key: Panel) => Math.max(minimum[key], Math.min(maximum[key], viewport - 320 - occupied(key === 'sidebar' ? 'swarm' : 'sidebar')))
  const stop = () => {
    const drag = active.current
    active.current = null
    if (drag?.handle.hasPointerCapture(drag.pointer)) drag.handle.releasePointerCapture(drag.pointer)
    setDragging(null)
  }
  useEffect(() => {
    const resize = () => { stop(); setViewport(window.innerWidth) }
    window.addEventListener('resize', resize); window.addEventListener('blur', stop)
    return () => { window.removeEventListener('resize', resize); window.removeEventListener('blur', stop) }
  }, [])
  const resize = (key: Panel, value: number) => {
    const fold = value < threshold[key] + (collapsed[key] ? 16 : 0)
    setCollapsed(previous => ({ ...previous, [key]: fold }))
    if (!fold) setSizes(previous => ({ ...previous, [key]: Math.max(active.current ? folded[key] : minimum[key], Math.min(value, limit(key))) }))
  }
  const separator = (key: Panel) => ({
    role: 'separator' as const, tabIndex: viewport > 900 ? 0 : -1,
    'aria-orientation': 'vertical' as const, 'aria-valuemin': folded[key], 'aria-valuemax': Math.round(limit(key)),
    'aria-valuenow': Math.round(occupied(key)), 'aria-valuetext': collapsed[key] ? 'Collapsed' : `${Math.round(widths[key])} pixels`,
    className: `column-resize ${dragging === key ? 'dragging' : ''}`,
    onPointerDown(event: PointerEvent<HTMLDivElement>) {
      if (event.button !== 0 || active.current || viewport <= 900) return
      event.preventDefault(); event.currentTarget.focus({ preventScroll: true })
      active.current = { key, x: event.clientX, width: occupied(key), pointer: event.pointerId, handle: event.currentTarget }
      setDragging(key); event.currentTarget.setPointerCapture(event.pointerId)
    },
    onPointerMove(event: PointerEvent<HTMLDivElement>) {
      const drag = active.current
      if (!drag || drag.pointer !== event.pointerId || drag.key !== key) return
      event.preventDefault(); resize(key, drag.width + (event.clientX - drag.x) * (key === 'sidebar' ? 1 : -1))
    },
    onPointerUp: stop, onPointerCancel: stop, onLostPointerCapture: stop,
    onKeyDown(event: KeyboardEvent<HTMLDivElement>) {
      if (viewport <= 900 || event.altKey || event.ctrlKey || event.metaKey || !['ArrowLeft', 'ArrowRight', 'Home', 'End', 'Enter'].includes(event.key)) return
      event.preventDefault()
      const delta = (event.key === 'ArrowRight' ? 16 : -16) * (key === 'sidebar' ? 1 : -1)
      if (event.key === 'Enter') resize(key, collapsed[key] ? widths[key] : folded[key])
      else if (event.key === 'Home') resize(key, folded[key])
      else if (event.key === 'End') resize(key, limit(key))
      else resize(key, collapsed[key] ? delta > 0 ? widths[key] : folded[key] : delta < 0 && widths[key] <= minimum[key] ? folded[key] : widths[key] + delta)
    },
  })
  return { collapsed, viewport, separator,
    toggle: (key: Panel) => setCollapsed(previous => ({ ...previous, [key]: !previous[key] })),
    expand: (key: Panel) => setCollapsed(previous => ({ ...previous, [key]: false })),
    className: `app ${collapsed.sidebar ? 'collapsed' : ''} ${collapsed.swarm ? 'swarm-collapsed' : ''} ${dragging ? 'resizing' : ''}`,
    style: { '--sidebar-width': `${Math.min(widths.sidebar, limit('sidebar'))}px`, '--swarm-width': `${Math.min(widths.swarm, limit('swarm'))}px` } as CSSProperties,
  }
}

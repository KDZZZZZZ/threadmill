import { useEffect, useRef } from 'react'

export interface GraphViewport { x: number; y: number; scale: number }

export function useGraphViewport(projectID: string) {
  const viewport = useRef<HTMLDivElement>(null)
  const content = useRef<HTMLDivElement>(null)
  const positions = useRef(new Map<string, GraphViewport>())
  useEffect(() => {
    const node = viewport.current, layer = content.current
    if (!node || !layer) return
    const view = positions.current.get(projectID) ?? { x: 0, y: 0, scale: 1 }
    positions.current.set(projectID, view)
    const apply = () => { layer.style.transformOrigin = '0 0'; layer.style.transform = `translate(${view.x}px, ${view.y}px) scale(${view.scale})` }
    const controller = new AbortController(), options = { signal: controller.signal }
    let drag: { id: number; clientX: number; clientY: number; x: number; y: number; moved: boolean } | undefined
    let suppressClick = false
    const end = () => {
      if (!drag) return
      suppressClick = drag.moved
      const id = drag.id; drag = undefined
      if (node.hasPointerCapture(id)) node.releasePointerCapture(id)
      node.style.cursor = ''
    }
    const editable = (target: EventTarget | null) => target instanceof Element && Boolean(target.closest('input,textarea,select,[contenteditable=true],a'))
    const zoom = (factor: number, clientX: number, clientY: number) => {
      const rect = node.getBoundingClientRect(), x = clientX - rect.left - layer.offsetLeft, y = clientY - rect.top - layer.offsetTop
      const scale = Math.max(.35, Math.min(3, view.scale * factor)), ratio = scale / view.scale
      view.x = x - (x - view.x) * ratio; view.y = y - (y - view.y) * ratio; view.scale = scale; apply()
    }
    node.addEventListener('pointerdown', event => {
      if (event.button !== 0 || editable(event.target)) return
      drag = { id: event.pointerId, clientX: event.clientX, clientY: event.clientY, x: view.x, y: view.y, moved: false }
      if (!(event.target as Element).closest('button')) node.focus({ preventScroll: true })
    }, options)
    window.addEventListener('pointermove', event => {
      if (!drag || drag.id !== event.pointerId) return
      const dx = event.clientX - drag.clientX, dy = event.clientY - drag.clientY
      if (!drag.moved) {
        if (Math.hypot(dx, dy) < 4) return
        drag.moved = true; node.setPointerCapture(event.pointerId); node.style.cursor = 'grabbing'
        node.focus({ preventScroll: true })
      }
      event.preventDefault(); view.x = drag.x + dx; view.y = drag.y + dy; apply()
    }, { ...options, passive: false })
    for (const name of ['pointerup', 'pointercancel', 'blur']) window.addEventListener(name, end, options)
    node.addEventListener('lostpointercapture', end, options)
    node.addEventListener('click', event => {
      if (suppressClick && event.detail !== 0) { suppressClick = false; event.preventDefault(); event.stopImmediatePropagation() }
    }, { ...options, capture: true })
    node.addEventListener('wheel', event => {
      if (!event.deltaY || editable(event.target)) return
      event.preventDefault(); end()
      const pixels = event.deltaY * (event.deltaMode === 1 ? 16 : event.deltaMode === 2 ? node.clientHeight : 1)
      zoom(Math.exp(-pixels * .0015), event.clientX, event.clientY)
    }, { ...options, passive: false })
    node.addEventListener('dblclick', event => { if (!editable(event.target)) { end(); Object.assign(view, { x: 0, y: 0, scale: 1 }); apply() } }, options)
    node.addEventListener('keydown', event => {
      if (editable(event.target) || event.altKey || event.ctrlKey || event.metaKey || !['+', '=', '-', '0', 'ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown'].includes(event.key)) return
      event.preventDefault(); event.stopPropagation(); end()
      if (event.key === '0') Object.assign(view, { x: 0, y: 0, scale: 1 })
      else if (['+', '=', '-'].includes(event.key)) {
        const rect = node.getBoundingClientRect(); zoom(event.key === '-' ? 1 / 1.2 : 1.2, rect.left + rect.width / 2, rect.top + rect.height / 2)
      } else {
        view.x += event.key === 'ArrowLeft' ? -32 : event.key === 'ArrowRight' ? 32 : 0
        view.y += event.key === 'ArrowUp' ? -32 : event.key === 'ArrowDown' ? 32 : 0
      }
      apply()
    }, options)
    apply()
    return () => { end(); controller.abort() }
  }, [projectID])
  return { viewport, content }
}

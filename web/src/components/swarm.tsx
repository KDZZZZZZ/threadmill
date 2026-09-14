import { useEffect, useRef, useState } from 'react'
import type { CSSProperties } from 'react'
import type { Activity, ProjectView } from '@/lib/types'
import { agentIcon, elapsed, stateLabel } from '@/lib/format'
import { agentState, liveTasks, taskRoles, visibleActivity } from '@/lib/project-events'
import { useGraphViewport } from '@/hooks/use-graph-viewport'
import { Icon } from './icons'

export function Swarm({ view, mobileOpen, onClose }: { view?: ProjectView; mobileOpen: boolean; onClose: () => void }) {
  const { viewport: graphViewportRef, content: graphContentRef } = useGraphViewport(view?.project.id ?? '')
  const waterfall = useRef<HTMLDivElement>(null)
  const [width, setWidth] = useState(316)
  const [height, setHeight] = useState(0)
  const [focus, setFocus] = useState<{ id: string; pinned: boolean } | null>(null)
  const [highlight, setHighlight] = useState<string | null>(null)
  useEffect(() => {
    const node = waterfall.current
    if (!node) return
    const observer = new ResizeObserver(() => { setWidth(Math.max(1, node.clientWidth - 28)); setHeight(node.clientHeight) })
    observer.observe(node)
    return () => observer.disconnect()
  }, [])
  useEffect(() => {
    const handler = (event: KeyboardEvent) => { if (event.key === 'Escape') { setFocus(null); setHighlight(null) } }
    document.addEventListener('keydown', handler)
    return () => document.removeEventListener('keydown', handler)
  }, [])
  const tasks = view ? liveTasks(view) : []
  const items = view ? visibleActivity(view) : []
  const ids = [...new Set(['manager', ...tasks.flatMap(task => taskRoles.map(role => task[role]?.ID).filter((id): id is string => Boolean(id))),
    ...items.map(item => item.agent_id)])].filter(id => items.some(item => item.agent_id === id))
  const pitch = Math.max(24, Math.min(62, width / Math.max(ids.length, 1)))
  const cell = Math.max(18, Math.min(34, pitch - 14)), step = cell + 20
  const groups = ids.map(id => items.filter(item => item.agent_id === id).toReversed())
  const rows = Math.max(0, ...groups.map(group => group.length))
  const activeItem = items.find(item => item.id === focus?.id)
  const highlighted = activeItem?.agent_id ?? highlight
  const style = { '--section-width': `${ids.length * pitch}px`, '--pitch': `${pitch}px`, '--cell': `${cell}px`,
    '--glyph-size': `${Math.max(10, Math.min(15, cell * .45))}px`, '--section-height': `${Math.max(height, ids.length ? 64 + rows * step : 0)}px` } as CSSProperties
  const show = (item: Activity, pin: boolean) => setFocus(previous => pin
    ? previous?.id === item.id && previous.pinned ? null : { id: item.id, pinned: true }
    : previous?.pinned ? previous : { id: item.id, pinned: false })
  const hide = () => { setFocus(previous => previous?.pinned ? previous : null); setHighlight(null) }
  const gotoAgent = (id: string) => {
    setHighlight(id)
    const lane = [...(waterfall.current?.querySelectorAll<HTMLElement>('.flow-lane') ?? [])].find(node => node.dataset.agent === id)
    if (lane && waterfall.current) {
      const bounds = lane.getBoundingClientRect(), viewport = waterfall.current.getBoundingClientRect()
      waterfall.current.scrollTop += bounds.top - viewport.top
      waterfall.current.scrollLeft += bounds.left - viewport.left - viewport.width / 2 + bounds.width / 2
    }
  }
  const detail = activeItem && view ? [activeItem.agent_id, `${activeItem.name} · ${stateLabel(activeItem.state)}`,
    activeItem.call_id, activeItem.duration ? elapsed(activeItem.duration / 1e6) : '', activeItem.error,
    activeItem.retries ? `Retries: ${activeItem.retries} · ${activeItem.retry_reason ?? ''}` : '',
    activeItem.kind === 'model' && activeItem.started_at && view.reasoning?.[activeItem.agent_id]?.started_at === activeItem.started_at
      ? view.reasoning[activeItem.agent_id].text : '',
  ].filter(Boolean).join('\n') : ''
  return <aside id="inspector" className={`inspector ${mobileOpen ? 'visible' : ''}`} aria-label="Swarm">
    <div className="panel-heading"><h2>Swarm</h2><button id="inspect-close" className="icon-btn mobile-only inspect-close" aria-label="Close Swarm panel" onClick={onClose}><Icon name="close" /></button></div>
    <div id="graph" className="graph" ref={graphViewportRef} tabIndex={0} aria-label="Coordination graph. Drag to pan, scroll to zoom, arrow keys to move, plus or minus to zoom, 0 to reset." onPointerLeave={hide} onBlur={hide}>
      <div id="graph-content" className="graph-content" ref={graphContentRef}>
        {view ? <>
          <button className={`graph-manager ${agentState(view, 'manager')} ${highlighted === 'manager' ? 'highlighted' : ''}`} data-agent="manager" title={`Manager · ${stateLabel(agentState(view, 'manager'))}`} onClick={() => gotoAgent('manager')} onPointerEnter={() => setHighlight('manager')} onFocus={() => setHighlight('manager')}>
            <span className="graph-glyph"><Icon name="logo" /></span><span>Manager</span><span className={`dot ${agentState(view, 'manager') === 'running' ? 'running' : ''}`} />
          </button>
          {tasks.map(task => <section className="graph-task" key={task.ID}><p className="graph-task-title" title={task.Info ?? task.ID}>{task.ID}</p>
            <div className="graph-nodes">{taskRoles.map((role, index) => {
              const id = task[role]?.ID ?? '', state = agentState(view, id)
              return <RoleNode key={role} id={id} role={role} state={state} arrow={index > 0} highlighted={highlighted === id} onClick={() => gotoAgent(id)} onHighlight={() => setHighlight(id)} />
            })}</div>
          </section>)}
        </> : <p className="graph-empty">No tasks yet</p>}
      </div>
    </div>
    <div className="tool-area" aria-label="Activity from all agents"><div id="waterfall" className="waterfall" ref={waterfall} onPointerLeave={hide} onBlur={hide}>
      <div id="flow-track" className="flow-track" key={view?.project.id} style={{ width: Math.max(1, width) }}>
        <div className="flow-section flow-current" aria-label="Live task trajectories" style={style}>
          <div className="flow-events">{groups.flatMap((group, lane) => group.map((item, index) => <button key={item.id}
            className={`flow-event ${item.state} ${focus?.pinned && focus.id === item.id ? 'pinned' : ''}`}
            data-activity={item.id} data-agent={item.agent_id} aria-describedby="flow-detail"
            aria-label={`${item.agent_id} · ${item.name} · ${item.state === 'running' && item.retries ? `Retrying (${item.retries})` : stateLabel(item.state)}`}
            style={{ '--x': `${lane * pitch + (pitch - cell) / 2}px`, '--y': `${64 + index * step}px` } as CSSProperties}
            onPointerEnter={() => show(item, false)} onFocus={() => show(item, false)} onClick={() => show(item, true)}>
            <Icon name={item.icon} /><Icon name={item.icon} className="event-light" />
          </button>))}</div>
          {ids.map((id, index) => <div key={id} className={`flow-lane ${highlighted === id ? 'highlighted' : ''}`} data-agent={id} style={{ '--x': `${index * pitch}px` } as CSSProperties}>
            <button className="lane-head" data-agent={id} title={id} aria-label={id} onClick={() => gotoAgent(id)}><Icon name={agentIcon(id)} /></button>
          </div>)}
        </div>
      </div>
    </div><div id="flow-detail" className={`flow-detail ${focus?.pinned ? 'pinned' : ''}`} role="tooltip" hidden={!activeItem}>{detail}</div></div>
  </aside>
}

function RoleNode({ id, role, state, arrow, highlighted, onClick, onHighlight }: {
  id: string; role: string; state: string; arrow: boolean; highlighted: boolean; onClick: () => void; onHighlight: () => void
}) {
  return <>{arrow && <span className="graph-arrow" aria-hidden="true">→</span>}
    <button className={`graph-node ${state} ${highlighted ? 'highlighted' : ''}`} data-agent={id} title={`${id} · ${stateLabel(state)}`} aria-label={`${id} · ${stateLabel(state)}`}
      onClick={onClick} onPointerEnter={onHighlight} onFocus={onHighlight}>
      <span className="graph-glyph"><Icon name={agentIcon(id)} /></span><b>{role.toLowerCase()}</b>
    </button></>
}

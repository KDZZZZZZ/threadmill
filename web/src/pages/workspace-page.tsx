import { useEffect, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { Button } from '@heroui/react'
import { Moon, Sun } from 'lucide-react'
import { useSearchParams } from 'react-router-dom'
import { useOpenProject, useProject, useProjects, useSendMessage } from '@/lib/api/projects'
import { Conversation } from '@/components/conversation'
import { Icon, IconDefinitions } from '@/components/icons'
import { Swarm } from '@/components/swarm'
import { usePanelLayout } from '@/hooks/use-panel-layout'
import { useThemeStore } from '@/stores/theme-store'

interface ProjectUI {
  draft: string
  details: Record<string, boolean>
  retry?: { content: string; key: string }
}

export function WorkspacePage() {
  const [params, setParams] = useSearchParams()
  const demo = params.get('demo') === '1'
  const list = useProjects(demo)
  const selected = params.get('project')
  const activeID = list.data?.items.some(project => project.id === selected) ? selected! : list.data?.items[0]?.id ?? ''
  const current = useProject(activeID, demo), open = useOpenProject(demo), send = useSendMessage(demo)
  const view = current.data
  const [ui, setUI] = useState<Record<string, ProjectUI>>({})
  const projectUI = ui[activeID] ?? { draft: '', details: {} }
  const [search, setSearch] = useState(''), [searchOpen, setSearchOpen] = useState(false), [projectsOpen, setProjectsOpen] = useState(true)
  const [sidebarOpen, setSidebarOpen] = useState(false), [swarmOpen, setSwarmOpen] = useState(false)
  const [notice, setNotice] = useState('')
  const [dialogOpen, setDialogOpen] = useState(false), [root, setRoot] = useState('')
  const dialog = useRef<HTMLDialogElement>(null), rootInput = useRef<HTMLInputElement>(null), searchInput = useRef<HTMLInputElement>(null)
  const layout = usePanelLayout(), theme = useThemeStore()
  const sending = send.isPending && send.variables?.id === activeID
  const pending = useRef(new Set<string>())
  useEffect(() => { document.title = view ? `${view.project.name} · Threadmill` : 'Threadmill' }, [view])
  useEffect(() => {
    if (!notice) return
    const timer = setTimeout(() => setNotice(''), 4000)
    return () => clearTimeout(timer)
  }, [notice])
  useEffect(() => {
    if (searchOpen) searchInput.current?.focus({ preventScroll: true })
  }, [searchOpen])
  useEffect(() => {
    const node = dialog.current
    if (!node) return
    const reduce = matchMedia('(prefers-reduced-motion: reduce)').matches
    let animation: Animation | undefined
    if (dialogOpen) {
      if (!node.open) node.showModal()
      animation = node.animate([{ opacity: 0, transform: 'translateY(9px) scale(.97)' }, { opacity: 1, transform: 'translateY(0) scale(1)' }],
        { duration: reduce ? 0 : 220, easing: 'cubic-bezier(.16,1,.3,1)' })
      rootInput.current?.focus({ preventScroll: true }); rootInput.current?.select()
    } else if (node.open) {
      animation = node.animate([{ opacity: 1, transform: 'translateY(0) scale(1)' }, { opacity: 0, transform: 'translateY(5px) scale(.98)' }],
        { duration: reduce ? 0 : 160, easing: 'cubic-bezier(.4,0,1,1)', fill: 'forwards' })
      void animation.finished.then(() => node.close()).catch(() => {})
    }
    return () => animation?.cancel()
  }, [dialogOpen])
  const changeUI = (id: string, update: (previous: ProjectUI) => ProjectUI) => setUI(previous => ({ ...previous, [id]: update(previous[id] ?? { draft: '', details: {} }) }))
  const switchProject = (id: string) => { setParams(previous => { const next = new URLSearchParams(previous); next.set('project', id); return next }); setSidebarOpen(false) }
  const showOpen = () => { open.reset(); setDialogOpen(true) }
  const submitProject = async (event: FormEvent) => {
    event.preventDefault()
    try { const project = await open.mutateAsync(root.trim()); switchProject(project.id); setDialogOpen(false) } catch { /* The inline error preserves the form. */ }
  }
  const submitMessage = async (event: FormEvent) => {
    event.preventDefault()
    const content = projectUI.draft.trim(), id = activeID
    if (!content || !view || view.project.runtime_state !== 'open' || pending.current.has(id)) return
    pending.current.add(id)
    const key = projectUI.retry?.content === content ? projectUI.retry.key : crypto.randomUUID()
    changeUI(id, previous => ({ ...previous, retry: { content, key } }))
    try {
      await send.mutateAsync({ id, content, clientMessageID: key })
      changeUI(id, previous => ({ ...previous, retry: undefined, draft: previous.draft === projectUI.draft ? '' : previous.draft }))
    } catch (error) { setNotice((error as Error).message) } finally { pending.current.delete(id) }
  }
  const project = view?.project
  const connection = project?.runtime_state === 'closed' ? 'Closed' : project?.runtime_state === 'error' ? 'Runtime error'
    : current.isError ? 'Unable to sync status' : demo ? 'Demo' : view?.connected ? 'Connected' : activeID ? 'Connecting…' : ''
  const filtered = (list.data?.items ?? []).filter(project => `${project.name} ${project.root}`.toLowerCase().includes(search.trim().toLowerCase()))
  const status = sending ? 'Sending…' : project?.runtime_state === 'error' ? project.error ?? 'Runtime ended. Reopen the project to continue.'
    : project?.pending ? `${project.pending} pending ${project.pending === 1 ? 'request' : 'requests'}` : ''
  return <>
    <IconDefinitions />
    <div id="app" className={layout.className} style={layout.style}>
      <nav id="sidebar" className={`sidebar ${sidebarOpen ? 'visible' : ''}`} aria-label="Project navigation"><div className="sidebar-inner">
        <div className="brand"><span className="brand-mark"><Icon name="logo" /></span><span className="nav-copy">Threadmill</span>
          <button id="sidebar-toggle" className="icon-btn" aria-label={layout.collapsed.sidebar ? 'Expand project navigation' : 'Collapse project navigation'} title={layout.collapsed.sidebar ? 'Expand project navigation' : 'Collapse project navigation'}
            onClick={() => { if (layout.viewport <= 600) setSidebarOpen(false); else { layout.toggle('sidebar'); setSearchOpen(false); setSearch('') } }}><Icon name="sidebar" /></button></div>
        <button id="open-project" className="nav-row" onClick={showOpen} disabled={list.isError}><span className="nav-icon"><Icon name="folder" /></span><span className="nav-copy">Open project</span></button>
        <div className={`project-section ${searchOpen ? 'search-open' : ''}`} id="project-section" inert={layout.collapsed.sidebar && layout.viewport > 600}>
          <button id="projects-toggle" className="project-disclosure" aria-expanded={projectsOpen} aria-controls="project-fold" inert={searchOpen} onClick={() => setProjectsOpen(!projectsOpen)}><Icon name="chevron" /><span>Projects</span></button>
          <button className="icon-btn" id="search-toggle" aria-label="Search projects" aria-expanded={searchOpen} aria-controls="project-search-shell" inert={searchOpen} onClick={() => { setSearchOpen(true); setProjectsOpen(true) }}><Icon name="search" /></button>
          <div id="project-search-shell" className="project-search-shell" inert={!searchOpen}><Icon name="search" />
            <input id="project-search" className="project-search" placeholder="Search projects" aria-label="Search projects" ref={searchInput} value={search} onChange={event => setSearch(event.target.value)} />
            <button id="search-close" className="icon-btn" aria-label="Close project search" onClick={() => { setSearchOpen(false); setSearch('') }}><Icon name="close" /></button></div>
        </div>
        <div id="project-fold" className={`project-fold ${projectsOpen ? '' : 'folded'}`} inert={!projectsOpen || (layout.collapsed.sidebar && layout.viewport > 600)}><div className="project-fold-inner"><div id="project-list" className="project-list">
          {filtered.map(item => <button key={item.id} className={`nav-row ${item.id === activeID ? 'selected' : ''}`} data-project={item.id} title={item.root} aria-current={item.id === activeID ? 'page' : undefined} onClick={() => switchProject(item.id)}>
            <span className="nav-copy">{list.data!.items.filter(other => other.name === item.name).length > 1 ? item.root.split('/').slice(-2).join('/') : item.name}</span><span className={`dot ${item.busy ? 'running' : item.runtime_state === 'error' ? 'error' : ''}`} />
          </button>)}
        </div></div></div>
        <div className="sidebar-foot"><span className={`dot ${list.isSuccess ? 'running' : ''}`} id="local-dot" /><span id="local-status">{demo ? 'Demo · sample data' : list.isError ? 'Local service offline' : list.isPending ? 'Connecting to local service' : 'Running locally'}</span>
          <button className="icon-btn theme-toggle" aria-label={theme.mode === 'dark' ? 'Use light theme' : 'Use dark theme'} onClick={theme.toggle}>{theme.mode === 'dark' ? <Sun /> : <Moon />}</button>
        </div>
      </div></nav>
      <div id="sidebar-resize" {...layout.separator('sidebar')} aria-label="Resize Projects and Manager" aria-controls="sidebar" />
      <main className="workspace"><header className="panel-heading">
        <button id="nav-toggle" className="icon-btn mobile-only" aria-label="Open project navigation" onClick={() => setSidebarOpen(true)}><Icon name="sidebar" /></button>
        <h1>Manager</h1><span id="connection-status" className="header-status">{connection}</span>
        <button id="inspect-toggle" className="icon-btn mobile-only inspect-toggle" aria-label="View Swarm" onClick={() => { if (layout.viewport > 900) layout.expand('swarm'); else setSwarmOpen(!swarmOpen) }}><Icon name="agents" /></button>
      </header>
        <Conversation key={activeID} view={view} details={projectUI.details} onDetail={(detail, isOpen) => changeUI(activeID, previous => previous.details[detail] === isOpen ? previous : { ...previous, details: { ...previous.details, [detail]: isOpen } })} onOpen={showOpen} error={list.error?.message} />
        <div id="composer-wrap" className="composer-wrap" hidden={!view}><form id="composer" className="prompt-pill" onSubmit={submitMessage}>
          <label htmlFor="prompt" className="sr-only">Message Manager</label><textarea id="prompt" rows={1} placeholder="Message Manager…" autoComplete="off" value={projectUI.draft}
            disabled={project?.runtime_state !== 'open'} onChange={event => changeUI(activeID, previous => ({ ...previous, draft: event.target.value }))}
            onKeyDown={event => { if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); event.currentTarget.form?.requestSubmit() } }} />
          <p id="composer-status" className="composer-status" role="status" title={status}>{status}</p>
          <button id="send" className="send" type="submit" aria-label="Send to Manager" disabled={project?.runtime_state !== 'open' || !projectUI.draft.trim() || sending}><Icon name="arrow" /></button>
        </form></div>
      </main>
      <div id="swarm-resize" {...layout.separator('swarm')} aria-label="Resize Manager and Swarm" aria-controls="inspector" />
      <Swarm view={view} mobileOpen={swarmOpen} onClose={() => setSwarmOpen(false)} />
    </div>
    <dialog id="project-dialog" ref={dialog} onCancel={event => { event.preventDefault(); setDialogOpen(false) }} onClick={event => {
      if (event.target !== event.currentTarget) return
      const rect = event.currentTarget.getBoundingClientRect()
      if (event.clientX < rect.left || event.clientX > rect.right || event.clientY < rect.top || event.clientY > rect.bottom) setDialogOpen(false)
    }}><form id="project-form" onSubmit={submitProject}>
      <div className="dialog-heading"><h2>Open project</h2><button type="button" className="icon-btn" id="dialog-close" aria-label="Close" onClick={() => setDialogOpen(false)}><Icon name="close" /></button></div>
      <label htmlFor="new-root" className="field-label">Project path on server</label><input id="new-root" className="path-input" ref={rootInput} placeholder="/home/you/projects/my-project" required autoComplete="off" spellCheck={false} value={root} onChange={event => setRoot(event.target.value)} />
      <p id="path-note" className="dialog-note">The same path opens the same project and Manager.</p><p id="project-error" className="error-message" role="alert" hidden={!open.error}>{open.error?.message}</p>
      <div className="dialog-actions"><Button type="button" className="btn" variant="ghost" id="dialog-cancel" onPress={() => setDialogOpen(false)}>Cancel</Button>
        <Button type="submit" id="project-submit" className="btn primary" isDisabled={open.isPending}>{open.isPending ? 'Opening…' : 'Open project'}</Button></div>
    </form></dialog>
    <div id="toast" className="toast" role="status" hidden={!notice}>{notice}</div>
  </>
}

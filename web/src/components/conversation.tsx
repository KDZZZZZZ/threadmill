import { useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import type { ProjectView, Reasoning } from '@/lib/types'
import { elapsed, stateLabel } from '@/lib/format'
import { renderMarkdown } from '@/lib/markdown'
import { Icon } from './icons'

export function Markdown({ content }: { content: string }) {
  const html = useMemo(() => renderMarkdown({ content }), [content])
  return <div className="markdown" dangerouslySetInnerHTML={{ __html: html }} />
}

function Clock({ since }: { since: number }) {
  const [now, setNow] = useState(Date.now)
  const [started] = useState(Date.now)
  useEffect(() => { const timer = setInterval(() => setNow(Date.now()), 100); return () => clearInterval(timer) }, [])
  return <span className="elapsed" data-clock="manager">{elapsed(Math.max(0, now - (Number.isFinite(since) ? since : started)))}</span>
}

export function Thinking({ trace, open, onToggle }: { trace: Reasoning; open: boolean; onToggle: (open: boolean) => void }) {
  return <details className="thinking" open={open} onToggle={event => onToggle(event.currentTarget.open)}>
    <summary><Icon name="sparkle" className="sparkle" /><span className={trace.running ? 'shimmer' : ''}>
      {trace.running ? 'Thinking' : 'Thought for ' + elapsed((trace.duration ?? 0) / 1e6)}
    </span><Icon name="chevron" className="chevron" /></summary>
    <div className="trace-wrap"><pre className="trace">{trace.text || 'No reasoning available.'}</pre>
      {trace.truncated ? <p className="trace-note">Only the most recent reasoning is retained.</p>
        : trace.partial ? <p className="trace-note">Reconnected. Earlier reasoning may be incomplete.</p> : null}</div>
  </details>
}

interface ConversationProps {
  view?: ProjectView
  details: Record<string, boolean>
  onDetail: (id: string, open: boolean) => void
  onOpen: () => void
  error?: string
}

export function Conversation({ view, details, onDetail, onOpen, error }: ConversationProps) {
  const scroll = useRef<HTMLDivElement>(null)
  const nearBottom = useRef(true)
  const busy = view?.project.busy ?? false
  const since = Date.parse(view?.messages.items.findLast(message => message.speaker === 'user')?.created_at
    ?? view?.reasoning?.manager?.started_at ?? '')
  useEffect(() => { if (nearBottom.current && scroll.current) scroll.current.scrollTop = scroll.current.scrollHeight }, [view])
  let content: ReactNode
  if (!view) content = <div className="empty"><Icon name="folder" /><h2>{error ? 'Unable to connect' : 'Open a project'}</h2>
    <p>{error ?? 'Choose a path on the Threadmill host to start working with Manager.'}</p>
    {!error && <button id="empty-open" onClick={onOpen}>Open project →</button>}</div>
  else {
    const { messages: { items: messages }, project, reasoning } = view
    const footerMessage = busy && messages.at(-1)?.speaker === 'user' ? -1 : messages.findLastIndex(message => message.speaker === 'manager')
    const footer = (busy || reasoning?.manager) && <>
      {reasoning?.manager && <Thinking trace={reasoning.manager} open={details.thinking ?? false} onToggle={open => onDetail('thinking', open)} />}
      {view.activity.filter(item => item.agent_id === 'manager' && item.kind !== 'model' && item.state === 'running').slice(-3).map(item =>
        <details key={item.id} className="thinking action-event" open={details[item.id] ?? false} onToggle={event => onDetail(item.id, event.currentTarget.open)}>
          <summary><Icon name={item.icon} /><span className="shimmer">{item.kind === 'memory' ? 'Organizing memory'
            : /read/.test(item.name) ? 'Reading' : /write|edit/.test(item.name) ? 'Writing' : /exec|run/.test(item.name) ? 'Running' : item.name}
            {item.retries ? ` · Retrying (${item.retries})` : ''}</span><Icon name="chevron" className="chevron" /></summary>
          <div className="trace-wrap"><pre className="trace">{[item.name, item.call_id].filter(Boolean).join('\n')}</pre></div>
        </details>)}
      {busy && <div className="loading-state"><span className="loader-grid" aria-hidden="true">
        {[90, 180, 270, 0, 90, 180, 90, 180, 270].map((delay, index) => <i key={index} style={{ animationDelay: `${delay}ms` }} />)}
      </span><span className="loader-label shimmer">{project.runtime_state === 'error' ? 'Manager stopped · Work unfinished' : 'Working'}</span><Clock since={since} /></div>}
    </>
    content = <>{messages.map((message, index) => message.speaker === 'user'
      ? <article key={message.id} className="message user"><div className="user-text"><Markdown content={message.content} /></div>
        <div className="message-meta">{message.status === 'queued' ? 'Sent' : message.status === 'error' ? 'Failed to send' : ''}</div></article>
      : <article key={message.id} className="message manager" data-message={message.id}>
        <div className="message-meta"><Icon name="logo" /><strong>Manager</strong>{message.kind === 'task_report' && <span>Task report</span>}</div>
        {message.kind === 'task_report' ? <details className="thinking" open={details[message.id] ?? false} onToggle={event => onDetail(message.id, event.currentTarget.open)}>
          <summary><Icon name="check" /><span>{message.content.split('\n')[0] || 'View task report'}</span><Icon name="chevron" className="chevron" /></summary>
          <div className="message-body"><Markdown content={message.content} /></div>
        </details> : <div className="message-body"><Markdown content={message.content} /></div>}
        {['error', 'canceled'].includes(message.status ?? '') && <p className="message-error">{stateLabel(message.status)}</p>}
        {index === footerMessage && footer}
      </article>)}
      {footerMessage < 0 && footer && <article className="message manager"><div className="message-meta"><Icon name="logo" /><strong>Manager</strong></div>{footer}</article>}
      {!messages.length && !footer && <div className="empty"><Icon name="logo" /><h2>{project.name}</h2><p>Tell Manager what you want to work on.</p></div>}
    </>
  }
  return <div id="chat-scroll" className="scroll" aria-label="Manager conversation" ref={scroll} onScroll={event => {
    const node = event.currentTarget; nearBottom.current = node.scrollHeight - node.scrollTop - node.clientHeight < 100
  }}><div id="conversation" className="conversation">{content}</div></div>
}

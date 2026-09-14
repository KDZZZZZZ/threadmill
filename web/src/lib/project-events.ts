import type { Activity, Envelope, GraphTask, Message, Project, ProjectSnapshot, ProjectView, RuntimeEvent } from './types.ts'

export const taskRoles = ['Planner', 'Executor', 'Verifier'] as const
const terminalTask = (outcome?: string) => ['done', 'failed', 'canceled', 'closed'].includes(outcome ?? '')

export function emptyView(project: Project): ProjectView {
  return { project, messages: { items: [] }, agents: [], graph: { tasks: [], edges: [] },
    reasoning: {}, activity: [], taskOutcomes: {}, lastSeq: null, connected: false }
}

export function agentState(view: ProjectView, id?: string) {
  const agent = view.agents.find(agent => agent.id === id)
  return agent?.current_tool === 'coordination_requestHelp' ? 'waiting' : agent?.state ?? 'unknown'
}

function taskHasActivity(view: ProjectView, task: GraphTask) {
  return taskRoles.some(role => ['running', 'waiting'].includes(agentState(view, task[role]?.ID)) &&
    view.agents.some(agent => agent.id === task[role]?.ID && agent.activity))
}

export function liveTasks(view: ProjectView) {
  return view.project.runtime_state === 'closed' ? [] : view.graph.tasks.filter(task =>
    !terminalTask(view.taskOutcomes[task.ID] ?? task.Outcome) || taskHasActivity(view, task))
}

function taskForAgent(view: ProjectView, id: string) {
  return view.graph.tasks.find(task => taskRoles.some(role => task[role]?.ID === id))?.ID
    ?? view.agents.find(agent => agent.id === id)?.task_id
}

export function visibleActivity(view: ProjectView) {
  const tasks = new Set(liveTasks(view).map(task => task.ID))
  return view.activity.filter(item => item.agent_id === 'manager'
    ? view.project.runtime_state === 'open' && view.project.busy
    : tasks.has(taskForAgent(view, item.agent_id) ?? '') ||
      (view.agents.find(agent => agent.id === item.agent_id)?.role === 'organizer' &&
        (view.project.busy || agentState(view, item.agent_id) === 'running')))
}

function recordActivity(view: ProjectView, event: RuntimeEvent) {
  const active = (item: Activity) => item.agent_id === event.agent_id && item.kind === event.kind &&
    ['running', 'waiting'].includes(item.state)
  if (event.phase === 'retry') {
    const item = view.activity.findLast(active)
    if (item) Object.assign(item, { retries: event.retries, retry_reason: event.retry_reason })
    return
  }
  if (!['tool', 'model', 'memory'].includes(event.kind) || !['start', 'end'].includes(event.phase)) return
  let item = event.phase === 'end' ? view.activity.findLast(item => active(item) &&
    (!event.call_id || !item.call_id || item.call_id === event.call_id)) : undefined
  if (!item) {
    item = { id: crypto.randomUUID(), agent_id: event.agent_id, kind: event.kind,
      name: event.name ?? event.kind, started_at: event.time, call_id: event.call_id, state: 'unknown',
      icon: event.kind === 'model' ? 'sparkle' : event.kind === 'memory' ? 'memory'
        : /write|edit/.test(event.name ?? '') ? 'write' : /read/.test(event.name ?? '') ? 'read' : 'run' }
    view.activity.push(item)
  }
  Object.assign(item, {
    state: event.phase === 'start' ? event.name === 'coordination_requestHelp' ? 'waiting' : 'running'
      : event.error || event.is_error ? 'error' : 'done',
    duration: event.duration, error: event.error,
  })
}

function reconcileActivity(view: ProjectView, reset: boolean) {
  for (const item of view.activity) {
    if (!['running', 'waiting'].includes(item.state)) continue
    const agent = view.agents.find(agent => agent.id === item.agent_id)
    if (reset || !agent || !['running', 'waiting'].includes(agent.state) ||
      (item.kind === 'tool' && agent.current_tool && agent.current_tool !== item.name)) {
      item.state = agent?.state === 'failed' ? 'error' : agent?.state === 'canceled' ? 'canceled' : 'unknown'
    }
  }
  for (const agent of view.agents) {
    if (!['running', 'waiting'].includes(agent.state) || view.activity.some(item =>
      item.agent_id === agent.id && ['running', 'waiting'].includes(item.state))) continue
    const trace = view.reasoning?.[agent.id]
    const kind = agent.current_tool ? 'tool' : agent.activity === 'memory' ? 'memory' : trace?.running ? 'model' : null
    if (kind) recordActivity(view, { agent_id: agent.id, kind, name: agent.current_tool ?? kind,
      time: kind === 'model' ? trace?.started_at : agent.updated_at, phase: 'start' })
  }
}

export function upsertMessage(view: ProjectView, message: Message) {
  const index = view.messages.items.findIndex(item => item.id === message.id)
  if (index < 0) view.messages.items.push(message)
  else view.messages.items[index] = { ...view.messages.items[index], ...message }
}

export function mergeMetadata(previous: ProjectView, snapshot: ProjectSnapshot): ProjectView {
  const next = structuredClone(previous)
  Object.assign(next, snapshot)
  reconcileActivity(next, false)
  return next
}

// Pure event projection. Clone before updating: Query's cache must remain immutable.
export function applyEvent(previous: ProjectView, name: string, envelope: Envelope): ProjectView {
  if (envelope.project_id !== previous.project.id || !/^\d+$/.test(envelope.seq)) return previous
  if (name !== 'snapshot' && previous.lastSeq !== null && BigInt(envelope.seq) <= BigInt(previous.lastSeq)) return previous
  const next = structuredClone(previous)
  next.lastSeq = envelope.seq
  if (name === 'snapshot') {
    const snapshot = envelope.data as ProjectSnapshot
    Object.assign(next, snapshot)
    if (!snapshot.reasoning) for (const trace of Object.values(next.reasoning ?? {})) trace.partial = true
    next.connected = true
    reconcileActivity(next, true)
  } else if (name === 'stream_reset') {
    const reset = envelope.data as { message_id: string }
    next.messages.items = next.messages.items.filter(message => message.id !== reset.message_id)
    next.reasoning ??= {}
    next.reasoning.manager = { text: '', running: true, started_at: new Date().toISOString() }
  } else if (name === 'output') {
    upsertMessage(next, envelope.data as Message)
  } else if (name === 'runtime_event') {
    const event = envelope.data as RuntimeEvent
    applyRuntime(next, { ...event, agent_id: envelope.role_agent_id ?? event.agent_id }, envelope.message_id)
  }
  const ended = new Set(next.graph.tasks.filter(task =>
    terminalTask(next.taskOutcomes[task.ID] ?? task.Outcome) && !taskHasActivity(next, task)).map(task => task.ID))
  next.activity = next.activity.filter(item => item.agent_id === 'manager' ? next.project.busy :
    !ended.has(taskForAgent(next, item.agent_id) ?? ''))
  return next
}

function applyRuntime(view: ProjectView, event: RuntimeEvent, messageID?: string) {
  if (!event.agent_id) return
  if (event.kind === 'task') {
    if (event.phase === 'end' && terminalTask(event.name)) view.taskOutcomes[event.agent_id] = event.name!
    if (event.phase === 'start') {
      delete view.taskOutcomes[event.agent_id]
      const task = view.graph.tasks.find(task => task.ID === event.agent_id)
      if (task) task.Outcome = 'active'
      view.project.busy = true
    }
    return
  }
  recordActivity(view, event)
  let agent = view.agents.find(agent => agent.id === event.agent_id)
  if (!agent) { agent = { id: event.agent_id, state: 'unknown' }; view.agents.push(agent) }
  if (event.phase === 'start') {
    agent.state = event.name === 'coordination_requestHelp' ? 'waiting' : 'running'
    agent.activity = event.kind
    if (event.kind === 'tool') agent.current_tool = event.name
    view.project.busy = true
  }
  if (event.phase === 'end') {
    if (event.kind === 'tool') agent.current_tool = null
    agent.state = view.activity.some(item => item.agent_id === event.agent_id && item.state === 'running')
      ? 'running' : event.error || event.is_error ? 'error' : 'idle'
  }
  if (event.kind !== 'model') return
  const traces = view.reasoning ??= {}
  if (event.phase === 'start') traces[event.agent_id] = { text: '', started_at: event.time, duration: 0, running: true }
  if (event.reasoning_delta !== undefined) {
    const trace = traces[event.agent_id] ??= { text: '', started_at: event.time, running: true, partial: true }
    trace.text += event.reasoning_delta
    if (trace.text.length > 65536) {
      trace.text = trace.text.slice(-65536)
      if (/^[\uDC00-\uDFFF]/.test(trace.text)) trace.text = trace.text.slice(1)
      trace.truncated = true
    }
  }
  if (event.phase === 'end' && traces[event.agent_id]) Object.assign(traces[event.agent_id], { running: false, duration: event.duration ?? 0 })
  if (event.agent_id === 'manager' && event.delta && messageID) {
    const message = view.messages.items.find(message => message.id === messageID)
    if (message) message.content += event.delta
    else upsertMessage(view, { id: messageID, speaker: 'manager', content: event.delta, kind: 'message', status: 'streaming' })
  }
}

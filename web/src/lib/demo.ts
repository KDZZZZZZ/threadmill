import { emptyView } from './project-events'
import type { ProjectView } from './types'

// Explicit demo mode only. These observations never claim to be a live model run.
const demo = new Map<string, ProjectView>()
const roles = ['Planner', 'Executor', 'Verifier'] as const
export function openDemo(root: string): ProjectView {
  if (!root.startsWith('/')) throw new Error('Enter an absolute path.')
  const parts: string[] = []
  for (const part of root.split('/')) {
    if (!part || part === '.') continue
    if (part === '..') parts.pop()
    else parts.push(part)
  }
  root = '/' + parts.join('/')
  const existing = [...demo.values()].find(view => view.project.root === root)
  if (existing) return existing
  const view = emptyView({ id: 'demo-' + crypto.randomUUID(), name: parts.at(-1) ?? '/', root,
    runtime_state: 'open', busy: false, pending: 0, manager_id: 'manager' })
  view.agents = [{ id: 'manager', state: 'idle' }]
  view.connected = true
  demo.set(view.project.id, view)
  return view
}

function seed() {
  const view = openDemo('/home/oops/repo/threadmill')
  view.project.busy = true
  view.messages.items = [
    { id: 'demo-user', speaker: 'user', content: 'Map the OpenAPI endpoints, then build a local workspace for our agents.', status: 'complete' },
    { id: 'demo-manager', speaker: 'manager', content: 'I’ll map the project API and connect the local interface. You can follow each agent’s progress in Swarm.', status: 'complete' },
  ]
  view.graph.tasks = [
    { ID: 'task-1', Info: 'Map the project API', Outcome: 'done' },
    { ID: 'task-2', Info: 'Connect the local WebUI', Outcome: 'active' },
  ].map(task => ({ ...task, ...Object.fromEntries(roles.map(role => [role, { ID: `${task.ID}:${role.toLowerCase()}` }])) }))
  view.agents = [{ id: 'manager', state: 'running' }, ...view.graph.tasks.flatMap(task => roles.map(role => ({
    id: task[role]!.ID, task_id: task.ID, role: role.toLowerCase(),
    state: task.ID === 'task-1' || role === 'Planner' ? 'done' : role === 'Executor' ? 'running' : 'waiting',
  })))]
  view.reasoning = { manager: { text: '', running: true, started_at: new Date(Date.now() - 105600).toISOString() } }
  const samples = [['manager', 'model', 'Thinking', 'sparkle'], ['task-2:planner', 'model', 'Thinking', 'sparkle'],
    ['task-2:executor', 'tool', 'read_file', 'read'], ['task-2:executor', 'tool', 'write_file', 'write'],
    ['manager', 'memory', 'memory', 'memory'], ['task-2:executor', 'tool', 'exec', 'run']]
  view.activity = samples.map(([agent_id, kind, name, icon], index) => ({
    id: `demo-event-${index}`, agent_id, kind, name, icon,
    state: index === samples.length - 1 ? 'running' : 'done', duration: 1500000000 + index * 500000000,
  }))
  view.activity.push({ id: 'demo-thinking', agent_id: 'manager', kind: 'model', name: 'model', icon: 'sparkle',
    started_at: view.reasoning.manager.started_at, state: 'running' })
  openDemo('/home/oops/repo/docs-site')
  openDemo('/home/oops/repo/playground')
}
export function demoViews() { if (!demo.size) seed(); return demo }

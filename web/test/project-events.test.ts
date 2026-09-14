import assert from 'node:assert/strict'
import test from 'node:test'
import { applyEvent, emptyView, liveTasks, visibleActivity } from '../src/lib/project-events.ts'
import type { ProjectView } from '../src/lib/types.ts'

function fixture(): ProjectView {
  const view = emptyView({ id: 'p', name: 'Project', root: '/project', runtime_state: 'open', busy: true, pending: 0 })
  view.graph.tasks = [{ ID: 'task', Outcome: 'active', Executor: { ID: 'opaque-executor' } }]
  view.agents = [{ id: 'opaque-executor', task_id: 'task', role: 'executor', state: 'running' }]
  return view
}

test('duplicate, stale and foreign project events cannot append streamed text', () => {
  const before = fixture()
  const envelope = { project_id: 'p', seq: '9007199254740993', message_id: 'm',
    data: { agent_id: 'manager', kind: 'model', phase: 'delta', delta: 'Hello' } }
  const next = applyEvent(before, 'runtime_event', envelope)
  assert.equal(next.messages.items[0].content, 'Hello')
  assert.equal(before.messages.items.length, 0)
  assert.strictEqual(applyEvent(next, 'runtime_event', envelope), next)
  assert.strictEqual(applyEvent(next, 'runtime_event', { ...envelope, seq: '9007199254740992' }), next)
  assert.strictEqual(applyEvent(next, 'runtime_event', { ...envelope, project_id: 'other', seq: '9007199254740994' }), next)
})

test('snapshot restores Help waiting and cancellation clears only that requester', () => {
  const before = fixture()
  before.agents[0] = { ...before.agents[0], state: 'waiting', current_tool: 'coordination_requestHelp', activity: 'tool' }
  let next = applyEvent(before, 'snapshot', { project_id: 'p', seq: '1', data: before })
  assert.equal(visibleActivity(next).filter(item => item.state === 'waiting').length, 1)
  next = applyEvent(next, 'snapshot', { project_id: 'p', seq: '2', data: before })
  assert.equal(visibleActivity(next).filter(item => item.state === 'waiting').length, 1)
  const snapshot = structuredClone(before)
  snapshot.agents[0].state = 'canceled'
  snapshot.agents[0].current_tool = null
  next = applyEvent(next, 'snapshot', { project_id: 'p', seq: '3', data: snapshot })
  assert.equal(visibleActivity(next).filter(item => item.state === 'waiting').length, 0)
})

test('role_agent_id routes observed reasoning to the opaque graph identity', () => {
  const next = applyEvent(fixture(), 'runtime_event', { project_id: 'p', seq: '1', role_agent_id: 'opaque-executor',
    data: { agent_id: 'internal-model', kind: 'model', phase: 'delta', reasoning_delta: 'Checking files' } })
  assert.equal(next.reasoning?.['opaque-executor'].text, 'Checking files')
  assert.equal(next.reasoning?.['internal-model'], undefined)
})

test('retry reset removes only the retried answer and preserves user messages', () => {
  const before = fixture()
  before.messages.items = [{ id: 'u', speaker: 'user', content: 'Request' }, { id: 'm', speaker: 'manager', content: 'Partial' }]
  const next = applyEvent(before, 'stream_reset', { project_id: 'p', seq: '2', data: { message_id: 'm' } })
  assert.deepEqual(next.messages.items.map(message => message.id), ['u'])
  assert.equal(next.reasoning?.manager.running, true)
  assert.equal(before.messages.items.length, 2)
})

test('reasoning keeps its Unicode boundary and reports truncation', () => {
  const next = applyEvent(fixture(), 'runtime_event', { project_id: 'p', seq: '1',
    data: { agent_id: 'manager', kind: 'model', phase: 'delta', reasoning_delta: '😀' + 'x'.repeat(65535) } })
  assert.equal(next.reasoning?.manager.text, 'x'.repeat(65535))
  assert.equal(next.reasoning?.manager.truncated, true)
})

test('terminal tasks leave the live graph unless their role still has activity', () => {
  const before = fixture()
  const next = applyEvent(before, 'runtime_event', { project_id: 'p', seq: '1',
    data: { agent_id: 'task', kind: 'task', phase: 'end', name: 'done' } })
  assert.equal(liveTasks(next).length, 0)
  assert.equal(liveTasks(before).length, 1)
})

test('real gateway snapshots carry a bare agent array', () => {
  const before = fixture()
  const next = applyEvent(before, 'snapshot', { project_id: 'p', seq: '1', data: {
    project: before.project, messages: { items: [] }, agents: [{ id: 'manager', state: 'running', activity: 'model' }],
    graph: { tasks: [], edges: [] }, reasoning: { manager: { text: 'Thinking', running: true } },
  } })
  assert.equal(visibleActivity(next).filter(item => item.agent_id === 'manager').length, 1)
})

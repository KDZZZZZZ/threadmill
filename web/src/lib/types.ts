// Wire types follow docs/openapi.yaml. Graph IDs are opaque; never reconstruct them.
export interface Project {
  id: string
  name: string
  root: string
  model?: string
  manager_id?: string
  runtime_state: 'open' | 'closed' | 'error'
  busy: boolean
  pending: number
  task_running?: boolean
  error?: string
}

export interface Message {
  id: string
  speaker: 'user' | 'manager'
  content: string
  kind?: 'message' | 'task_report'
  status?: string
  created_at?: string
}

export interface Agent {
  id: string
  task_id?: string
  role?: string
  state: string
  activity?: string
  current_tool?: string | null
  updated_at?: string
}

export interface GraphTask {
  ID: string
  Info?: string
  Outcome?: string
  Planner?: { ID: string }
  Executor?: { ID: string }
  Verifier?: { ID: string }
  RealDirectory?: boolean
  Persistent?: boolean
  Activation?: number
}

export interface Graph {
  tasks: GraphTask[]
  edges: { from: string; to: string }[]
  project_task_id?: string
  revision?: number
  executing?: boolean
}

export interface Reasoning {
  text: string
  started_at?: string
  duration?: number // nanoseconds, matching the Go gateway
  running: boolean
  truncated?: boolean
  partial?: boolean
}

export interface ProjectSnapshot {
  project: Project
  messages: { items: Message[] }
  agents: Agent[]
  graph: Graph
  reasoning?: Record<string, Reasoning>
}

export interface RuntimeEvent {
  agent_id: string
  kind: 'task' | 'tool' | 'model' | 'memory'
  phase: 'start' | 'end' | 'delta' | 'retry'
  name?: string
  time?: string
  call_id?: string
  duration?: number
  error?: string
  is_error?: boolean
  retries?: number
  retry_reason?: string
  reasoning_delta?: string
  delta?: string
}

export interface Envelope<T = unknown> {
  project_id: string
  seq: string
  data: T
  role_agent_id?: string
  message_id?: string
}

export interface Activity {
  id: string
  agent_id: string
  kind: string
  name: string
  icon: string
  state: string
  started_at?: string
  call_id?: string
  duration?: number
  error?: string
  retries?: number
  retry_reason?: string
}

// Cached server observations, including projections of the event stream.
export interface ProjectView extends ProjectSnapshot {
  activity: Activity[]
  taskOutcomes: Record<string, string>
  lastSeq: string | null
  connected: boolean
}

export interface MessageReceipt {
  accepted: boolean
  message_id: string
  queue_depth: number
}

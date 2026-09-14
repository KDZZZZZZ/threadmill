export const stateLabel = (state?: string) => ({
  running: 'Running', active: 'Running', idle: 'Idle', done: 'Completed',
  completed: 'Completed', waiting: 'Waiting', queued: 'Queued', canceled: 'Stopped',
  failed: 'Failed', error: 'Failed', unknown: 'Unknown', held: 'Waiting', pending: 'Waiting',
}[state ?? ''] ?? state ?? 'Unknown')

export const elapsed = (ms: number) => ms < 60_000
  ? `${(ms / 1000).toFixed(1)}s`
  : `${Math.floor(ms / 60_000)}m ${((ms % 60_000) / 1000).toFixed(1)}s`

export const agentIcon = (id: string) => id === 'manager' ? 'logo'
  : id.includes('organizer') ? 'memory'
    : id.endsWith(':planner') ? 'sparkle' : id.endsWith(':verifier') ? 'check' : 'run'

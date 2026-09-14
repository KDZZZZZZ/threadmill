import type { Envelope } from './types'

export class ApiError extends Error {
  constructor(message: string, readonly status: number) { super(message) }
}

// Threadmill uses plain JSON responses, not the example stack's code/data envelope.
export async function apiFetch<T>(path: string, json?: unknown, signal?: AbortSignal): Promise<T> {
  const response = await fetch(path, {
    method: json === undefined ? 'GET' : 'POST', signal,
    headers: json === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: json === undefined ? undefined : JSON.stringify(json),
  })
  let data
  try { data = await response.json() } catch { throw new ApiError('Invalid response from the local service.', response.status) }
  if (!response.ok) throw new ApiError(data.error?.message ?? data.message ?? String(data.error ?? `HTTP ${response.status}`), response.status)
  return data as T
}

// Keep all network access at the gateway, including SSE. Cleanup mirrors subscription.
export function subscribeEvents(path: string, onEvent: (name: string, event: Envelope) => void, onConnection: (connected: boolean) => void) {
  const stream = new EventSource(path)
  let closed = false
  stream.onopen = () => { if (!closed) onConnection(true) }
  stream.onerror = () => { if (!closed) onConnection(false) }
  for (const name of ['snapshot', 'runtime_event', 'output', 'stream_reset']) {
    stream.addEventListener(name, event => {
      if (closed) return
      let envelope: Envelope
      try { envelope = JSON.parse((event as MessageEvent<string>).data) } catch { return }
      onEvent(name, envelope)
    })
  }
  return () => { closed = true; stream.close() }
}

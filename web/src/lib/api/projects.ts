import { useEffect } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { apiFetch, subscribeEvents } from '@/lib/http'
import { demoViews, openDemo } from '@/lib/demo'
import { applyEvent, emptyView, mergeMetadata, upsertMessage } from '@/lib/project-events'
import type { Agent, Graph, Message, MessageReceipt, Project, ProjectView } from '@/lib/types'

const keys = {
  all: (demo: boolean) => ['projects', demo] as const,
  list: (demo: boolean) => [...keys.all(demo), 'list'] as const,
  detail: (demo: boolean, id: string) => [...keys.all(demo), 'detail', id] as const,
}
const endpoint = (id: string, tail = '') => `/api/v1/projects/${encodeURIComponent(id)}${tail}`

export function useProjects(demo: boolean) {
  return useQuery({ queryKey: keys.list(demo), queryFn: async ({ signal }) => demo
    ? { items: [...demoViews().values()].map(view => view.project) }
    : apiFetch<{ items: Project[] }>('/api/v1/projects', undefined, signal), refetchInterval: demo ? false : 1500 })
}

export function useProject(id: string, demo: boolean) {
  const client = useQueryClient()
  const key = keys.detail(demo, id)
  const query = useQuery({
    queryKey: key, enabled: Boolean(id), staleTime: 0,
    initialData: () => {
      if (demo) return structuredClone(demoViews().get(id))
      const project = client.getQueryData<{ items: Project[] }>(keys.list(demo))?.items.find(project => project.id === id)
      return project ? emptyView(project) : undefined
    },
    queryFn: async ({ signal }) => {
      if (demo) return structuredClone(demoViews().get(id)!)
      const before = client.getQueryData<ProjectView>(key)
      const [projectResult, agentsResult, graphResult, messagesResult] = await Promise.allSettled([
        apiFetch<Project>(endpoint(id), undefined, signal),
        apiFetch<{ items: Agent[] }>(endpoint(id, '/agents'), undefined, signal),
        apiFetch<Graph>(endpoint(id, '/graph'), undefined, signal),
        before?.lastSeq !== null && before?.messages
          ? Promise.resolve(before.messages)
          : apiFetch<{ items: Message[] }>(endpoint(id, '/messages'), undefined, signal),
      ])
      if (projectResult.status === 'rejected') throw projectResult.reason
      const current = client.getQueryData<ProjectView>(key)
      const project = projectResult.value
      const agents = agentsResult.status === 'fulfilled' ? agentsResult.value.items : current?.agents ?? []
      const graph = graphResult.status === 'fulfilled' ? graphResult.value : current?.graph ?? { tasks: [], edges: [] }
      const messages = messagesResult.status === 'fulfilled' ? messagesResult.value : current?.messages ?? { items: [] }
      // Preserve events/messages received during refresh, but keep graph discovery moving
      // during a continuous model stream. Graph revisions cannot move backwards.
      if (current && current !== before) return { ...current,
        ...(project.runtime_state === 'closed' ? { project, connected: false } : {}),
        graph: (graph.revision ?? 0) >= (current.graph.revision ?? 0) ? graph : current.graph }
      return mergeMetadata(current ?? emptyView(project), { project, agents, graph, messages })
    },
    refetchInterval: demo ? false : 1500,
  })
  const closed = query.data?.project.runtime_state === 'closed'
  useEffect(() => {
    if (!id || demo || closed) return
    const key = keys.detail(false, id)
    // https://react.dev/reference/react/useEffect#connecting-to-an-external-system
    return subscribeEvents(endpoint(id, '/events'), (name, envelope) => {
      client.setQueryData<ProjectView>(key, previous => previous ? applyEvent(previous, name, envelope) : previous)
    }, connected => client.setQueryData<ProjectView>(key, previous => previous ? { ...previous, connected } : previous))
  }, [id, demo, closed, client])
  return query
}

export function useOpenProject(demo: boolean) {
  const client = useQueryClient()
  return useMutation({
    mutationFn: async (root: string) => demo ? openDemo(root).project : apiFetch<Project>('/api/v1/projects', { root }),
    onSuccess: project => {
      client.setQueryData<ProjectView>(keys.detail(demo, project.id), previous => demo
        ? structuredClone(demoViews().get(project.id)) : previous ? { ...previous, project } : emptyView(project))
      client.setQueryData<{ items: Project[] }>(keys.list(demo), old => ({
        items: [...(old?.items ?? []).filter(item => item.id !== project.id), project],
      }))
      void client.invalidateQueries({ queryKey: keys.all(demo) })
    },
  })
}

export function useSendMessage(demo: boolean) {
  const client = useQueryClient()
  return useMutation({
    mutationFn: async (input: { id: string; content: string; clientMessageID: string }) => demo
      ? { accepted: true, message_id: input.clientMessageID, queue_depth: 1 }
      : apiFetch<MessageReceipt>(endpoint(input.id, '/messages'), {
        content: input.content, client_message_id: input.clientMessageID,
      }),
    onSuccess: (receipt, input) => {
      client.setQueryData<ProjectView>(keys.detail(demo, input.id), previous => {
        if (!previous) return previous
        const next = structuredClone(previous)
        upsertMessage(next, { id: receipt.message_id, speaker: 'user', kind: 'message', content: input.content, status: 'queued' })
        next.project.pending = receipt.queue_depth
        if (demo) demoViews().set(input.id, next)
        return next
      })
      if (demo) setTimeout(() => {
        client.setQueryData<ProjectView>(keys.detail(true, input.id), previous => {
          if (!previous) return previous
          const next = structuredClone(previous)
          upsertMessage(next, { id: crypto.randomUUID(), speaker: 'manager', kind: 'message', status: 'complete',
            content: 'I’ll continue working on this project and share the results when ready.' })
          next.project.pending = Math.max(0, next.project.pending - 1)
          demoViews().set(input.id, next)
          return next
        })
      }, 1500)
      void client.invalidateQueries({ queryKey: keys.all(demo) })
    },
  })
}

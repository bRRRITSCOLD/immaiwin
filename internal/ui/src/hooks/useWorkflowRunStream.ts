import { useCallback, useEffect, useRef, useState } from 'react'

export type RunStatus = 'idle' | 'connecting' | 'running' | 'done' | 'error'

// Wire shape — mirrors workflow.RunEvent in Go
// (`internal/workflow/events.go`). Keep in sync.
export interface RunEvent {
  type:
    | 'step_start'
    | 'step_done'
    | 'agent_iter'
    | 'agent_llm'
    | 'agent_tool_call'
    | 'agent_tool_result'
    | 'agent_final'
    | 'run_done'
    | 'error'
  at: string
  node_id?: string
  node_type?: string
  iter?: number
  text?: string
  tool_name?: string
  tool_id?: string
  tool_args?: unknown
  result?: string
  is_error?: boolean
  output?: unknown
  error?: string
  usage?: {
    input_tokens: number
    output_tokens: number
    total_tokens: number
    cost_usd: number
  }
}

export interface NodeRunState {
  nodeId: string
  nodeType?: string
  status: 'pending' | 'running' | 'done' | 'error'
  output?: unknown
  error?: string
  // Agent-specific accumulators
  iters: AgentIter[]
  finalText?: string
}

export interface AgentIter {
  iter: number
  llm?: { text?: string; usage?: RunEvent['usage'] }
  toolCalls: AgentToolCall[]
}

export interface AgentToolCall {
  toolName: string
  toolId: string
  args?: unknown
  result?: string
  isError?: boolean
}

export interface WorkflowRunStream {
  status: RunStatus
  events: RunEvent[]
  nodes: Record<string, NodeRunState>
  error: string | null
  run(workflowId: string, input?: unknown, stopAt?: string): void
  cancel(): void
  reset(): void
}

/**
 * useWorkflowRunStream wraps the `/api/v1/workflows/:id/run/stream` WS
 * endpoint. The hook maintains:
 *  - `events`: raw event log (debug + history view)
 *  - `nodes`: per-node materialised state (lookup table for the canvas)
 *  - `status`: connection lifecycle
 *
 * Cancel closes the socket; the server's runCtx then cancels and aborts
 * any in-flight LLM call (loopCtx is derived from request ctx).
 */
export function useWorkflowRunStream(): WorkflowRunStream {
  const [status, setStatus] = useState<RunStatus>('idle')
  const [events, setEvents] = useState<RunEvent[]>([])
  const [nodes, setNodes] = useState<Record<string, NodeRunState>>({})
  const [error, setError] = useState<string | null>(null)
  const wsRef = useRef<WebSocket | null>(null)

  const handleMessage = useCallback((raw: MessageEvent) => {
    let ev: RunEvent
    try {
      ev = JSON.parse(raw.data)
    } catch {
      return
    }
    setEvents((prev) => [...prev, ev])

    setNodes((prev) => {
      const next = { ...prev }
      const id = ev.node_id ?? ''
      const ensure = (): NodeRunState => {
        if (!next[id]) {
          next[id] = {
            nodeId: id,
            nodeType: ev.node_type,
            status: 'pending',
            iters: [],
          }
        }
        return next[id]
      }

      switch (ev.type) {
        case 'step_start': {
          const n = ensure()
          next[id] = { ...n, status: 'running', nodeType: ev.node_type ?? n.nodeType }
          break
        }
        case 'step_done': {
          const n = ensure()
          next[id] = {
            ...n,
            status: ev.is_error ? 'error' : 'done',
            output: ev.output,
            error: ev.error,
            nodeType: ev.node_type ?? n.nodeType,
          }
          break
        }
        case 'agent_iter': {
          const n = ensure()
          // Append new AgentIter entry if iter index isn't already tracked.
          const existing = n.iters.find((i) => i.iter === (ev.iter ?? 0))
          const iters = existing
            ? n.iters
            : [...n.iters, { iter: ev.iter ?? 0, toolCalls: [] }]
          next[id] = { ...n, iters }
          break
        }
        case 'agent_llm': {
          const n = ensure()
          const iters = n.iters.map((it) =>
            it.iter === (ev.iter ?? 0)
              ? { ...it, llm: { text: ev.text, usage: ev.usage } }
              : it,
          )
          next[id] = { ...n, iters }
          break
        }
        case 'agent_tool_call': {
          const n = ensure()
          const iters = n.iters.map((it) =>
            it.iter === (ev.iter ?? 0)
              ? {
                  ...it,
                  toolCalls: [
                    ...it.toolCalls,
                    {
                      toolName: ev.tool_name ?? '',
                      toolId: ev.tool_id ?? '',
                      args: ev.tool_args,
                    },
                  ],
                }
              : it,
          )
          next[id] = { ...n, iters }
          break
        }
        case 'agent_tool_result': {
          const n = ensure()
          const iters = n.iters.map((it) =>
            it.iter === (ev.iter ?? 0)
              ? {
                  ...it,
                  toolCalls: it.toolCalls.map((tc) =>
                    tc.toolId === ev.tool_id
                      ? { ...tc, result: ev.result, isError: ev.is_error }
                      : tc,
                  ),
                }
              : it,
          )
          next[id] = { ...n, iters }
          break
        }
        case 'agent_final': {
          const n = ensure()
          next[id] = { ...n, finalText: ev.text }
          break
        }
        case 'error': {
          setError(ev.error ?? 'unknown error')
          setStatus('error')
          break
        }
        case 'run_done': {
          setStatus('done')
          break
        }
      }
      return next
    })
  }, [])

  const run = useCallback(
    (workflowId: string, input?: unknown, stopAt?: string) => {
      // Reset prior state
      setStatus('connecting')
      setEvents([])
      setNodes({})
      setError(null)

      if (wsRef.current) {
        wsRef.current.close()
      }

      const apiBase = import.meta.env['VITE_API_URL'] ?? 'http://localhost:8080'
      const wsUrl = apiBase.replace(/^http/, 'ws') + `/api/v1/workflows/${workflowId}/run/stream`
      const ws = new WebSocket(wsUrl)
      wsRef.current = ws

      ws.onopen = () => {
        setStatus('running')
        ws.send(
          JSON.stringify({
            type: 'run',
            ...(input !== undefined ? { input } : {}),
            ...(stopAt ? { stop_at: stopAt } : {}),
          }),
        )
      }
      ws.onmessage = handleMessage
      ws.onerror = () => {
        setError('WebSocket connection error')
        setStatus('error')
      }
      ws.onclose = () => {
        wsRef.current = null
        setStatus((s) => (s === 'running' || s === 'connecting' ? 'done' : s))
      }
    },
    [handleMessage],
  )

  const cancel = useCallback(() => {
    if (wsRef.current) {
      wsRef.current.close()
      wsRef.current = null
    }
  }, [])

  const reset = useCallback(() => {
    cancel()
    setStatus('idle')
    setEvents([])
    setNodes({})
    setError(null)
  }, [cancel])

  // Cleanup on unmount.
  useEffect(() => {
    return () => {
      if (wsRef.current) wsRef.current.close()
    }
  }, [])

  return { status, events, nodes, error, run, cancel, reset }
}

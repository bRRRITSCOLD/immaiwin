import { useCallback, useEffect, useRef, useState } from 'react'

export type RunStatus = 'idle' | 'connecting' | 'running' | 'done' | 'error'

// Wire shape — mirrors workflow.RunEvent in Go
// (`internal/workflow/events.go`). Keep in sync.
export interface RunEvent {
  type:
    | 'step_start'
    | 'step_done'
    | 'step_pending'
    | 'agent_iter'
    | 'agent_llm'
    | 'agent_tool_call'
    | 'agent_tool_approval'
    | 'agent_tool_result'
    | 'agent_final'
    | 'run_done'
    | 'cost_exceeded'
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
  // Populated on `agent_llm` events. Lets timeline + run detail views
  // show "anthropic / claude-sonnet-4-6" per iter without chasing
  // connection config.
  provider?: string
  model?: string
  // Populated on the terminal `run_done` event so the UI can flip Run ↔
  // Continue based on whether the run paused mid-way.
  run_id?: string
  status?: 'running' | 'success' | 'error' | 'cancelled' | 'paused'
}

export interface NodeRunState {
  nodeId: string
  nodeType?: string
  status: 'pending' | 'running' | 'done' | 'error' | 'paused' | 'cancelled'
  output?: unknown
  error?: string
  // Agent-specific accumulators
  iters: AgentIter[]
  finalText?: string
}

export interface AgentIter {
  iter: number
  llm?: { text?: string; usage?: RunEvent['usage']; provider?: string; model?: string }
  toolCalls: AgentToolCall[]
}

export interface AgentToolCall {
  toolName: string
  toolId: string
  args?: unknown
  result?: string
  isError?: boolean
  // True when the agent paused before executing this tool call, awaiting
  // a user verdict via approve_tool. Cleared by the timeline buttons
  // (optimistic) and authoritatively by the agent_tool_result event.
  pendingApproval?: boolean
}

export interface WorkflowRunStream {
  status: RunStatus
  events: RunEvent[]
  nodes: Record<string, NodeRunState>
  error: string | null
  // Populated by the terminal `run_done` event. `pausedRunID` is set
  // only when the server flagged the run as `paused` so the next call
  // to `run(..., undefined, undefined, pausedRunID)` resumes mid-loop.
  // Cleared by reset() and by the next non-paused completion.
  pausedRunID: string | null
  lastRunID: string | null
  run(workflowId: string, input?: unknown, stopAt?: string | string[], resumeRunID?: string): void
  // Releases an in-process pre-exec breakpoint pause by sending a
  // `{type:"continue"}` frame over the live WS. Distinct from re-running
  // with `resumeRunID` (which is for cross-process resume from a saved
  // PausedAgent snapshot). Returns true if the frame was sent, false
  // when no live connection.
  continue_(): boolean
  // Send an approve_tool decision for a paused tool call (require_approval
  // gate). Returns true when the frame was sent. Optimistically clears
  // the pendingApproval flag locally; server's agent_tool_result event
  // is the authoritative status.
  approveTool(toolId: string, approved: boolean, reason?: string): boolean
  // Live-mutate the run's breakpoint set. Sends a set_breakpoints WS
  // frame; server replaces the executor's stopAtSet so subsequent BFS
  // steps honour the new list. Returns true when the frame was sent.
  // No-op when no live WS connection.
  setBreakpoints(nodeIds: string[]): boolean
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
  const [pausedRunID, setPausedRunID] = useState<string | null>(null)
  const [lastRunID, setLastRunID] = useState<string | null>(null)
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
        case 'step_pending': {
          const n = ensure()
          next[id] = { ...n, status: 'paused', nodeType: ev.node_type ?? n.nodeType }
          break
        }
        case 'step_done': {
          const n = ensure()
          // Status precedence: error > paused > done. Paused is set by the
          // executor only on the agent node when env.stopAtHit short-
          // circuits its ReAct loop; the agent's tool target gets a
          // normal step_done (it really did succeed).
          let s: NodeRunState['status']
          if (ev.is_error) s = 'error'
          else if (ev.status === 'paused') s = 'paused'
          else s = 'done'
          next[id] = {
            ...n,
            status: s,
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
              ? { ...it, llm: { text: ev.text, usage: ev.usage, provider: ev.provider, model: ev.model } }
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
        case 'agent_tool_approval': {
          // Server is paused waiting for the user to Approve/Reject this
          // tool call. The matching agent_tool_call event has already
          // populated the toolCalls entry; flip the local pendingApproval
          // flag so the timeline renders the buttons.
          const n = ensure()
          const iters = n.iters.map((it) =>
            it.iter === (ev.iter ?? 0)
              ? {
                  ...it,
                  toolCalls: it.toolCalls.map((tc) =>
                    tc.toolId === ev.tool_id ? { ...tc, pendingApproval: true } : tc,
                  ),
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
                      ? { ...tc, result: ev.result, isError: ev.is_error, pendingApproval: false }
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
        case 'cost_exceeded': {
          // Daily / per-run cost cap breached. Treated as a terminal error
          // for the run; surfaced to the route's effect via `error` so the
          // cap reason lands in a red toast instead of a generic black one.
          setError(ev.error ?? 'cost cap exceeded')
          setStatus('error')
          break
        }
        case 'run_done': {
          setStatus('done')
          if (ev.run_id) setLastRunID(ev.run_id)
          // Track paused run ID separately so a subsequent run() call
          // can choose to send `resume_run_id`. Clear on any non-paused
          // terminal status (success / error / cancelled).
          if (ev.status === 'paused' && ev.run_id) {
            setPausedRunID(ev.run_id)
          } else {
            setPausedRunID(null)
          }
          break
        }
      }
      return next
    })
  }, [])

  const run = useCallback(
    (workflowId: string, input?: unknown, stopAt?: string | string[], resumeRunID?: string) => {
      // Reset prior state. Don't clear pausedRunID yet — caller may pass
      // it as resumeRunID; the run_done handler clears it on success.
      setStatus('connecting')
      setError(null)
      if (resumeRunID) {
        // On resume, keep the prior node snapshot so already-completed
        // nodes (HTTP get_weather, etc.) remain visible as success
        // instead of flickering to "idle" then "not executed". The
        // server's resume path skips re-running them; without this,
        // the canvas would lose all context. Reset agent's paused node
        // back to "running" so the live indicator updates correctly.
        setEvents([])
        setNodes((prev) => {
          const next = { ...prev }
          for (const id of Object.keys(next)) {
            const n = next[id]!
            if (n.status === 'paused') {
              next[id] = { ...n, status: 'running' }
            }
          }
          return next
        })
      } else {
        setEvents([])
        setNodes({})
      }

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
            ...(resumeRunID ? { resume_run_id: resumeRunID } : {}),
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
    // Server context cancel doesn't reach the UI as a step_done event
    // (the WS is already gone by the time the goroutine winds down), so
    // synthesise the terminal state here. Any node still in-flight at
    // cancel time gets marked `cancelled` — distinct from `error` (which
    // implies the workflow itself failed).
    setNodes((prev) => {
      const next = { ...prev }
      for (const id of Object.keys(next)) {
        const n = next[id]!
        if (n.status === 'running' || n.status === 'paused' || n.status === 'pending') {
          next[id] = { ...n, status: 'cancelled' }
        }
      }
      return next
    })
    setStatus('done')
    setPausedRunID(null)
  }, [])

  const approveTool = useCallback((toolId: string, approved: boolean, reason?: string) => {
    if (!wsRef.current || wsRef.current.readyState !== WebSocket.OPEN) {
      return false
    }
    wsRef.current.send(
      JSON.stringify({
        type: 'approve_tool',
        tool_call_id: toolId,
        approved,
        ...(reason ? { reason } : {}),
      }),
    )
    // Optimistically clear pendingApproval so the timeline doesn't lag
    // behind the WS round-trip. Authoritative clear comes from the
    // server's agent_tool_result event when the tool finishes (or is
    // rejected on the server side).
    setNodes((prev) => {
      const next = { ...prev }
      for (const id of Object.keys(next)) {
        const n = next[id]!
        const iters = n.iters.map((it) => ({
          ...it,
          toolCalls: it.toolCalls.map((tc) =>
            tc.toolId === toolId ? { ...tc, pendingApproval: false } : tc,
          ),
        }))
        next[id] = { ...n, iters }
      }
      return next
    })
    return true
  }, [])

  const setBreakpoints = useCallback((nodeIds: string[]) => {
    if (!wsRef.current || wsRef.current.readyState !== WebSocket.OPEN) {
      return false
    }
    wsRef.current.send(JSON.stringify({ type: 'set_breakpoints', node_ids: nodeIds }))
    return true
  }, [])

  const continue_ = useCallback(() => {
    if (!wsRef.current || wsRef.current.readyState !== WebSocket.OPEN) {
      return false
    }
    wsRef.current.send(JSON.stringify({ type: 'continue' }))
    // Optimistically flip every paused node back to running so the UI
    // doesn't lag behind the WS round-trip. Server will resync via
    // step_start when the breakpoint actually unblocks.
    setNodes((prev) => {
      const next = { ...prev }
      for (const id of Object.keys(next)) {
        const n = next[id]!
        if (n.status === 'paused') next[id] = { ...n, status: 'running' }
      }
      return next
    })
    return true
  }, [])

  const reset = useCallback(() => {
    cancel()
    setStatus('idle')
    setEvents([])
    setNodes({})
    setError(null)
    setPausedRunID(null)
    setLastRunID(null)
  }, [cancel])

  // Cleanup on unmount.
  useEffect(() => {
    return () => {
      if (wsRef.current) wsRef.current.close()
    }
  }, [])

  return { status, events, nodes, error, run, continue_, approveTool, setBreakpoints, cancel, reset, pausedRunID, lastRunID }
}

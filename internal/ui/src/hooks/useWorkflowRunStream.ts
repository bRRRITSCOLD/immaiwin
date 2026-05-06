import { useCallback, useEffect, useRef, useState } from 'react'
import { api, buildWSURL } from '~/lib/api'

export type RunStatus = 'idle' | 'connecting' | 'running' | 'done' | 'error' | 'cancelled'

// Wire shape — mirrors workflow.RunEvent in Go
// (`internal/workflow/events.go`). Keep in sync.
export interface RunEvent {
  type:
    | 'run_start'
    | 'step_start'
    | 'step_done'
    | 'step_pending'
    | 'agent_iter'
    | 'agent_llm'
    | 'agent_tool_call'
    | 'agent_tool_approval'
    | 'node_approval_pending'
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
  // Populated when a `node_approval_pending` event lands or when an
  // agent's per-tool approval gate fires on the OOB path. Lets the
  // canvas surface a banner with a deep-link to `/runs/:runID` so the
  // user can Approve/Reject when the dispatcher (Slack / email) failed
  // or is unwired. Distinct from `pausedRunID` (breakpoint resume).
  // Cleared by reset() and by a terminal `run_done` event.
  pendingApprovalRunID: string | null
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
  const [pendingApprovalRunID, setPendingApprovalRunID] = useState<string | null>(null)
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
        case 'run_start': {
          // Synthetic envelope from the WS handler so the hook learns the
          // server-side run_id ASAP — needed by approveTool / cancel which
          // both POST to /api/v1/workflow_runs/<id>/... and would silently
          // drop the click without a run id to target.
          if (ev.run_id) setLastRunID(ev.run_id)
          break
        }
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
          const iterIdx = ev.iter ?? 0
          const existing = n.iters.find((i) => i.iter === iterIdx)
          // Receiving an agent_iter for an iter index already in
          // n.iters means the agent restarted from iter 0 — the
          // worker died mid-run and a fresh worker re-claimed the
          // run and re-ran the agent (no per-iter checkpoint yet,
          // see FUTURE-FEATURES.md). Drop the dead attempt's iters
          // so the live canvas reflects the active execution; the
          // historical view on /runs/:id keeps every attempt and
          // groups them by attempt boundary.
          const iters = existing
            ? [{ iter: iterIdx, toolCalls: [] }]
            : [...n.iters, { iter: iterIdx, toolCalls: [] }]
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
        case 'node_approval_pending': {
          // Pre-exec node approval gate fired (require_node_approval).
          // Flip the node into a `paused` state so the canvas dot turns
          // amber/yellow instead of staying "running" while server
          // blocks on the approval channel. Distinct from step_pending
          // (breakpoint) — both render the same paused indicator but
          // the run record's status differs (pending_approval vs
          // running).
          const n = ensure()
          next[id] = { ...n, status: 'paused', nodeType: ev.node_type ?? n.nodeType }
          // Also stash the run_id so the canvas can render a banner
          // pointing at /runs/:id where the user resolves the gate.
          if (ev.run_id) {
            setPendingApprovalRunID(ev.run_id)
          }
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
          // Stash run_id so the canvas can show a "resolve via /runs/:id"
          // banner — useful when no live approve buttons are available
          // (e.g. live WS dropped, dispatcher failed). Live UI's inline
          // Approve/Reject still works alongside.
          if (ev.run_id) {
            setPendingApprovalRunID(ev.run_id)
          }
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
          // Distinct hook statuses for distinct terminal outcomes so
          // the route's toast can pick the right copy. Cancelled and
          // error get their own toasts; only `success` gets the green
          // "Workflow completed" message.
          if (ev.status === 'cancelled') {
            setStatus('cancelled')
          } else if (ev.status === 'error') {
            setStatus('error')
            if (ev.error) setError(ev.error)
          } else {
            setStatus('done')
          }
          if (ev.run_id) setLastRunID(ev.run_id)
          // Track paused run ID separately so a subsequent run() call
          // can choose to send `resume_run_id`. Clear on any non-paused
          // terminal status (success / error / cancelled).
          if (ev.status === 'paused' && ev.run_id) {
            setPausedRunID(ev.run_id)
          } else {
            setPausedRunID(null)
          }
          // Pending-approval banner clears on every terminal status —
          // the gate either resolved (success / error follow), got
          // cancelled, or rejected.
          setPendingApprovalRunID(null)
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

      void (async () => {
        let wsUrl: string
        try {
          wsUrl = await buildWSURL(`/api/v1/workflows/${workflowId}/run/stream`)
        } catch {
          setError('failed to mint ws token (auth required)')
          setStatus('error')
          return
        }
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
      })()
    },
    [handleMessage],
  )

  const cancel = useCallback(() => {
    // Lease unification: the WS handler no longer drives execution —
    // the worker owns the run via the lease. Closing the socket does
    // NOT cancel the run; we have to hit POST /cancel so the worker
    // record flips terminal and the lease releases. Best-effort —
    // network failures still close the socket so the UI stops
    // streaming, but log so the user can retry from /runs/:id.
    const id = lastRunID
    if (id) {
      api.post(`/api/v1/workflow_runs/${id}/cancel`).catch((err: unknown) => {
        console.warn('cancel run failed; the run may keep running on the worker', err)
      })
    }
    // Don't synthesise terminal state here — the cancel handler
    // publishes a `run_done` envelope on the per-run event channel,
    // and the WS subscriber forwards it to the browser. Optimistic
    // status='done' would fire the "Workflow completed" toast a
    // round-trip too early (and incorrectly, since the run was
    // cancelled, not completed). Just nudge in-flight nodes so the
    // canvas doesn't look frozen for the ~50ms before run_done lands.
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
    setPausedRunID(null)
  }, [lastRunID])

  const approveTool = useCallback((toolId: string, approved: boolean, reason?: string) => {
    // Lease unification: the agent runs in the worker; the WS handler
    // is just an event subscriber. Approval decisions cross the
    // process boundary via POST /approval (same endpoint the
    // /runs/:id page uses) → ApplyApprovalDecision → wakeup → the
    // worker re-claims and feeds the decision into the agent loop.
    const id = lastRunID
    if (!id) {
      return false
    }
    api
      .post(`/api/v1/workflow_runs/${id}/approval`, {
        tool_call_id: toolId,
        approved,
        ...(reason ? { reason } : {}),
      })
      .catch((err: unknown) => {
        console.warn('approve tool failed', err)
      })
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
  }, [lastRunID])

  const setBreakpoints = useCallback((_nodeIds: string[]) => {
    // Phase 2 TODO: route through a Redis control channel so the
    // worker can mutate the run's breakpoint set mid-flight. The old
    // WS frame doesn't reach the worker any more (lease unification);
    // this is a no-op until the control bridge lands. Surfacing as a
    // false return so callers know nothing happened.
    console.warn('setBreakpoints: control channel not yet wired (Phase 2)')
    return false
  }, [])

  const continue_ = useCallback(() => {
    // Phase 2 TODO: same story as setBreakpoints — needs a control
    // channel so the worker's runEnv can release a breakpoint pause
    // from outside its process. Until then, breakpoint pauses on
    // canvas runs require restarting the run.
    console.warn('continue: control channel not yet wired (Phase 2)')
    return false
  }, [])

  const reset = useCallback(() => {
    cancel()
    setStatus('idle')
    setEvents([])
    setNodes({})
    setError(null)
    setPausedRunID(null)
    setPendingApprovalRunID(null)
    setLastRunID(null)
  }, [cancel])

  // Cleanup on unmount.
  useEffect(() => {
    return () => {
      if (wsRef.current) wsRef.current.close()
    }
  }, [])

  return { status, events, nodes, error, run, continue_, approveTool, setBreakpoints, cancel, reset, pausedRunID, pendingApprovalRunID, lastRunID }
}

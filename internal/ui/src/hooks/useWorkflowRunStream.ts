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
    | 'loop_iter_start'
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
  loop_iter?: number // 1-based for_each iteration (0/absent = not looped)
  loop_total?: number // total for_each iterations (M) — "iter K/M" denominator
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
  // for_each body nodes run once per loop element; without this the
  // single status/output slot is overwritten every iteration and the
  // canvas can't show which iteration it's on (or that it re-ran).
  // One snapshot per loop_iter — workflows.tsx maps this to the
  // per-iteration StepResult[] that NodeDebugPanel already pages
  // ("iter N/total"). Empty/absent for non-looped nodes.
  loopSteps?: {
    loopIter: number
    status: NodeRunState['status']
    output?: unknown
    error?: string
  }[]
  // Loop total (M) from the for_each — the "iter K/M" denominator,
  // uniform across the whole body subgraph even for a node that
  // doesn't run every iteration.
  loopTotal?: number
  // Agent-specific accumulators
  iters: AgentIter[]
  finalText?: string
}

export interface AgentIter {
  iter: number
  loopIter?: number // for_each iteration this belongs to (0/undef = not looped)
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
  // streamStale = true when status is 'running' but no event has
  // arrived for STREAM_STALE_AFTER_MS. Heuristic for "worker may
  // have died, awaiting recovery" — the api's keepalive ping keeps
  // the WS open during legitimate pauses (breakpoint, slow LLM),
  // but if the worker actually dies the orphan-sweep/reclaim path
  // takes ~30–60s during which the user sees no node activity.
  // The canvas surfaces this as a banner so the user isn't staring
  // at a frozen-looking screen wondering what happened.
  streamStale: boolean
}

// STREAM_STALE_AFTER_MS — quiet period before the canvas shows
// "worker may have died" banner. Tuned > the worker's lease TTL
// (30s) + heartbeat (10s) so legitimate-but-quiet runs don't
// flicker the warning.
const STREAM_STALE_AFTER_MS = 45_000

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
// upsertLoopStep returns a new loopSteps array with the entry for
// `loopIter` created or shallow-merged. Keeps one snapshot per
// for_each iteration so the canvas can page them ("iter N/total")
// instead of collapsing every iteration into one overwritten slot.
function upsertLoopStep(
  prev: NodeRunState['loopSteps'],
  loopIter: number,
  patch: Partial<NonNullable<NodeRunState['loopSteps']>[number]>,
): NodeRunState['loopSteps'] {
  const arr = prev ? [...prev] : []
  const i = arr.findIndex((s) => s.loopIter === loopIter)
  if (i === -1) {
    arr.push({ loopIter, status: 'running', ...patch })
  } else {
    arr[i] = { ...arr[i]!, ...patch }
  }
  arr.sort((a, b) => a.loopIter - b.loopIter)
  return arr
}

export function useWorkflowRunStream(): WorkflowRunStream {
  const [status, setStatus] = useState<RunStatus>('idle')
  const [events, setEvents] = useState<RunEvent[]>([])
  const [nodes, setNodes] = useState<Record<string, NodeRunState>>({})
  const [error, setError] = useState<string | null>(null)
  const [pausedRunID, setPausedRunID] = useState<string | null>(null)
  const [pendingApprovalRunID, setPendingApprovalRunID] = useState<string | null>(null)
  const [lastRunID, setLastRunID] = useState<string | null>(null)
  const [lastEventAt, setLastEventAt] = useState<number | null>(null)
  const [now, setNow] = useState<number>(Date.now())
  const wsRef = useRef<WebSocket | null>(null)

  // Periodic now-tick drives the streamStale flag without each
  // component computing its own setInterval. 5s cadence is fine —
  // the threshold is 45s so 5s precision doesn't matter UX-wise.
  useEffect(() => {
    if (status !== 'running') return
    const t = window.setInterval(() => setNow(Date.now()), 5000)
    return () => window.clearInterval(t)
  }, [status])

  const handleMessage = useCallback((raw: MessageEvent) => {
    let ev: RunEvent
    try {
      ev = JSON.parse(raw.data)
    } catch {
      return
    }
    setEvents((prev) => [...prev, ev])
    setLastEventAt(Date.now())

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
        case 'loop_iter_start': {
          // for_each advanced — reset THIS body node to idle/pending
          // for the new iteration before it (maybe) runs, so a node
          // still showing the previous iteration's "done" flips to
          // "idle · iter K/M" the instant the loop moves on.
          const n = ensure()
          const li = ev.loop_iter ?? 0
          next[id] = {
            ...n,
            status: 'pending',
            nodeType: ev.node_type ?? n.nodeType,
            loopTotal: ev.loop_total ?? n.loopTotal,
            loopSteps:
              li > 0 ? upsertLoopStep(n.loopSteps, li, { status: 'pending' }) : n.loopSteps,
          }
          break
        }
        case 'step_start': {
          const n = ensure()
          const li = ev.loop_iter ?? 0
          next[id] = {
            ...n,
            status: 'running',
            nodeType: ev.node_type ?? n.nodeType,
            loopTotal: ev.loop_total ?? n.loopTotal,
            loopSteps:
              li > 0 ? upsertLoopStep(n.loopSteps, li, { status: 'running' }) : n.loopSteps,
          }
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
          const li = ev.loop_iter ?? 0
          next[id] = {
            ...n,
            status: s,
            output: ev.output,
            error: ev.error,
            nodeType: ev.node_type ?? n.nodeType,
            loopTotal: ev.loop_total ?? n.loopTotal,
            loopSteps:
              li > 0
                ? upsertLoopStep(n.loopSteps, li, {
                    status: s,
                    output: ev.output,
                    error: ev.error,
                  })
                : n.loopSteps,
          }
          break
        }
        case 'agent_iter': {
          const n = ensure()
          const iterIdx = ev.iter ?? 0
          const loopIdx = ev.loop_iter ?? 0
          // Seeing the SAME (iter, loop_iter) pair again = a genuine
          // restart: the worker died and a fresh worker re-ran the
          // whole run from scratch (no per-iter checkpoint, see
          // FUTURE-FEATURES.md). Reset so the live canvas shows only
          // the active attempt; /runs/:id keeps full history.
          // A new loop_iter (for_each advancing to the next element)
          // is NOT a restart — it's a different (iter, loop) pair, so
          // it appends and every loop iteration stays visible without
          // bleeding into the previous one.
          const restarted = n.iters.some(
            (i) => i.iter === iterIdx && (i.loopIter ?? 0) === loopIdx,
          )
          const iters = restarted
            ? [{ iter: iterIdx, loopIter: loopIdx, toolCalls: [] }]
            : [...n.iters, { iter: iterIdx, loopIter: loopIdx, toolCalls: [] }]
          next[id] = { ...n, iters }
          break
        }
        case 'agent_llm': {
          const n = ensure()
          const iters = n.iters.map((it) =>
            it.iter === (ev.iter ?? 0) && (it.loopIter ?? 0) === (ev.loop_iter ?? 0)
              ? { ...it, llm: { text: ev.text, usage: ev.usage, provider: ev.provider, model: ev.model } }
              : it,
          )
          next[id] = { ...n, iters }
          break
        }
        case 'agent_tool_call': {
          const n = ensure()
          const iters = n.iters.map((it) =>
            it.iter === (ev.iter ?? 0) && (it.loopIter ?? 0) === (ev.loop_iter ?? 0)
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
            it.iter === (ev.iter ?? 0) && (it.loopIter ?? 0) === (ev.loop_iter ?? 0)
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
            it.iter === (ev.iter ?? 0) && (it.loopIter ?? 0) === (ev.loop_iter ?? 0)
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
          // Sweep stuck nodes: on a cancelled/error terminal the worker
          // abandons mid-graph, so an in-flight node's own step_done
          // may never arrive (e.g. cancelled for_each body / http).
          // Without this it spins "Running" forever. Flip any
          // non-terminal node (and its loop iterations) to 'cancelled'.
          if (ev.status === 'cancelled' || ev.status === 'error') {
            for (const nid of Object.keys(next)) {
              const nn = next[nid]!
              const stuck =
                nn.status === 'running' ||
                nn.status === 'pending' ||
                nn.status === 'paused'
              let lsChanged = false
              const ls = nn.loopSteps?.map((s) => {
                if (s.status === 'running' || s.status === 'pending') {
                  lsChanged = true
                  return { ...s, status: 'cancelled' as const }
                }
                return s
              })
              if (stuck || lsChanged) {
                next[nid] = {
                  ...nn,
                  status: stuck ? 'cancelled' : nn.status,
                  loopSteps: lsChanged ? ls : nn.loopSteps,
                }
              }
            }
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
        setLastEventAt(Date.now())
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
        setLastEventAt(Date.now())
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

  const setBreakpoints = useCallback((nodeIds: string[]) => {
    // Routes through the Phase-2 control bridge: PUT publishes a
    // RunControlMessage on burrow:run_control:<runID>; the worker's
    // bridge goroutine mutates env.stopAtSet which the next BFS
    // step honours. Best-effort — terminal runs return 400, an
    // already-finished run won't have a worker listening.
    if (!lastRunID) {
      console.warn('setBreakpoints: no active run')
      return false
    }
    api
      .put(`/api/v1/workflow_runs/${lastRunID}/breakpoints`, { node_ids: nodeIds })
      .catch((err) => {
        console.warn('setBreakpoints failed:', err)
      })
    return true
  }, [lastRunID])

  const continue_ = useCallback(() => {
    // Routes through the Phase-2 control bridge — same pattern as
    // setBreakpoints. Worker bridge wakes runEnv.continueCh which
    // releases the pre-exec breakpoint pause.
    if (!lastRunID) {
      console.warn('continue: no active run')
      return false
    }
    api
      .post(`/api/v1/workflow_runs/${lastRunID}/continue`, {})
      .then(() => {
        // Optimistically clear local paused indicator so the UI
        // flips to running immediately. Authoritative status flows
        // back over the live event stream.
        setPausedRunID((prev) => (prev === lastRunID ? null : prev))
      })
      .catch((err) => {
        console.warn('continue failed:', err)
      })
    return true
  }, [lastRunID])

  const reset = useCallback(() => {
    cancel()
    setStatus('idle')
    setEvents([])
    setNodes({})
    setError(null)
    setPausedRunID(null)
    setPendingApprovalRunID(null)
    setLastRunID(null)
    setLastEventAt(null)
  }, [cancel])

  // Cleanup on unmount.
  useEffect(() => {
    return () => {
      if (wsRef.current) wsRef.current.close()
    }
  }, [])

  const streamStale =
    status === 'running' &&
    lastEventAt !== null &&
    now - lastEventAt > STREAM_STALE_AFTER_MS

  return { status, events, nodes, error, run, continue_, approveTool, setBreakpoints, cancel, reset, pausedRunID, pendingApprovalRunID, lastRunID, streamStale }
}

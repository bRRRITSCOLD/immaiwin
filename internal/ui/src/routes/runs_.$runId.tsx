// /runs/:runId — single workflow run detail.
//
// Shows the persisted run record + (if available) the workflow doc the
// run executed against so we can label nodes by name. Renders three
// sections:
//
//  1. Header with metadata + Replay button
//  2. Trigger input (raw JSON, collapsible)
//  3. Per-node steps (output / error)
//  4. Per-agent traces (iter timeline)
//
// "Replay" POSTs the original trigger_input back to /api/v1/workflows/:id/run
// — the async lease path (PR 3.3). The endpoint persists a run record,
// publishes a wakeup, and returns 202 with the new run_id; a workflow-
// executor worker claims the lease and runs the BFS. We navigate to the
// new run id so the user lands on a fresh detail view that polls for
// progress. Distinct from the canvas Run button which uses the WS
// streaming path (RunResumable, BypassLease=true) for live debugger UX.

import { createFileRoute, Link } from '@tanstack/react-router'
import { useCallback, useEffect, useState } from 'react'
import { toast } from 'sonner'
import { ArrowLeft, Play } from 'lucide-react'
import { Badge } from '~/components/ui/badge'
import { Button } from '~/components/ui/button'
import { api, ApiError } from '~/lib/api'
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from '~/components/ui/collapsible'
import { ChevronDown, ChevronRight } from 'lucide-react'

export const Route = createFileRoute('/runs_/$runId')({
  component: RunDetailPage,
})


type RunStatus = 'queued' | 'running' | 'success' | 'error' | 'cancelled' | 'paused' | 'pending_approval'

interface UsageTotal {
  input_tokens?: number
  output_tokens?: number
  total_tokens?: number
  cost_usd?: number
}

interface StepResult {
  node_id: string
  node_type: string
  output?: unknown
  error?: string
  // Continued is set by the executor when a node errored but its
  // `on_error: continue` policy suppressed run-status promotion.
  // Renders as a distinct amber "continued" badge instead of red
  // "error" so the suppressed failure stays visible.
  continued?: boolean
}

interface TraceEvent {
  at: string
  type: 'iter_start' | 'llm_call' | 'tool_call' | 'tool_result' | 'final'
  iter?: number
  loop_iter?: number // 1-based for_each iteration (0/absent = not looped)
  tool_name?: string
  tool_id?: string
  tool_args?: unknown
  result?: unknown
  text?: string
  usage?: UsageTotal
  is_error?: boolean
  provider?: string
  model?: string
}

interface PausedAgent {
  agent_node_id?: string
  iter?: number
}

interface PendingApproval {
  kind?: 'tool_call' | 'node'
  // tool_call kind
  agent_node_id?: string
  iter?: number
  tool_call_id?: string
  tool_name?: string
  tool_args?: unknown
  // node kind
  node_id?: string
  node_type?: string
  node_input?: unknown
  node_name?: string
  requested_at: string
  // Populated when the OOB dispatcher (Slack / SMTP) failed to deliver
  // the magic-link prompt. The gate still resolves through the
  // Approve/Reject buttons below; this string explains why no email or
  // Slack message arrived.
  dispatch_error?: string
}

interface WorkflowRun {
  id: string
  workflow_id: string
  tenant_id: string
  queued_at: string
  started_at?: string
  finished_at?: string
  status: RunStatus
  trigger_input?: unknown
  params?: Record<string, string>
  steps?: StepResult[]
  agent_traces?: Record<string, TraceEvent[]>
  usage?: UsageTotal
  error?: string
  paused_agent?: PausedAgent | null
  pending_approval?: PendingApproval | null
}

interface WorkflowNode {
  id: string
  type: string
  data?: { name?: string; label?: string; [k: string]: unknown }
}

interface WorkflowDoc {
  id: string
  name: string
  nodes?: WorkflowNode[]
}

interface RunDetailResponse {
  run: WorkflowRun
  workflow: WorkflowDoc | null
}

// statusBadgeClass mirrors /runs table colors + NodeDebugPanel dots so
// the user sees the same colour for the same state regardless of view.
function statusBadgeClass(s: RunStatus): string {
  switch (s) {
    case 'success':
      return 'bg-green-600 hover:bg-green-600 text-white border-transparent'
    case 'error':
      return 'bg-red-600 hover:bg-red-600 text-white border-transparent'
    case 'cancelled':
      return 'bg-zinc-500 hover:bg-zinc-500 text-white border-transparent'
    case 'paused':
      return 'bg-yellow-500 hover:bg-yellow-500 text-black border-transparent'
    case 'pending_approval':
      return 'bg-amber-500 hover:bg-amber-500 text-black border-transparent'
    case 'queued':
      return 'bg-slate-500 hover:bg-slate-500 text-white border-transparent'
    case 'running':
    default:
      return 'bg-blue-500 hover:bg-blue-500 text-white border-transparent animate-pulse'
  }
}

// finished - started (run time only — queue wait excluded). Returns
// "—" if either bound is missing.
function formatDuration(start?: string, end?: string): string {
  if (!start || !end) return '—'
  const ms = new Date(end).getTime() - new Date(start).getTime()
  if (!Number.isFinite(ms) || ms < 0) return '—'
  if (ms < 1000) return `${ms}ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`
  const m = Math.floor(ms / 60_000)
  const s = Math.floor((ms % 60_000) / 1000)
  return `${m}m ${s}s`
}

// Detail page mirrors the live canvas AgentCostBadge: always 4 decimals so
// sub-cent ai-agent calls stay visible. Persisted cost_usd is full-precision
// float64 (executor.aggregateUsage sums it from per-llm_call traces).
function formatCost(usd?: number): string {
  return `$${(usd ?? 0).toFixed(4)}`
}

function nodeLabel(wf: WorkflowDoc | null, nodeId: string, fallbackType?: string): string {
  if (!wf?.nodes) return fallbackType ? `${fallbackType} (${nodeId})` : nodeId
  const n = wf.nodes.find((x) => x.id === nodeId)
  if (!n) return fallbackType ? `${fallbackType} (${nodeId})` : nodeId
  const name = (n.data?.name as string | undefined) ?? (n.data?.label as string | undefined)
  if (name) return `${name} (${n.type})`
  return `${n.type} (${nodeId})`
}

function PrettyJSON({ value }: { value: unknown }) {
  if (value === undefined || value === null) {
    return <span className="text-muted-foreground italic">empty</span>
  }
  let text: string
  try {
    text = typeof value === 'string' ? value : JSON.stringify(value, null, 2)
  } catch {
    text = String(value)
  }
  return (
    <pre className="text-xs bg-muted/50 rounded p-2 overflow-auto max-h-96 font-mono whitespace-pre-wrap break-words">
      {text}
    </pre>
  )
}

function CollapsibleSection({
  title,
  defaultOpen,
  children,
}: {
  title: React.ReactNode
  defaultOpen?: boolean
  children: React.ReactNode
}) {
  const [open, setOpen] = useState(!!defaultOpen)
  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <CollapsibleTrigger className="flex items-center gap-1 text-sm font-medium hover:text-primary transition-colors">
        {open ? <ChevronDown className="h-4 w-4" /> : <ChevronRight className="h-4 w-4" />}
        {title}
      </CollapsibleTrigger>
      <CollapsibleContent className="mt-2">{children}</CollapsibleContent>
    </Collapsible>
  )
}

function RunDetailPage() {
  const { runId } = Route.useParams()
  const [data, setData] = useState<RunDetailResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [replaying, setReplaying] = useState(false)
  const [submittingApproval, setSubmittingApproval] = useState(false)
  const [cancelling, setCancelling] = useState(false)

  const load = useCallback(async () => {
    // Only show the loading spinner on the first fetch — polls during
    // pending_approval would otherwise flash the spinner every 2s.
    setData((prev) => {
      if (prev === null) setLoading(true)
      return prev
    })
    try {
      const next = await api.get<RunDetailResponse>(`/api/v1/workflow_runs/${runId}`)
      // Skip the setState when the new payload is byte-equivalent to
      // the current one. Polling otherwise rebuilds the entire run-
      // detail subtree every 2s and the user sees a noticeable
      // flicker on the canvas. JSON.stringify compare is cheap for
      // these payloads (~1-10kb).
      setData((prev) => {
        if (prev && JSON.stringify(prev) === JSON.stringify(next)) {
          return prev
        }
        return next
      })
    } catch (err) {
      toast.error(err instanceof ApiError ? `Failed to load run: ${err.message}` : 'Network error loading run')
    } finally {
      setLoading(false)
    }
  }, [runId])

  useEffect(() => {
    load()
  }, [load])

  // Poll while the run is non-terminal so the user sees status flips
  // (pending_approval → running → next pending_approval → … →
  // success/error). 2s is gentle enough not to flood the API. Stops
  // polling on terminal states.
  useEffect(() => {
    const status = data?.run.status
    if (!status) return
    const terminal = status === 'success' || status === 'error' || status === 'cancelled'
    if (terminal) return
    const id = setInterval(() => {
      load()
    }, 2000)
    return () => clearInterval(id)
  }, [data?.run.status, load])

  const submitApproval = useCallback(
    async (approved: boolean, reason?: string) => {
      if (!data?.run.pending_approval) return
      setSubmittingApproval(true)
      try {
        // Backend's ApprovalDecision carries an opaque tool_call_id
        // field used as the correlation key. For node-kind gates we
        // pass node_id under the same field so the agent / executor
        // sees a non-empty match. The receiver treats both kinds the
        // same — only the wait-side cares about the value.
        const correlationID =
          data.run.pending_approval.kind === 'node'
            ? data.run.pending_approval.node_id
            : data.run.pending_approval.tool_call_id
        await api.post(`/api/v1/workflow_runs/${data.run.id}/approval`, {
          tool_call_id: correlationID,
          approved,
          reason,
        })
        if (approved) {
          toast.success('Approved — run resuming')
        } else {
          // Reject is now a hard-fail contract (run lands `error`),
          // so the toast shouldn't promise a resume that won't
          // happen. Use info + an explicit "ending" verb.
          toast.info('Rejected — ending run')
        }
        // Refresh once immediately; the polling effect picks up from
        // there. Server still has to flush state writes, so initial
        // refresh may show pending — the poll covers that.
        load()
      } catch (err) {
        // 410 Gone = orphan run (api restart while paused). The
        // server already auto-cancelled the run; the polling effect
        // picks up the new terminal state. Emit a friendlier toast
        // so the user knows what happened.
        if (err instanceof ApiError && err.status === 410) {
          toast.error('Run was abandoned during a restart — auto-cancelled. Re-trigger the workflow to retry.', { duration: 10000 })
          load()
        } else {
          toast.error(err instanceof ApiError ? `Approval submit failed: ${err.message}` : 'Network error submitting approval')
        }
      } finally {
        setSubmittingApproval(false)
      }
    },
    [data, load],
  )

  const handleCancel = useCallback(async () => {
    if (!data?.run) return
    if (!confirm('Force-cancel this run? The run record will be marked as cancelled. If a worker is still alive somewhere, it will receive a reject signal and unblock.')) return
    setCancelling(true)
    try {
      await api.post(`/api/v1/workflow_runs/${data.run.id}/cancel`)
      toast.success('Run cancelled')
      load()
    } catch (err) {
      toast.error(err instanceof ApiError ? `Cancel failed: ${err.message}` : 'Network error cancelling run')
    } finally {
      setCancelling(false)
    }
  }, [data, load])

  const handleReplay = useCallback(async () => {
    if (!data?.run) return
    setReplaying(true)
    try {
      const body = await api.post<{ run_id?: string }>(`/api/v1/workflows/${data.run.workflow_id}/run`, {
        input: data.run.trigger_input ?? null,
      })
      // POST /workflows/:id/run is async (lease path) — the response
      // returns the new run_id immediately; execution happens out-of-
      // process in the worker. Open the new run's detail in a fresh
      // tab so the current view stays parked on the original run; the
      // detail page polls and updates as the worker progresses.
      const newID = body.run_id
      if (newID) {
        toast.success('Replay completed')
        window.open(`/runs/${newID}`, '_blank', 'noopener,noreferrer')
      } else {
        toast.success('Replay completed (no new run id)')
        load()
      }
    } catch {
      toast.error('Network error replaying run')
    } finally {
      setReplaying(false)
    }
  }, [data, load])

  return (
      <main className="flex-1 min-h-0 overflow-y-auto p-6">
        <div className="max-w-5xl mx-auto">
          <div className="mb-4">
            <Button variant="ghost" size="sm" asChild>
              <Link to="/runs">
                <ArrowLeft className="h-4 w-4 mr-1" />
                Back to runs
              </Link>
            </Button>
          </div>

          {loading && <div className="text-muted-foreground text-sm">Loading…</div>}

          {!loading && data && (
            <>
              <div className="border rounded-md bg-card p-4 mb-6">
                <div className="flex items-start justify-between gap-4 flex-wrap">
                  <div>
                    <div className="text-sm text-muted-foreground">
                      {data.workflow?.name ?? data.run.workflow_id}
                    </div>
                    <div className="text-2xl font-semibold tracking-tight font-mono">
                      {data.run.id}
                    </div>
                    <div className="mt-2 flex items-center gap-2 flex-wrap">
                      <Badge className={statusBadgeClass(data.run.status)}>{data.run.status}</Badge>
                      {data.run.paused_agent && (
                        <Badge variant="outline" className="text-amber-600 dark:text-amber-400">
                          breakpoint
                        </Badge>
                      )}
                      {data.run.error?.startsWith('cost_exceeded:') && (
                        <Badge variant="outline" className="text-amber-700 dark:text-amber-300 border-amber-700 dark:border-amber-300">
                          cost cap
                        </Badge>
                      )}
                    </div>
                  </div>
                  <div className="flex items-center gap-2">
                    {/* Force-cancel: only shown for non-terminal runs.
                        Manual safety valve for runs stuck due to worker
                        crash, abandoned approval, etc. Stage 2 = reaper
                        worker auto-fails based on heartbeat staleness. */}
                    {(data.run.status === 'queued' ||
                      data.run.status === 'running' ||
                      data.run.status === 'pending_approval' ||
                      data.run.status === 'paused') && (
                      <Button
                        variant="destructive"
                        onClick={handleCancel}
                        disabled={cancelling}
                      >
                        {cancelling ? 'Cancelling…' : 'Force Cancel'}
                      </Button>
                    )}
                    <Button onClick={handleReplay} disabled={replaying}>
                      <Play className="h-4 w-4 mr-1" />
                      {replaying ? 'Replaying…' : 'Replay'}
                    </Button>
                  </div>
                </div>

                <div className="mt-4 grid grid-cols-2 md:grid-cols-5 gap-4 text-sm">
                  <Stat label="Queued" value={new Date(data.run.queued_at).toLocaleString()} />
                  <Stat
                    label="Started"
                    value={data.run.started_at ? new Date(data.run.started_at).toLocaleString() : '—'}
                  />
                  <Stat
                    label="Finished"
                    value={data.run.finished_at ? new Date(data.run.finished_at).toLocaleString() : '—'}
                  />
                  <Stat
                    label="Duration"
                    value={formatDuration(data.run.started_at, data.run.finished_at)}
                  />
                  <Stat
                    label="Tokens / Cost"
                    value={`${(data.run.usage?.total_tokens ?? 0).toLocaleString()} · ${formatCost(data.run.usage?.cost_usd)}`}
                  />
                </div>

                {data.run.error && (
                  <div className="mt-4 text-sm text-destructive bg-destructive/10 rounded p-2">
                    {data.run.error}
                  </div>
                )}

                {/* Out-of-band approval gate. Two kinds:
                    - tool_call: agent's per-tool gate (existing).
                    - node:      pre-exec gate on a non-agent node;
                                 BFS halts here, reject = error edge. */}
                {data.run.status === 'pending_approval' && data.run.pending_approval && (
                  <div className="mt-4 border border-amber-500/40 bg-amber-500/5 rounded p-3 space-y-2">
                    <div className="flex items-center gap-2">
                      <Badge variant="outline" className="text-amber-500 border-amber-500/60">
                        approval needed
                      </Badge>
                      {data.run.pending_approval.kind === 'node' ? (
                        <span className="text-sm font-medium">
                          Node: <code className="font-mono">{data.run.pending_approval.node_name}</code>{' '}
                          <span className="text-xs text-muted-foreground">({data.run.pending_approval.node_type})</span>
                        </span>
                      ) : (
                        <span className="text-sm font-medium">
                          Tool call: <code className="font-mono">{data.run.pending_approval.tool_name}</code>
                        </span>
                      )}
                    </div>
                    <p className="text-xs text-muted-foreground">
                      {data.run.pending_approval.kind === 'node'
                        ? `Pre-exec gate · `
                        : `Agent requested at iter ${data.run.pending_approval.iter} · `}
                      {new Date(data.run.pending_approval.requested_at).toLocaleString()}
                    </p>
                    {data.run.pending_approval.dispatch_error && (
                      <div className="rounded border border-amber-300 bg-amber-50 dark:bg-amber-950/30 dark:border-amber-700 px-2 py-1.5 text-[11px] leading-snug text-amber-900 dark:text-amber-100">
                        <span className="font-semibold">Channel delivery failed.</span>{' '}
                        Resolve the gate below — no email or Slack message arrived.
                        <p className="mt-0.5 font-mono text-[10px] opacity-80 break-words">{data.run.pending_approval.dispatch_error}</p>
                      </div>
                    )}
                    {data.run.pending_approval.kind === 'node'
                      ? data.run.pending_approval.node_input !== undefined && (
                          <div>
                            <p className="text-[11px] font-semibold uppercase text-muted-foreground/80 mb-1">Node input</p>
                            <PrettyJSON value={data.run.pending_approval.node_input} />
                          </div>
                        )
                      : data.run.pending_approval.tool_args !== undefined && (
                          <div>
                            <p className="text-[11px] font-semibold uppercase text-muted-foreground/80 mb-1">Args</p>
                            <PrettyJSON value={data.run.pending_approval.tool_args} />
                          </div>
                        )}
                    <div className="flex items-center gap-2 pt-1">
                      <Button
                        size="sm"
                        className="bg-green-700 hover:bg-green-600 text-white"
                        disabled={submittingApproval}
                        onClick={() => submitApproval(true)}
                      >
                        Approve
                      </Button>
                      <Button
                        size="sm"
                        variant="destructive"
                        disabled={submittingApproval}
                        onClick={() => submitApproval(false)}
                      >
                        Reject
                      </Button>
                    </div>
                  </div>
                )}
              </div>

              {data.run.trigger_input !== undefined && data.run.trigger_input !== null && (
                <div className="border rounded-md bg-card p-4 mb-6">
                  <CollapsibleSection title="Trigger input">
                    <PrettyJSON value={data.run.trigger_input} />
                  </CollapsibleSection>
                </div>
              )}

              {data.run.params && Object.keys(data.run.params).length > 0 && (
                <div className="border rounded-md bg-card p-4 mb-6">
                  <CollapsibleSection title="Params">
                    <PrettyJSON value={data.run.params} />
                  </CollapsibleSection>
                </div>
              )}

              {data.run.steps && data.run.steps.length > 0 && (
                <div className="border rounded-md bg-card p-4 mb-6">
                  <h2 className="text-sm font-semibold mb-3">Steps</h2>
                  <div className="space-y-3">
                    {data.run.steps.map((s, i) => (
                      <div key={`${s.node_id}-${i}`} className="border rounded p-3 bg-muted/20">
                        <div className="flex items-center justify-between">
                          <div className="text-sm font-medium">
                            {nodeLabel(data.workflow, s.node_id, s.node_type)}
                          </div>
                          {s.error && s.continued && (
                            <Badge className="bg-amber-500/15 text-amber-600 dark:text-amber-400 border-amber-500/40 hover:bg-amber-500/20">
                              continued
                            </Badge>
                          )}
                          {s.error && !s.continued && <Badge variant="destructive">error</Badge>}
                        </div>
                        {s.error && (
                          <div className={`mt-2 text-sm ${s.continued ? 'text-amber-600 dark:text-amber-400' : 'text-destructive'}`}>
                            {s.error}
                          </div>
                        )}
                        {s.output !== undefined && (
                          <div className="mt-2">
                            <CollapsibleSection title="Output">
                              <PrettyJSON value={s.output} />
                            </CollapsibleSection>
                          </div>
                        )}
                      </div>
                    ))}
                  </div>
                </div>
              )}

              {data.run.agent_traces && Object.keys(data.run.agent_traces).length > 0 && (
                <div className="border rounded-md bg-card p-4 mb-6">
                  <h2 className="text-sm font-semibold mb-3">Agent traces</h2>
                  <div className="space-y-4">
                    {Object.entries(data.run.agent_traces).map(([nodeId, events]) => (
                      <AgentTraceBlock
                        key={nodeId}
                        title={nodeLabel(data.workflow, nodeId, 'ai_agent')}
                        events={events}
                        runTerminal={
                          data.run.status === 'success' ||
                          data.run.status === 'error' ||
                          data.run.status === 'cancelled'
                        }
                      />
                    ))}
                  </div>
                </div>
              )}
            </>
          )}
        </div>
      </main>
  )
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <div className="text-xs text-muted-foreground uppercase tracking-wide">{label}</div>
      <div className="font-mono text-sm mt-0.5">{value}</div>
    </div>
  )
}

// AgentTraceBlock groups TraceEvents by iter and renders a compact
// per-iteration timeline matching the live AgentTimelinePanel +
// AgentCostBadge:
//
//   AI Agent (ai_agent)                   1318 in / 62 out · $0.0031
//     ▾ iter 0 · 84 in / 35 out · $0.0010 · 1 tool
//     ▾ iter 1 · 1234 in / 27 out · $0.0021
//
// Source of truth is the persisted llm_call.Usage on each TraceEvent
// (one llm_call per iter); tool_call events are counted for the badge.
// Tag each trace event with an `attempt` index so re-runs after worker
// death (where the same agent_traces array gets concatenated across
// attempts) can be matched correctly: a tool_call only resolves when
// its tool_id appears as a tool_result WITHIN THE SAME ATTEMPT.
// Without this, gemma's deterministic tool_ids ("format_weather_0")
// from a killed attempt steal the resolution from a successful next
// attempt, hiding the interrupted state.
//
// Attempt boundary = iter_start for an iter index we've already seen
// in the current attempt. (Models can only re-enter iter 0 by
// restarting the agent loop from scratch.)
type EventWithAttempt = TraceEvent & { attempt: number }
function tagAttempts(events: TraceEvent[]): EventWithAttempt[] {
  const out: EventWithAttempt[] = []
  let attempt = 0
  let seen = new Set<number>()
  for (const ev of events) {
    if (ev.type === 'iter_start') {
      const iter = ev.iter ?? 0
      if (seen.has(iter)) {
        attempt++
        seen = new Set<number>()
      }
      seen.add(iter)
    }
    out.push({ ...ev, attempt })
  }
  return out
}

function AgentTraceBlock({
  title,
  events,
  runTerminal,
}: {
  title: string
  events: TraceEvent[]
  runTerminal: boolean
}) {
  const tagged = tagAttempts(events)
  const attemptCount = tagged.length === 0 ? 0 : tagged[tagged.length - 1]!.attempt + 1
  // Group by attempt → iter. Render each attempt as its own subsection
  // when there's more than one (re-run after worker death); single-
  // attempt runs render flat the same way they always have.
  const byAttempt: Record<number, Record<number, EventWithAttempt[]>> = {}
  for (const ev of tagged) {
    const a = ev.attempt
    const i = ev.iter ?? 0
    if (!byAttempt[a]) byAttempt[a] = {}
    if (!byAttempt[a][i]) byAttempt[a][i] = []
    byAttempt[a][i].push(ev)
  }

  // for_each body: the same agent node re-runs once per loop element,
  // so its agent_traces are concatenated and each iteration restarts
  // the ReAct loop at iter 0. tagAttempts() would read every iter-0
  // re-entry as a NEW attempt and label all but the last "abandoned
  // (worker died)" — wrong, they completed fine. When the backend
  // stamped loop_iter (TraceEvent.LoopIter), group by THAT instead and
  // render neutral "loop iteration N" sections. Non-looped runs
  // (loop_iter absent) keep the attempt/abandoned path unchanged.
  const loopIters = Array.from(
    new Set(tagged.map((e) => e.loop_iter ?? 0).filter((n) => n > 0)),
  ).sort((x, y) => x - y)
  const isLoop = loopIters.length > 0
  const byLoop: Record<number, Record<number, EventWithAttempt[]>> = {}
  if (isLoop) {
    for (const ev of tagged) {
      const L = ev.loop_iter ?? 0
      const i = ev.iter ?? 0
      if (!byLoop[L]) byLoop[L] = {}
      if (!byLoop[L][i]) byLoop[L][i] = []
      byLoop[L][i].push(ev)
    }
  }

  // Agent total — same numbers as AgentCostBadge on the workflow canvas.
  let totalIn = 0
  let totalOut = 0
  let totalCost = 0
  // Track provider/model usage across iters. Most agents use one model
  // for the whole run, but model_override mid-run is theoretically
  // possible (and OpenAI auto-bumps dated revisions), so collect a Set.
  const models = new Set<string>()
  let anyProvider: string | undefined
  for (const ev of events) {
    if (ev.type !== 'llm_call') continue
    if (ev.usage) {
      totalIn += ev.usage.input_tokens ?? 0
      totalOut += ev.usage.output_tokens ?? 0
      totalCost += ev.usage.cost_usd ?? 0
    }
    if (ev.model) models.add(ev.model)
    if (ev.provider && !anyProvider) anyProvider = ev.provider
  }
  const modelLabel = models.size === 1
    ? Array.from(models)[0]
    : models.size > 1
    ? `${models.size} models`
    : undefined

  return (
    <div className="border rounded p-3 bg-muted/20">
      <div className="flex items-center justify-between mb-2">
        <div className="flex items-center gap-2">
          <div className="text-sm font-medium">{title}</div>
          {modelLabel && (
            <Badge variant="outline" className="text-[10px]" title={anyProvider ? `${anyProvider} provider` : undefined}>
              {anyProvider ? `${anyProvider}/` : ''}{modelLabel}
            </Badge>
          )}
        </div>
        {(totalIn > 0 || totalOut > 0 || totalCost > 0) && (
          <span className="text-xs text-muted-foreground tabular-nums">
            {totalIn.toLocaleString()} in / {totalOut.toLocaleString()} out · ${totalCost.toFixed(4)}
          </span>
        )}
      </div>
      <div className="space-y-3">
        {isLoop
          ? loopIters.map((L, idx) => {
              const iters = byLoop[L] ?? {}
              const iterKeys = Object.keys(iters)
                .map(Number)
                .sort((x, y) => x - y)
              const isLast = idx === loopIters.length - 1
              return (
                <div key={`loop-${L}`}>
                  <div className="flex items-center gap-2 mb-1 text-[11px] text-muted-foreground">
                    <Badge
                      variant="outline"
                      className="text-[10px] border-violet-500/40 text-violet-300"
                    >
                      loop iteration {L} of {loopIters.length}
                    </Badge>
                  </div>
                  <div className="space-y-2">
                    {iterKeys.map((i) => (
                      <IterSection
                        key={i}
                        iter={i}
                        events={iters[i]!}
                        defaultOpen={isLast && i === iterKeys[0]}
                        // for_each iter is always live-correct (no
                        // worker-death attempt split). Only label a
                        // dangling tool_call as interrupted once the
                        // whole run is terminal — otherwise it's just
                        // pending and the tool_result is still on its
                        // way over the stream.
                        canInterrupt={runTerminal}
                      />
                    ))}
                  </div>
                </div>
              )
            })
          : Array.from({ length: attemptCount }, (_, a) => {
              const iters = byAttempt[a] ?? {}
              const iterKeys = Object.keys(iters)
                .map(Number)
                .sort((x, y) => x - y)
              const isAbandoned = a < attemptCount - 1
              return (
                <div key={a} className={isAbandoned ? 'opacity-70' : ''}>
                  {attemptCount > 1 && (
                    <div className="flex items-center gap-2 mb-1 text-[11px] text-muted-foreground">
                      <Badge variant="outline" className={`text-[10px] ${isAbandoned ? 'border-amber-500/40 text-amber-400' : ''}`}>
                        attempt {a + 1}{isAbandoned ? ' · abandoned (worker died)' : ''}
                      </Badge>
                    </div>
                  )}
                  <div className="space-y-2">
                    {iterKeys.map((i) => (
                      <IterSection
                        key={i}
                        iter={i}
                        events={iters[i]!}
                        defaultOpen={a === attemptCount - 1 && i === iterKeys[0]}
                        // Two paths can produce a tool_call w/o a
                        // tool_result:
                        //   (a) prior attempt died mid-tool (worker
                        //       lease lapsed) — labelling is correct
                        //       AND useful, even while a later attempt
                        //       is still running;
                        //   (b) current live attempt — the result row
                        //       just hasn't streamed in yet.
                        // Gate the badge to (a) OR (terminal run) so
                        // (b) stops false-flagging mid-flight.
                        canInterrupt={isAbandoned || runTerminal}
                      />
                    ))}
                  </div>
                </div>
              )
            })}
      </div>
    </div>
  )
}

// IterSection renders one iteration of the ReAct loop with a header that
// summarizes its llm_call usage + tool-call count, mirroring the live
// AgentTimelinePanel row.
function IterSection({
  iter,
  events,
  defaultOpen,
  canInterrupt,
}: {
  iter: number
  events: TraceEvent[]
  defaultOpen?: boolean
  // True when a tool_call without a matching tool_result should be
  // labelled "interrupted — no result". False during live streaming
  // on the current attempt, where the result row is just on its way.
  canInterrupt: boolean
}) {
  const [open, setOpen] = useState(!!defaultOpen)
  let inTok = 0
  let outTok = 0
  let cost = 0
  let toolCount = 0
  let provider: string | undefined
  let model: string | undefined
  for (const ev of events) {
    if (ev.type === 'llm_call' && ev.usage) {
      inTok += ev.usage.input_tokens ?? 0
      outTok += ev.usage.output_tokens ?? 0
      cost += ev.usage.cost_usd ?? 0
    }
    if (ev.type === 'llm_call') {
      if (ev.provider && !provider) provider = ev.provider
      if (ev.model && !model) model = ev.model
    }
    if (ev.type === 'tool_call') toolCount++
  }

  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <CollapsibleTrigger className="w-full flex items-center gap-2 text-xs hover:text-primary transition-colors text-left">
        {open ? <ChevronDown className="h-3 w-3 shrink-0" /> : <ChevronRight className="h-3 w-3 shrink-0" />}
        <span className="font-medium text-foreground">iter {iter}</span>
        {model && (
          <span className="text-muted-foreground" title={provider ? `${provider} provider` : undefined}>
            · {provider ? `${provider}/` : ''}{model}
          </span>
        )}
        {(inTok > 0 || outTok > 0) && (
          <span className="text-muted-foreground tabular-nums">
            · {inTok.toLocaleString()} in / {outTok.toLocaleString()} out
          </span>
        )}
        {cost > 0 && (
          <span className="text-muted-foreground tabular-nums">· ${cost.toFixed(4)}</span>
        )}
        {toolCount > 0 && (
          <span className="text-muted-foreground">
            · {toolCount} tool{toolCount === 1 ? '' : 's'}
          </span>
        )}
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="space-y-2 ml-5 mt-2">
          {(() => {
            // Mark tool_call events that never resolved into a tool_result
            // as "interrupted" — happens when the worker died mid-tool
            // (lease auto-expires, fresh worker re-runs the agent from
            // iter 0, both attempts share the same agent_traces array).
            // No data deleted; the orphaned attempt is just labelled.
            const resolvedToolIDs = new Set(
              events
                .filter((e) => e.type === 'tool_result' && e.tool_id)
                .map((e) => e.tool_id!),
            )
            return events.map((ev, idx) => (
              <TraceEventRow
                key={idx}
                ev={ev}
                interrupted={
                  canInterrupt &&
                  ev.type === 'tool_call' &&
                  !!ev.tool_id &&
                  !resolvedToolIDs.has(ev.tool_id)
                }
              />
            ))
          })()}
        </div>
      </CollapsibleContent>
    </Collapsible>
  )
}

function TraceEventRow({ ev, interrupted }: { ev: TraceEvent; interrupted?: boolean }) {
  return (
    <div className="text-xs border-l-2 border-muted pl-2">
      <div className="flex items-center gap-2">
        <Badge variant="outline" className="text-[10px]">
          {ev.type}
        </Badge>
        {ev.tool_name && <span className="font-mono">{ev.tool_name}</span>}
        {ev.is_error && <Badge variant="destructive">error</Badge>}
        {interrupted && (
          <Badge variant="outline" className="text-[10px] border-amber-500/40 text-amber-400">
            interrupted — no result
          </Badge>
        )}
        {ev.usage && (
          <span className="text-muted-foreground tabular-nums">
            {(ev.usage.total_tokens ?? 0).toLocaleString()} tok
          </span>
        )}
      </div>
      {ev.text && (
        <div className="mt-1 text-muted-foreground whitespace-pre-wrap break-words">{ev.text}</div>
      )}
      {/* Empty llm_call text is normal for smaller / local models (Ollama
          Gemma, etc.) that go straight to tool_calls without narrating.
          Render a placeholder so it's clear nothing was missed. */}
      {!ev.text && ev.type === 'llm_call' && (
        <div className="mt-1 text-muted-foreground/60 italic">(no narration — model went straight to tool call)</div>
      )}
      {ev.tool_args !== undefined && (
        <div className="mt-1">
          <PrettyJSON value={ev.tool_args} />
        </div>
      )}
      {ev.result !== undefined && (
        <div className="mt-1">
          <PrettyJSON value={ev.result} />
        </div>
      )}
    </div>
  )
}

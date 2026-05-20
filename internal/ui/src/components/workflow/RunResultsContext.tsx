import { createContext, useContext, useEffect, useRef, useState } from 'react'

export interface StepResult {
  node_id: string
  node_type: string
  output?: unknown
  error?: string
  // Live-run lifecycle status. Optional so post-run snapshots (which
  // never set it) keep rendering as "success/error" via the error field.
  // Streaming runs set this to 'running' while in flight and 'done'/'error'
  // when the corresponding step_done event arrives.
  status?: 'running' | 'done' | 'error' | 'paused' | 'cancelled'
  // Continued is true when the node errored but its `on_error` policy
  // is "continue" — the run-level aggregate skipped this step's
  // failure during status promotion. The step still has `error`
  // populated; the UI surfaces the combination distinctly (amber
  // "completed with error") so the suppressed failure stays visible.
  continued?: boolean
  // for_each per-iteration extras. `pending` = this iteration was
  // reset to idle (loop advanced) but the node hasn't run it yet.
  // `loopTotal` = M, the uniform "iter K/M" denominator across the
  // whole body subgraph (a node skipped in some iterations would
  // otherwise show a wrong per-node count as the denominator).
  pending?: boolean
  loopTotal?: number
}

export type RunResults = Record<string, StepResult[]>

export const RunResultsContext = createContext<RunResults | null>(null)

// RunStatusContext signals whether a workflow run is in flight. Read by
// NodeDebugPanel to differentiate "not executed yet" from "haven't reached
// this node yet during a live run". Keeping it separate from RunResults
// lets per-node memoisation off the results map stay stable across status
// changes (every node panel doesn't re-render on every status flip).
export const RunStatusContext = createContext<{ running: boolean }>({ running: false })

// AgentIter mirrors the structure produced by useWorkflowRunStream — kept
// duplicated (rather than re-exported from the hook) so this context can
// be consumed without dragging in WS-specific code.
export interface AgentIterSummary {
  iter: number
  loopIter?: number // for_each iteration this belongs to (0/undef = not looped)
  llm?: {
    text?: string
    usage?: { input_tokens?: number; output_tokens?: number; total_tokens?: number; cost_usd?: number }
    provider?: string
    model?: string
  }
  toolCalls: { toolName: string; toolId: string; args?: unknown; result?: string; isError?: boolean; pendingApproval?: boolean }[]
}

// AgentRunContext exposes per-agent-node iter timelines so the AI Agent
// node UI can render a live ReAct breakdown (LLM call + tool calls per
// iteration) without a parallel WS connection per node. Filled in by the
// page-level provider that owns the WS hook; stays nil when no run is in
// flight or for non-agent nodes.
export const AgentRunContext = createContext<Record<string, AgentIterSummary[]> | null>(null)

// ToolApprovalContext exposes the approveTool function from the live WS
// hook so the AgentTimelinePanel can render Approve/Reject buttons on a
// paused tool call without each panel maintaining its own WS connection.
// Null when no live run is connected (post-run snapshots, replay view).
export const ToolApprovalContext = createContext<((toolId: string, approved: boolean, reason?: string) => void) | null>(null)

export interface DebugState {
  debugMode: boolean
  // Multi-breakpoint set. Multiple nodes can carry a breakpoint and the
  // executor halts at each one in turn (Continue advances to the next).
  breakpointIds: Set<string>
  toggleBreakpoint(nodeId: string): void
}

export const DebugContext = createContext<DebugState>({
  debugMode: false,
  breakpointIds: new Set(),
  toggleBreakpoint: () => {},
})

export function BreakpointMarker({ id }: { id: string }) {
  const { debugMode, breakpointIds, toggleBreakpoint } = useContext(DebugContext)
  const results = useContext(RunResultsContext)
  if (!debugMode) return null

  const isSet = breakpointIds.has(id)

  // Active breakpoint = THIS node is currently paused at a pre-exec
  // breakpoint pause. Distinct visual so the user can tell at a
  // glance which of N set breakpoints the run is actually halted on.
  // Reads the latest StepResult.status for the node; 'paused' = the
  // step_pending event landed and the worker is waiting on Continue.
  const steps = results?.[id]
  const lastStatus = steps && steps.length > 0 ? steps[steps.length - 1]!.status : undefined
  const isActive = isSet && lastStatus === 'paused'

  const borderColor = isActive ? '#facc15' : isSet ? '#ef4444' : '#666'
  const backgroundColor = isActive ? '#facc15' : isSet ? '#ef4444' : 'transparent'

  return (
    <button
      className={`nodrag absolute -left-3.5 -top-3.5 z-10 h-5 w-5 rounded-full border-2 flex items-center justify-center transition-colors hover:scale-110 ${
        isActive ? 'animate-pulse ring-2 ring-yellow-400/60 ring-offset-1 ring-offset-background' : ''
      }`}
      style={{
        borderColor,
        backgroundColor,
      }}
      onClick={(e) => {
        e.stopPropagation()
        toggleBreakpoint(id)
      }}
      title={
        isActive
          ? 'Paused here — click Continue to advance'
          : isSet
          ? 'Remove breakpoint'
          : 'Set breakpoint'
      }
    >
      {isActive ? (
        // Pause glyph: two thin vertical bars. Clear "halted here"
        // signal in addition to the colour change.
        <span className="flex gap-[1px]">
          <span className="block h-1.5 w-[2px] bg-zinc-900" />
          <span className="block h-1.5 w-[2px] bg-zinc-900" />
        </span>
      ) : isSet ? (
        <span className="block h-1.5 w-1.5 rounded-full bg-white" />
      ) : null}
    </button>
  )
}

// ApprovalMarker is the per-node "require approval" toggle. Sits on
// the top-RIGHT corner of each node so it doesn't clash with
// BreakpointMarker (top-left). Toggling sets / clears
// `node.data.require_approval`. The executor's BFS halts at any node
// with the flag set + persists `pending_approval` state on the run
// record so /runs/:id can present an Approve/Reject panel.
//
// Rendered on every node EXCEPT ai_agent — that node has its own
// per-tool gate inside its config panel; gating both layers would
// double-prompt for the same intent.
export function ApprovalMarker({
  id,
  enabled,
  onToggle,
}: {
  id: string
  enabled: boolean
  onToggle(id: string, next: boolean): void
}) {
  return (
    <button
      className="nodrag absolute -right-3.5 -top-3.5 z-10 h-5 w-5 rounded-full border-2 flex items-center justify-center transition-colors hover:scale-110"
      style={{
        borderColor: enabled ? '#f59e0b' : '#666',
        backgroundColor: enabled ? '#f59e0b' : 'transparent',
      }}
      onClick={(e) => {
        e.stopPropagation()
        onToggle(id, !enabled)
      }}
      title={enabled ? 'Remove pre-exec approval gate' : 'Require approval before running'}
    >
      {enabled && <span className="block h-1.5 w-1.5 rounded-full bg-white" />}
    </button>
  )
}

function formatOutput(v: unknown): string {
  if (typeof v === 'string') {
    return v.length > 2000 ? v.slice(0, 2000) + '\n…(truncated)' : v
  }
  const s = JSON.stringify(v, null, 2) ?? 'null'
  return s.length > 2000 ? s.slice(0, 2000) + '\n…(truncated)' : s
}

function fullOutput(v: unknown): string {
  if (typeof v === 'string') return v
  return JSON.stringify(v, null, 2) ?? 'null'
}

export function NodeDebugPanel({ id }: { id: string }) {
  const results = useContext(RunResultsContext)
  const { running } = useContext(RunStatusContext)
  const [expanded, setExpanded] = useState(false)
  const [iterIdx, setIterIdx] = useState(0)
  // While a for_each loops, auto-follow the newest iteration so the
  // panel tracks live progress — unless the user manually paged, in
  // which case respect their selection.
  const userPaged = useRef(false)
  const stepCount = results?.[id]?.length ?? 0
  useEffect(() => {
    if (running && !userPaged.current && stepCount > 0) {
      setIterIdx(stepCount - 1)
    }
    if (!running) userPaged.current = false
  }, [stepCount, running])

  if (results === null) return null

  const steps = results[id]

  if (!steps) {
    // During a live run, nodes the BFS hasn't reached yet show "idle"
    // (amber dot, no pulse) so the user sees the workflow waiting on
    // them rather than thinking they're skipped. After the run ends
    // and they still have no result, that's the "not executed" path
    // (canvas-author dropped the edge, etc.).
    if (running) {
      return (
        <div className="nodrag flex items-center gap-2 px-3 py-2 border-t border-border/40">
          <span className="inline-block h-2.5 w-2.5 rounded-full bg-amber-500 shrink-0" />
          <span className="text-xs text-muted-foreground flex-1">idle</span>
        </div>
      )
    }
    return (
      <div className="nodrag px-3 py-1.5 border-t border-border/40 text-[10px] text-muted-foreground/50 italic">
        not executed
      </div>
    )
  }

  const total = steps.length
  const idx = Math.min(iterIdx, total - 1)
  const step = steps[idx]!
  const hasError = !!step.error || step.status === 'error'
  // `on_error: continue` step. Run-level aggregate skipped this
  // step's failure during status promotion, so the run can land
  // green even though this step has an Error. Distinct amber badge
  // + "continued" label keeps the suppressed fault visible.
  const isContinued = hasError && !!step.continued
  // Rejection precedence rule: a continued-policy gate rejection is
  // still surfaced as "continued" (the more important fact for the
  // operator is that the run kept going).
  const isRejected = hasError && !isContinued && (step.error ?? '').startsWith('rejected by user')
  const isRunning = step.status === 'running'
  const isPaused = step.status === 'paused'
  const isCancelled = step.status === 'cancelled'
  // pending = for_each reset this node to idle for the current
  // iteration; it hasn't run THIS iteration yet.
  const isPending = !hasError && !isRunning && !!step.pending
  // Denominator is the loop total (M) — uniform across the body
  // subgraph — falling back to the entry count for non-looped nodes.
  const denom = step.loopTotal && step.loopTotal > 0 ? step.loopTotal : total
  const isMulti = denom > 1 || total > 1

  // Live status indicator — continued > rejected > error > cancelled > paused > running > success.
  // Continued + rejected both use amber; the label disambiguates.
  // (Continued = "node blew up but on_error=continue let the run keep
  // going", rejected = "user vetoed it via approval gate".)
  const dotClass = isContinued
    ? 'bg-amber-500'
    : isRejected
    ? 'bg-amber-500'
    : hasError
    ? 'bg-red-500'
    : isCancelled
    ? 'bg-zinc-500'
    : isPaused
    ? 'bg-yellow-500'
    : isPending
    ? 'bg-amber-500'
    : isRunning
    ? 'bg-blue-500 animate-pulse'
    : 'bg-green-500'
  const label = isContinued
    ? 'continued'
    : isRejected
    ? 'rejected'
    : hasError
    ? 'error'
    : isCancelled
    ? 'cancelled'
    : isPaused
    ? 'paused'
    : isPending
    ? 'idle'
    : isRunning
    ? 'running'
    : 'success'

  return (
    <div className="nodrag border-t border-border/40">
      <button
        className="nodrag w-full flex items-center gap-2 px-3 py-2 text-left hover:bg-muted/30 transition-colors"
        onClick={() => setExpanded((v) => !v)}
      >
        <span
          className={`inline-block h-2.5 w-2.5 rounded-full shrink-0 ${dotClass}`}
        />
        <span className="text-xs text-muted-foreground flex-1">
          {label}
          {isMulti && ` · iter ${idx + 1}/${denom}`}
        </span>
        {isMulti && (
          <span
            className="flex gap-0.5"
            onClick={(e) => e.stopPropagation()}
          >
            <button
              className="nodrag text-sm px-1 rounded hover:bg-muted/50 disabled:opacity-30"
              disabled={idx === 0}
              onClick={() => { userPaged.current = true; setIterIdx((i) => Math.max(0, i - 1)) }}
            >
              ‹
            </button>
            <button
              className="nodrag text-sm px-1 rounded hover:bg-muted/50 disabled:opacity-30"
              disabled={idx === total - 1}
              onClick={() => { userPaged.current = true; setIterIdx((i) => Math.min(total - 1, i + 1)) }}
            >
              ›
            </button>
          </span>
        )}
        <span
          className="text-base text-muted-foreground/50 hover:text-foreground px-0.5 leading-none cursor-pointer"
          title="Copy output"
          onClick={(e) => {
            e.stopPropagation()
            const text = hasError ? (step.error ?? '') : fullOutput(step.output)
            navigator.clipboard.writeText(text)
          }}
        >
          ⎘
        </span>
        <span className="text-sm text-muted-foreground/50">{expanded ? '▴' : '▾'}</span>
      </button>
      {expanded && (
        <pre className="nodrag nowheel px-3 pb-2 text-[10px] leading-4 max-h-[160px] overflow-y-auto overflow-x-hidden text-muted-foreground whitespace-pre-wrap break-all w-0 min-w-full">
          {hasError ? step.error : formatOutput(step.output)}
        </pre>
      )}
    </div>
  )
}

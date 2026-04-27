import { createContext, useContext, useState } from 'react'

export interface StepResult {
  node_id: string
  node_type: string
  output?: unknown
  error?: string
}

export type RunResults = Record<string, StepResult[]>

export const RunResultsContext = createContext<RunResults | null>(null)

export interface DebugState {
  debugMode: boolean
  breakpointId: string | null
  toggleBreakpoint(nodeId: string): void
}

export const DebugContext = createContext<DebugState>({
  debugMode: false,
  breakpointId: null,
  toggleBreakpoint: () => {},
})

export function BreakpointMarker({ id }: { id: string }) {
  const { debugMode, breakpointId, toggleBreakpoint } = useContext(DebugContext)
  if (!debugMode) return null

  const isSet = breakpointId === id

  return (
    <button
      className="nodrag absolute -left-3.5 -top-3.5 z-10 h-5 w-5 rounded-full border-2 flex items-center justify-center transition-colors hover:scale-110"
      style={{
        borderColor: isSet ? '#ef4444' : '#666',
        backgroundColor: isSet ? '#ef4444' : 'transparent',
      }}
      onClick={(e) => {
        e.stopPropagation()
        toggleBreakpoint(id)
      }}
      title={isSet ? 'Remove breakpoint' : 'Set breakpoint'}
    >
      {isSet && <span className="block h-1.5 w-1.5 rounded-full bg-white" />}
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
  const [expanded, setExpanded] = useState(false)
  const [iterIdx, setIterIdx] = useState(0)

  if (results === null) return null

  const steps = results[id]

  if (!steps) {
    return (
      <div className="nodrag px-3 py-1.5 border-t border-border/40 text-[10px] text-muted-foreground/50 italic">
        not executed
      </div>
    )
  }

  const total = steps.length
  const idx = Math.min(iterIdx, total - 1)
  const step = steps[idx]
  const hasError = !!step.error
  const isMulti = total > 1

  return (
    <div className="nodrag border-t border-border/40">
      <button
        className="nodrag w-full flex items-center gap-2 px-3 py-2 text-left hover:bg-muted/30 transition-colors"
        onClick={() => setExpanded((v) => !v)}
      >
        <span
          className={`inline-block h-2.5 w-2.5 rounded-full shrink-0 ${hasError ? 'bg-red-500' : 'bg-green-500'}`}
        />
        <span className="text-xs text-muted-foreground flex-1">
          {hasError ? 'error' : 'success'}
          {isMulti && ` · iter ${idx + 1}/${total}`}
        </span>
        {isMulti && (
          <span
            className="flex gap-0.5"
            onClick={(e) => e.stopPropagation()}
          >
            <button
              className="nodrag text-sm px-1 rounded hover:bg-muted/50 disabled:opacity-30"
              disabled={idx === 0}
              onClick={() => setIterIdx((i) => Math.max(0, i - 1))}
            >
              ‹
            </button>
            <button
              className="nodrag text-sm px-1 rounded hover:bg-muted/50 disabled:opacity-30"
              disabled={idx === total - 1}
              onClick={() => setIterIdx((i) => Math.min(total - 1, i + 1))}
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

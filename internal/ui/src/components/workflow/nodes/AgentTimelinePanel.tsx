import { useContext, useState } from 'react'
import { ChevronDown, ChevronRight } from 'lucide-react'
import { AgentRunContext } from '../RunResultsContext'

/**
 * AgentTimelinePanel renders the per-iteration ReAct breakdown for a single
 * AI Agent node during (and after) a streaming run. Sourced from
 * `AgentRunContext`, which the workflow page wires up from
 * `useWorkflowRunStream`.
 *
 * Layout per iter:
 *   ▾ iter 0 · 84 in / 35 out tokens · $0.0010
 *     llm: I'll fetch the weather ...
 *     ▸ get_weather({"city": "..."}) → {"...":"..."}
 *     ▸ format_weather({"city": "...", "raw": "..."}) → output: {...}
 *   ▾ iter 1 · 1234 in / 27 out tokens · $0.0021
 *     ...
 *
 * Stays empty when the node has no iters yet (non-running, non-agent
 * nodes never get entries because the agent loop is the only emitter).
 */
export function AgentTimelinePanel({ id }: { id: string }) {
  const ctx = useContext(AgentRunContext)
  const iters = ctx?.[id] ?? []
  const [expanded, setExpanded] = useState<Record<number, boolean>>({})

  if (iters.length === 0) return null

  return (
    <div className="nodrag border-t border-border/40 bg-muted/10">
      <div className="px-3 py-1.5 text-[10px] font-medium text-muted-foreground/80 uppercase tracking-wider">
        Agent timeline · {iters.length} iter{iters.length === 1 ? '' : 's'}
      </div>
      <div className="space-y-0.5 px-2 pb-2">
        {iters.map((iter) => {
          const open = expanded[iter.iter] ?? true
          const inTok = iter.llm?.usage?.input_tokens ?? 0
          const outTok = iter.llm?.usage?.output_tokens ?? 0
          const cost = iter.llm?.usage?.cost_usd ?? 0
          return (
            <div key={iter.iter} className="rounded border border-border/30">
              <button
                type="button"
                onClick={() =>
                  setExpanded((prev) => ({ ...prev, [iter.iter]: !open }))
                }
                className="nodrag flex w-full items-center gap-1.5 px-2 py-1 text-[10px] hover:bg-muted/40"
              >
                {open ? (
                  <ChevronDown className="h-3 w-3 shrink-0" />
                ) : (
                  <ChevronRight className="h-3 w-3 shrink-0" />
                )}
                <span className="font-medium text-foreground">iter {iter.iter}</span>
                {(inTok > 0 || outTok > 0) && (
                  <span className="text-muted-foreground">
                    · {inTok} in / {outTok} out
                  </span>
                )}
                {cost > 0 && (
                  <span className="text-muted-foreground">
                    · ${cost.toFixed(4)}
                  </span>
                )}
                <span className="text-muted-foreground ml-auto">
                  {iter.toolCalls.length > 0 && `${iter.toolCalls.length} tool${iter.toolCalls.length === 1 ? '' : 's'}`}
                </span>
              </button>
              {open && (
                <div className="px-2 pb-1.5 space-y-1 text-[10px]">
                  {iter.llm?.text && (
                    <div>
                      <p className="text-muted-foreground/70 italic">llm</p>
                      <p className="text-foreground whitespace-pre-wrap break-words">{iter.llm.text}</p>
                    </div>
                  )}
                  {iter.toolCalls.map((tc) => (
                    <div
                      key={tc.toolId}
                      className={`rounded border px-1.5 py-1 ${
                        tc.isError
                          ? 'border-red-500/30 bg-red-500/5'
                          : tc.result === undefined
                          ? 'border-blue-500/30 bg-blue-500/5'
                          : 'border-green-500/30 bg-green-500/5'
                      }`}
                    >
                      <p className="text-foreground font-mono">
                        <span className="text-muted-foreground">→</span> {tc.toolName}
                      </p>
                      {tc.args !== undefined && (
                        <p className="font-mono text-[9px] text-muted-foreground break-all">
                          args: {JSON.stringify(tc.args)}
                        </p>
                      )}
                      {tc.result === undefined ? (
                        <p className="text-[9px] italic text-blue-400">running…</p>
                      ) : (
                        <p className="font-mono text-[9px] text-muted-foreground break-all whitespace-pre-wrap">
                          {tc.result.length > 400
                            ? tc.result.slice(0, 400) + '…(truncated)'
                            : tc.result}
                        </p>
                      )}
                    </div>
                  ))}
                </div>
              )}
            </div>
          )
        })}
      </div>
    </div>
  )
}

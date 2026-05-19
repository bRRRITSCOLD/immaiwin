import { useContext, useState } from 'react'
import { ChevronDown, ChevronRight, Check, X } from 'lucide-react'
import { Button } from '~/components/ui/button'
import { AgentRunContext, ToolApprovalContext } from '../RunResultsContext'

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
  const approveTool = useContext(ToolApprovalContext)
  const iters = ctx?.[id] ?? []
  // Keyed by `${loopIter}:${iter}` so the same ReAct iter index across
  // different for_each loop iterations doesn't share collapse state.
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})

  if (iters.length === 0) return null

  // for_each body: the agent re-runs once per loop element, so iters
  // carry a loopIter and the same `iter` index repeats. Group by
  // loopIter and render "loop iteration N of M" sections to mirror the
  // run-detail page. Non-looped runs (loopIter absent) render flat as
  // before — one implicit group.
  const loopVals = Array.from(
    new Set(iters.map((i) => i.loopIter ?? 0).filter((n) => n > 0)),
  ).sort((a, b) => a - b)
  const isLoop = loopVals.length > 0
  const groups = isLoop
    ? loopVals.map((L) => ({ loop: L, items: iters.filter((i) => (i.loopIter ?? 0) === L) }))
    : [{ loop: 0, items: iters }]

  const renderIter = (iter: (typeof iters)[number]) => {
          const ekey = `${iter.loopIter ?? 0}:${iter.iter}`
          const open = expanded[ekey] ?? true
          const inTok = iter.llm?.usage?.input_tokens ?? 0
          const outTok = iter.llm?.usage?.output_tokens ?? 0
          const cost = iter.llm?.usage?.cost_usd ?? 0
          return (
            <div key={ekey} className="rounded border border-border/30">
              <button
                type="button"
                onClick={() =>
                  setExpanded((prev) => ({ ...prev, [ekey]: !open }))
                }
                className="nodrag flex w-full items-center gap-1.5 px-2 py-1 text-[10px] hover:bg-muted/40"
              >
                {open ? (
                  <ChevronDown className="h-3 w-3 shrink-0" />
                ) : (
                  <ChevronRight className="h-3 w-3 shrink-0" />
                )}
                <span className="font-medium text-foreground">iter {iter.iter}</span>
                {iter.llm?.model && (
                  <span className="text-muted-foreground" title={iter.llm.provider ? `${iter.llm.provider} provider` : undefined}>
                    · {iter.llm.provider ? `${iter.llm.provider}/` : ''}{iter.llm.model}
                  </span>
                )}
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
                  {iter.llm?.text ? (
                    <div>
                      <p className="text-muted-foreground/70 italic">llm</p>
                      <p className="text-foreground whitespace-pre-wrap break-words">{iter.llm.text}</p>
                    </div>
                  ) : iter.llm && iter.toolCalls.length > 0 ? (
                    <div>
                      <p className="text-muted-foreground/70 italic">llm</p>
                      <p className="text-muted-foreground/60 italic">(no narration — model went straight to tool call)</p>
                    </div>
                  ) : null}
                  {iter.toolCalls.map((tc) => (
                    <div
                      key={tc.toolId}
                      className={`rounded border px-1.5 py-1 ${
                        tc.pendingApproval
                          ? 'border-amber-500/40 bg-amber-500/5'
                          : tc.isError
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
                      {tc.pendingApproval ? (
                        <div className="mt-1 flex items-center gap-1">
                          <p className="text-[9px] italic text-amber-500 mr-1">awaiting approval…</p>
                          {approveTool && (
                            <>
                              <Button
                                type="button"
                                size="sm"
                                variant="default"
                                className="nodrag h-5 px-2 text-[10px] bg-green-700 hover:bg-green-600"
                                onClick={() => approveTool(tc.toolId, true)}
                              >
                                <Check className="h-3 w-3 mr-0.5" />
                                Approve
                              </Button>
                              <Button
                                type="button"
                                size="sm"
                                variant="destructive"
                                className="nodrag h-5 px-2 text-[10px]"
                                onClick={() => approveTool(tc.toolId, false)}
                              >
                                <X className="h-3 w-3 mr-0.5" />
                                Reject
                              </Button>
                            </>
                          )}
                        </div>
                      ) : tc.result === undefined ? (
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
  }

  return (
    <div className="nodrag border-t border-border/40 bg-muted/10">
      <div className="px-3 py-1.5 text-[10px] font-medium text-muted-foreground/80 uppercase tracking-wider">
        Agent timeline · {isLoop
          ? `${groups.length} loop iteration${groups.length === 1 ? '' : 's'}`
          : `${iters.length} iter${iters.length === 1 ? '' : 's'}`}
      </div>
      <div className="space-y-1.5 px-2 pb-2">
        {groups.map((g) => (
          <div key={g.loop} className="space-y-0.5">
            {isLoop && (
              <div className="px-1 pt-0.5">
                <span className="inline-block rounded border border-violet-500/40 px-1.5 py-0.5 text-[9px] font-medium text-violet-300">
                  loop iteration {g.loop} of {groups.length}
                </span>
              </div>
            )}
            {g.items.map(renderIter)}
          </div>
        ))}
      </div>
    </div>
  )
}

import { useReactFlow } from '@xyflow/react'
import { HelpCircle } from 'lucide-react'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '~/components/ui/select'
import { Tooltip, TooltipTrigger, TooltipContent } from '~/components/ui/tooltip'

// OnErrorPolicySelect renders the per-node `on_error` dropdown
// shared by every fallible node type (http_request, mongo_request,
// redis_request, notify, sandbox_script, ai_agent). Two values:
//
//   stop      — default; first node failure flips the run to error.
//   continue  — failure recorded on the StepResult but suppressed
//               for run-status promotion. Run lands `success` if
//               nothing else failed; the run-detail UI surfaces the
//               step distinctly so the suppressed failure is still
//               visible.
//
// The dropdown writes `node.data.on_error` straight through to the
// workflow doc; the executor reads the same key. Keeping the value
// space minimal (no `branch` keyword etc.) is intentional — error
// edges already route on the existing `sourceHandle: "error"`
// channel regardless of policy. Adding more values invites
// confusion without adding capability.
export function OnErrorPolicySelect({ nodeId, value, nodeType }: { nodeId: string; value: string; nodeType?: string }) {
  const { updateNodeData } = useReactFlow()
  const current = value === 'continue' ? 'continue' : 'stop'
  const isAgent = nodeType === 'ai_agent'
  const isForEach = nodeType === 'for_each'

  return (
    <div className="flex items-center justify-between gap-2 pt-1">
      <div className="flex items-center gap-1">
        <p className="text-[10px] text-muted-foreground">On error</p>
        <Tooltip>
          <TooltipTrigger asChild>
            <button
              type="button"
              className="nodrag text-muted-foreground hover:text-foreground"
              aria-label="On-error policy help"
            >
              <HelpCircle className="h-3 w-3" />
            </button>
          </TooltipTrigger>
          <TooltipContent side="top" className="max-w-[340px] text-[11px] leading-snug">
            <p className="font-medium mb-1">Per-node failure policy.</p>
            <p>
              <strong>Stop run</strong> (default): a failure in this node flips the run to <code>error</code>.
              Existing <code>error</code> edges still route to their targets.
            </p>
            <p className="mt-1">
              <strong>Continue past error</strong>: failure is recorded on the step but the run still lands <code>success</code> if nothing else failed.
              The step renders distinctly (amber) so the suppressed fault stays visible. <strong>BOTH</strong> the error edge AND the success edge fire — the success branch keeps rolling as if nothing happened, and the error edge becomes an optional sidecar (cleanup, alerting, etc).
            </p>
            {isForEach ? (
              <>
                <p className="mt-1 text-zinc-200">
                  <strong>Heads up for For Each nodes:</strong> this is a <em>loop-abort</em> policy, not a node fault. <strong>Stop run</strong> (default): the first item whose body chain ends with an unsuppressed error aborts the loop — remaining items are <strong>skipped</strong> — and the run lands <code>error</code>. <strong>Continue past error</strong>: never aborts; every item runs (best-effort fan-out). The run lands <code>error</code> at the end <em>only</em> if some item ended with an <strong>unsuppressed</strong> error.
                </p>
                <p className="mt-1 text-zinc-200">
                  A body node with its own <code>on_error: continue</code> <strong>suppresses</strong> its own fault — it never counts as unsuppressed, so it neither aborts the loop under <strong>Stop run</strong> nor flips the run under <strong>Continue past error</strong>. A loop whose only failures are suppressed still lands <code>success</code>.
                </p>
              </>
            ) : isAgent ? (
              <>
                <p className="mt-1 text-zinc-200">
                  <strong>Heads up for AI Agent nodes:</strong> this policy only governs whether the <em>agent loop</em> keeps running when a wired tool errors. The LLM still decides what to do next — retry, pivot to another tool, or end the turn.
                </p>
                <p className="mt-1 text-zinc-200">
                  The workflow does <strong>not</strong> force a specific tool sequence. If you need a guaranteed fallback (e.g. "always call <code>publish_weather</code> even if <code>get_weather</code> fails"), say so in the system prompt — that's how the model knows to call it anyway.
                </p>
              </>
            ) : (
              <p className="mt-1 text-zinc-200">
                When this node is invoked as an agent tool (<code>as_tool</code>), the same policy applies: <code>stop</code> aborts the agent loop on tool error,
                <code>continue</code> feeds the error back to the LLM so the model can pivot.
              </p>
            )}
          </TooltipContent>
        </Tooltip>
      </div>
      <Select
        value={current}
        onValueChange={(v) => updateNodeData(nodeId, { on_error: v })}
      >
        <SelectTrigger className="nodrag h-7 w-[180px] text-[11px]">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="stop">Stop run</SelectItem>
          <SelectItem value="continue">Continue past error</SelectItem>
        </SelectContent>
      </Select>
    </div>
  )
}

import { NodeResizer, type NodeProps, useReactFlow } from '@xyflow/react'
import { RefreshCw } from 'lucide-react'
import { useEffect } from 'react'
import { StepNameInput } from './StepNameInput'
import { OnErrorPolicySelect } from './OnErrorPolicySelect'
import { DynamicHandles } from './DynamicHandles'
import { NodeDebugPanel, BreakpointMarker, ApprovalMarker } from '../RunResultsContext'
import { OutputTransformPanel } from './OutputTransformPanel'

export function ForEachNode({ id, data, selected }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const requireApproval = (data?.require_node_approval as boolean) ?? false
  const onErrorPolicy = (data?.on_error as string) ?? 'stop'
  const parallelism = (data?.parallelism as number) ?? 1

  // Auto-coerce: flipping on_error to 'stop' while parallelism > 1
  // would otherwise leave the workflow in a save-time-rejected state
  // (engine refuses the combo). Drop parallelism back to 1 the moment
  // the policy goes incompatible.
  useEffect(() => {
    if (onErrorPolicy !== 'continue' && parallelism > 1) {
      updateNodeData(id, { parallelism: 1 })
    }
  }, [onErrorPolicy, parallelism, id, updateNodeData])
  return (
    <div className="relative min-w-[220px] h-full">
      <BreakpointMarker id={id} />
      <ApprovalMarker id={id} enabled={requireApproval} onToggle={(_, next) => updateNodeData(id, { require_node_approval: next })} />
      <div className="overflow-x-hidden rounded-lg border-2 border-violet-500 bg-card text-card-foreground shadow-sm h-full">
        <NodeResizer minWidth={220} minHeight={80} isVisible={selected} />
        <div className="flex items-center gap-2 px-4 py-2.5 border-b border-violet-500/40">
          <RefreshCw className="h-4 w-4 text-violet-400 shrink-0" />
          <span className="text-sm font-medium">For Each</span>
        </div>
        <StepNameInput id={id} data={data} />
        <input
          className="nodrag w-full px-4 py-1 text-[10px] text-muted-foreground bg-transparent
                     border-b border-border/20 focus:border-border/60 outline-none placeholder:italic font-mono"
          placeholder="items (optional) — e.g. {{input.docs}}; blank = iterate raw input"
          value={(data?.items as string) ?? ''}
          onChange={(e) => updateNodeData(id, { items: e.target.value })}
        />
        <div className="px-4 py-2 space-y-1.5 text-xs text-muted-foreground">
          <div className="flex items-center justify-between">
            <span>item →</span>
            <span className="text-[10px] text-violet-400">runs once per element</span>
          </div>
          <div className="flex items-center justify-between">
            <span>success →</span>
            <span className="text-[10px]">all outputs [ ]</span>
          </div>
          <p className="text-[9px] text-muted-foreground/60 pt-0.5">name → body access via <code className="text-[9px]">context.stepName.item</code></p>
        </div>
        <div className="px-3 py-2 border-t border-border/50 space-y-1.5">
          {/* Parallelism is gated on on_error: continue because the
              engine refuses to run parallel iterations under stop
              semantics (in-flight goroutines can't be cleanly aborted
              mid-flight). Disable + dim the input + show a helper
              line so the gating is visible BEFORE the author hits
              save and gets a 400. */}
          {(() => {
            const onErr = (data?.on_error as string) ?? 'stop'
            const allowParallel = onErr === 'continue'
            return (
              <>
                <div className={`flex items-center justify-between gap-2 ${allowParallel ? '' : 'opacity-50'}`}>
                  <label className="text-[10px] text-muted-foreground" title="1 (default) = sequential. >1 = bounded concurrent iterations. Requires on_error: continue — in-flight iterations can't be aborted mid-flight. Server clamps to 32.">
                    Parallelism
                  </label>
                  <input
                    type="number"
                    min={1}
                    max={32}
                    disabled={!allowParallel}
                    className="nodrag w-16 h-7 px-1.5 text-xs bg-transparent border border-border/40 rounded text-right font-mono disabled:cursor-not-allowed"
                    value={(data?.parallelism as number) ?? 1}
                    onChange={(e) => {
                      if (!allowParallel) return
                      const n = parseInt(e.target.value, 10)
                      if (!Number.isFinite(n) || n <= 1) {
                        updateNodeData(id, { parallelism: 1 })
                      } else {
                        updateNodeData(id, { parallelism: Math.min(32, n) })
                      }
                    }}
                  />
                </div>
                {!allowParallel && (
                  <p className="text-[9px] text-muted-foreground/70 italic">
                    Set on_error to <code className="text-[9px]">continue</code> to enable parallel iterations.
                  </p>
                )}
              </>
            )
          })()}
          <OnErrorPolicySelect nodeId={id} value={(data?.on_error as string) ?? 'stop'} nodeType="for_each" />
        </div>
        <OutputTransformPanel nodeId={id} data={data as Record<string, unknown>} />
        <NodeDebugPanel id={id} />
      </div>
      <DynamicHandles nodeId={id} nodeType="for_each" data={data as Record<string, unknown>} />
    </div>
  )
}

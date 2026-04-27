import { NodeResizer, type NodeProps } from '@xyflow/react'
import { RefreshCw } from 'lucide-react'
import { StepNameInput } from './StepNameInput'
import { DynamicHandles } from './DynamicHandles'
import { NodeDebugPanel, BreakpointMarker } from '../RunResultsContext'

export function ForEachNode({ id, data, selected }: NodeProps) {
  return (
    <div className="relative min-w-[220px]">
      <BreakpointMarker id={id} />
      <div className="overflow-x-hidden rounded-lg border-2 border-violet-500 bg-card text-card-foreground shadow-sm">
        <NodeResizer minWidth={220} minHeight={80} isVisible={selected} />
        <div className="flex items-center gap-2 px-4 py-2.5 border-b border-violet-500/40">
          <RefreshCw className="h-4 w-4 text-violet-400 shrink-0" />
          <span className="text-sm font-medium">For Each</span>
        </div>
        <StepNameInput id={id} data={data} />
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
        <NodeDebugPanel id={id} />
      </div>
      <DynamicHandles nodeId={id} nodeType="for_each" data={data as Record<string, unknown>} />
    </div>
  )
}

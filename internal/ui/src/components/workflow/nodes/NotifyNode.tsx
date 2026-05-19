import { NodeResizer, type NodeProps, useReactFlow } from '@xyflow/react'
import { Bell } from 'lucide-react'
import { Textarea } from '~/components/ui/textarea'
import { StepNameInput } from './StepNameInput'
import { DynamicHandles } from './DynamicHandles'
import { NodeDebugPanel, BreakpointMarker, ApprovalMarker } from '../RunResultsContext'
import { OnErrorPolicySelect } from './OnErrorPolicySelect'

export function NotifyNode({ id, data, selected }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const message = (data?.message as string) ?? ''
  const requireApproval = (data?.require_node_approval as boolean) ?? false

  return (
    <div className="relative min-w-[240px] h-full">
      <BreakpointMarker id={id} />
      <ApprovalMarker id={id} enabled={requireApproval} onToggle={(_, next) => updateNodeData(id, { require_node_approval: next })} />
      <div className="overflow-x-hidden rounded-lg border-2 border-amber-500 bg-card text-card-foreground shadow-sm h-full">
        <NodeResizer minWidth={240} minHeight={80} isVisible={selected} />
        <div className="flex items-center gap-2 px-4 py-2.5 border-b border-amber-500/40">
          <Bell className="h-4 w-4 text-amber-400 shrink-0" />
          <span className="text-sm font-medium">Notify</span>
        </div>
        <StepNameInput id={id} data={data} />
        <div className="px-3 py-2">
          <p className="text-[10px] text-muted-foreground mb-1">Message</p>
          <Textarea
            className="nodrag text-xs min-h-[28px] resize-y"
            rows={1}
            placeholder="optional log message"
            value={message}
            onChange={(e) => updateNodeData(id, { message: e.target.value })}
          />
        </div>
        <div className="px-3 py-2 border-t border-border/50">
          <OnErrorPolicySelect nodeId={id} value={(data?.on_error as string) ?? 'stop'} />
        </div>
        <NodeDebugPanel id={id} />
      </div>
      <DynamicHandles nodeId={id} nodeType="notify" data={data as Record<string, unknown>} />
    </div>
  )
}

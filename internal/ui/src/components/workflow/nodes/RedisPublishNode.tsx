import { NodeResizer, type NodeProps, useReactFlow } from '@xyflow/react'
import { Radio } from 'lucide-react'
import { Textarea } from '~/components/ui/textarea'
import { StepNameInput } from './StepNameInput'
import { DynamicHandles } from './DynamicHandles'
import { NodeDebugPanel, BreakpointMarker } from '../RunResultsContext'
import { ConnectionPicker } from './ConnectionPicker'

export function RedisPublishNode({ id, data, selected }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const channel = (data?.channel as string) ?? ''

  return (
    <div className="relative min-w-[260px]">
      <BreakpointMarker id={id} />
      <div className="overflow-x-hidden rounded-lg border-2 border-orange-500 bg-card text-card-foreground shadow-sm">
        <NodeResizer minWidth={260} minHeight={80} isVisible={selected} />
        <div className="flex items-center gap-2 px-4 py-2.5 border-b border-orange-500/40">
          <Radio className="h-4 w-4 text-orange-400 shrink-0" />
          <span className="text-sm font-medium flex-1">Redis Publish</span>
          <ConnectionPicker nodeId={id} connectionType="redis" data={data as Record<string, unknown>} activeColor="text-orange-400" />
        </div>
        <StepNameInput id={id} data={data} />
        <div className="px-3 py-2">
          <p className="text-[10px] text-muted-foreground mb-0.5">Channel</p>
          <Textarea
            className="nodrag text-xs min-h-[28px] resize-y"
            rows={1}
            placeholder="e.g. immaiwin:news:articles"
            value={channel}
            onChange={(e) => updateNodeData(id, { channel: e.target.value })}
          />
        </div>
        <NodeDebugPanel id={id} />
      </div>
      <DynamicHandles nodeId={id} nodeType="redis_publish" data={data as Record<string, unknown>} />
    </div>
  )
}

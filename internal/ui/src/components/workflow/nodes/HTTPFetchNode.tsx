import { NodeResizer, type NodeProps, useReactFlow } from '@xyflow/react'
import { Globe } from 'lucide-react'
import { Textarea } from '~/components/ui/textarea'
import { StepNameInput } from './StepNameInput'
import { DynamicHandles } from './DynamicHandles'
import { NodeDebugPanel, BreakpointMarker } from '../RunResultsContext'

export function HTTPFetchNode({ id, data, selected }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const url = (data?.url as string) ?? ''

  return (
    <div className="relative min-w-[280px] h-full">
      <BreakpointMarker id={id} />
      <div className="overflow-x-hidden rounded-lg border-2 border-sky-400 bg-card text-card-foreground shadow-sm h-full">
        <NodeResizer minWidth={280} minHeight={80} isVisible={selected} />
        <div className="flex items-center gap-2 px-4 py-2.5 border-b border-sky-400/40">
          <Globe className="h-4 w-4 text-sky-400 shrink-0" />
          <span className="text-sm font-medium">HTTP Fetch</span>
        </div>
        <StepNameInput id={id} data={data} />
        <div className="px-3 py-2">
          <p className="text-[10px] text-muted-foreground">URL — supports <code className="text-[10px]">{'{{…}}'}</code> templates</p>
          <Textarea
            className="nodrag text-xs min-h-[28px] resize-y"
            rows={1}
            placeholder="https://feeds.example.com/rss"
            value={url}
            onChange={(e) => updateNodeData(id, { url: e.target.value })}
          />
        </div>
        <NodeDebugPanel id={id} />
      </div>
      <DynamicHandles nodeId={id} nodeType="http_fetch" data={data as Record<string, unknown>} />
    </div>
  )
}

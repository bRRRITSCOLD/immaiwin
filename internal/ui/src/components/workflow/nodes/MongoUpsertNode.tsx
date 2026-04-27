import { NodeResizer, type NodeProps, useReactFlow } from '@xyflow/react'
import { Database } from 'lucide-react'
import { Textarea } from '~/components/ui/textarea'
import { StepNameInput } from './StepNameInput'
import { DynamicHandles } from './DynamicHandles'
import { NodeDebugPanel, BreakpointMarker } from '../RunResultsContext'
import { ConnectionPicker } from './ConnectionPicker'

export function MongoUpsertNode({ id, data, selected }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const collection = (data?.collection as string) ?? ''
  const filterField = (data?.filter_field as string) ?? ''
  return (
    <div className="relative min-w-[280px] h-full">
      <BreakpointMarker id={id} />
      <div className="overflow-x-hidden rounded-lg border-2 border-green-600 bg-card text-card-foreground shadow-sm h-full">
        <NodeResizer minWidth={280} minHeight={80} isVisible={selected} />
        <div className="flex items-center gap-2 px-4 py-2.5 border-b border-green-600/40">
          <Database className="h-4 w-4 text-green-500 shrink-0" />
          <span className="text-sm font-medium flex-1">Mongo Upsert</span>
          <ConnectionPicker nodeId={id} connectionType="mongodb" data={data as Record<string, unknown>} />
        </div>
        <StepNameInput id={id} data={data} />
        <div className="px-3 py-2 space-y-2">
          <div>
            <p className="text-[10px] text-muted-foreground mb-0.5">Collection</p>
            <Textarea
              className="nodrag text-xs min-h-[28px] resize-y"
              rows={1}
              placeholder="e.g. news_articles"
              value={collection}
              onChange={(e) => updateNodeData(id, { collection: e.target.value })}
            />
          </div>
          <div>
            <p className="text-[10px] text-muted-foreground mb-0.5">Filter field — dedup key</p>
            <Textarea
              className="nodrag text-xs min-h-[28px] resize-y"
              rows={1}
              placeholder="e.g. url"
              value={filterField}
              onChange={(e) => updateNodeData(id, { filter_field: e.target.value })}
            />
          </div>
        </div>
        <NodeDebugPanel id={id} />
      </div>
      <DynamicHandles nodeId={id} nodeType="mongo_upsert" data={data as Record<string, unknown>} />
    </div>
  )
}

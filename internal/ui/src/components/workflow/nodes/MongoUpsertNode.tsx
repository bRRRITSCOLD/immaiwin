import { Handle, Position, type NodeProps, useReactFlow } from '@xyflow/react'
import { Database } from 'lucide-react'
import { Input } from '~/components/ui/input'
import { StepNameInput } from './StepNameInput'
import { NodeDebugPanel } from '../RunResultsContext'

export function MongoUpsertNode({ id, data }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const collection = (data?.collection as string) ?? ''
  const filterField = (data?.filter_field as string) ?? ''
  return (
    <div className="w-[280px] rounded-lg border-2 border-green-600 bg-card text-card-foreground shadow-sm">
      <div className="flex items-center gap-2 px-4 py-2.5 border-b border-green-600/40">
        <Database className="h-4 w-4 text-green-500 shrink-0" />
        <span className="text-sm font-medium">Mongo Upsert</span>
      </div>
      <StepNameInput id={id} data={data} />
      <div className="px-3 py-2 space-y-2">
        <div>
          <p className="text-[10px] text-muted-foreground mb-0.5">Collection</p>
          <Input
            className="nodrag h-7 text-xs"
            placeholder="e.g. news_articles"
            value={collection}
            onChange={(e) => updateNodeData(id, { collection: e.target.value })}
          />
        </div>
        <div>
          <p className="text-[10px] text-muted-foreground mb-0.5">Filter field — dedup key</p>
          <Input
            className="nodrag h-7 text-xs"
            placeholder="e.g. url"
            value={filterField}
            onChange={(e) => updateNodeData(id, { filter_field: e.target.value })}
          />
        </div>
      </div>
      <NodeDebugPanel id={id} />
      <Handle type="target" position={Position.Left} />
      <Handle type="source" position={Position.Right} id="success" style={{ top: '35%' }} />
      <Handle
        type="source"
        position={Position.Right}
        id="error"
        style={{ top: '65%', background: 'rgb(239,68,68)' }}
      />
    </div>
  )
}

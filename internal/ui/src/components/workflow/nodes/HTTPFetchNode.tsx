import { Handle, Position, type NodeProps, useReactFlow } from '@xyflow/react'
import { Globe } from 'lucide-react'
import { Input } from '~/components/ui/input'
import { StepNameInput } from './StepNameInput'
import { NodeDebugPanel } from '../RunResultsContext'

export function HTTPFetchNode({ id, data }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const url = (data?.url as string) ?? ''

  return (
    <div className="w-[280px] rounded-lg border bg-card text-card-foreground shadow-sm">
      <div className="flex items-center gap-2 px-4 py-2.5 border-b">
        <Globe className="h-4 w-4 text-sky-400 shrink-0" />
        <span className="text-sm font-medium">HTTP Fetch</span>
      </div>
      <StepNameInput id={id} data={data} />
      <div className="px-3 py-2">
        <p className="text-[10px] text-muted-foreground">URL — supports <code className="text-[10px]">{'{{…}}'}</code> templates</p>
        <Input
          className="nodrag h-7 text-xs"
          placeholder="https://feeds.example.com/rss"
          value={url}
          onChange={(e) => updateNodeData(id, { url: e.target.value })}
        />
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

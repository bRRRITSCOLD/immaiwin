import { Handle, Position, type NodeProps, useReactFlow } from '@xyflow/react'
import { Radio } from 'lucide-react'
import { Input } from '~/components/ui/input'
import { StepNameInput } from './StepNameInput'
import { NodeDebugPanel } from '../RunResultsContext'

export function RedisPublishNode({ id, data }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const channel = (data?.channel as string) ?? ''

  return (
    <div className="w-[260px] rounded-lg border-2 border-orange-500 bg-card text-card-foreground shadow-sm">
      <div className="flex items-center gap-2 px-4 py-2.5 border-b border-orange-500/40">
        <Radio className="h-4 w-4 text-orange-400 shrink-0" />
        <span className="text-sm font-medium">Redis Publish</span>
      </div>
      <StepNameInput id={id} data={data} />
      <div className="px-3 py-2">
        <p className="text-[10px] text-muted-foreground mb-0.5">Channel</p>
        <Input
          className="nodrag h-7 text-xs"
          placeholder="e.g. immaiwin:news:articles"
          value={channel}
          onChange={(e) => updateNodeData(id, { channel: e.target.value })}
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

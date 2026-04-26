import { Handle, Position, type NodeProps, useReactFlow } from '@xyflow/react'
import { Bell } from 'lucide-react'
import { Input } from '~/components/ui/input'
import { StepNameInput } from './StepNameInput'
import { NodeDebugPanel } from '../RunResultsContext'

export function NotifyNode({ id, data }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const message = (data?.message as string) ?? ''

  return (
    <div className="w-[240px] rounded-lg border-2 border-amber-500 bg-card text-card-foreground shadow-sm">
      <div className="flex items-center gap-2 px-4 py-2.5 border-b border-amber-500/40">
        <Bell className="h-4 w-4 text-amber-400 shrink-0" />
        <span className="text-sm font-medium">Notify</span>
      </div>
      <StepNameInput id={id} data={data} />
      <div className="px-3 py-2">
        <p className="text-[10px] text-muted-foreground mb-1">Message</p>
        <Input
          className="nodrag h-7 text-xs"
          placeholder="optional log message"
          value={message}
          onChange={(e) => updateNodeData(id, { message: e.target.value })}
        />
      </div>
      <NodeDebugPanel id={id} />
      {/* Target handles on all 4 sides — connect from any direction */}
      <Handle type="target" position={Position.Top}    id="in-top"    />
      <Handle type="target" position={Position.Left}   id="in-left"   />
      <Handle type="target" position={Position.Bottom} id="in-bottom" />
      <Handle type="target" position={Position.Right}  id="in-right"  style={{ top: '35%' }} />
      {/* Source — chain after notify if needed */}
      <Handle type="source" position={Position.Right}  id="out"       style={{ top: '65%' }} />
    </div>
  )
}

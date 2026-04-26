import { Handle, Position, type NodeProps } from '@xyflow/react'
import { Play } from 'lucide-react'
import { StepNameInput } from './StepNameInput'
import { NodeDebugPanel } from '../RunResultsContext'

export function TriggerNode({ id, data }: NodeProps) {
  return (
    <div className="w-[220px] rounded-lg border-2 border-blue-500 bg-card text-card-foreground shadow-sm">
      <div className="flex items-center gap-2 px-4 py-3 border-b border-blue-500/40">
        <Play className="h-4 w-4 text-blue-500 shrink-0" />
        <span className="text-sm font-medium">Manual Trigger</span>
      </div>
      <StepNameInput id={id} data={data} />
      <NodeDebugPanel id={id} />
      <Handle type="source" position={Position.Right} />
    </div>
  )
}

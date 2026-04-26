import { Handle, Position } from '@xyflow/react'
import { Database } from 'lucide-react'

export function OutputNode() {
  return (
    <div className="w-[220px] rounded-lg border-2 border-green-500 bg-card text-card-foreground shadow-sm">
      <div className="flex items-center gap-2 px-4 py-3">
        <Database className="h-4 w-4 text-green-500 shrink-0" />
        <span className="text-sm font-medium">MongoDB → Publish</span>
      </div>
      <Handle type="target" position={Position.Left} />
    </div>
  )
}

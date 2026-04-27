import { useReactFlow } from '@xyflow/react'
import { Plug } from 'lucide-react'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '~/components/ui/select'
import { useWorkflowStore, type ConnectionType } from '../useWorkflowStore'

interface Props {
  nodeId: string
  connectionType: ConnectionType
  data: Record<string, unknown>
  activeColor?: string
}

export function ConnectionPicker({ nodeId, connectionType, data, activeColor = 'text-green-500' }: Props) {
  const { updateNodeData } = useReactFlow()
  const connections = useWorkflowStore((s) => s.connections)
  const filtered = connections.filter((c) => c.type === connectionType)
  const currentId = (data?.connection_id as string) ?? ''
  const currentName = filtered.find((c) => c.id === currentId)?.name

  return (
    <Select
      value={currentId || '__default__'}
      onValueChange={(v) => updateNodeData(nodeId, { connection_id: v === '__default__' ? '' : v })}
    >
      <SelectTrigger className="nodrag h-6 max-w-[120px] gap-1 border-none bg-transparent px-1.5 text-xs shadow-none focus:ring-0">
        <Plug className={`h-3 w-3 shrink-0 ${currentId ? activeColor : 'text-muted-foreground'}`} />
        <span className="truncate"><SelectValue>{currentName ?? 'Default'}</SelectValue></span>
      </SelectTrigger>
      <SelectContent className="z-[9999]">
        <SelectItem value="__default__">Default (env)</SelectItem>
        {filtered.map((c) => (
          <SelectItem key={c.id} value={c.id}>
            {c.name}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

import { useReactFlow } from '@xyflow/react'
import { Plug } from 'lucide-react'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '~/components/ui/select'
import { useWorkflowStore, type ConnectionType } from '../useWorkflowStore'

interface Props {
  nodeId: string
  // One or many acceptable connection types. AI Agent accepts
  // anthropic / openai / ollama; everything else picks a single type.
  connectionType: ConnectionType | ConnectionType[]
  data: Record<string, unknown>
  // Which field on the node's data to read/write. Defaults to
  // connection_id; AI Agent overrides to llm_connection_id.
  field?: string
  // Visual variant. `icon` = compact node-header pill with a plug icon
  // (used by request nodes + triggers in the title bar). `full` =
  // full-width select for in-body forms (AI Agent's LLM picker).
  variant?: 'icon' | 'full'
  // Color of the plug icon when a real connection is selected. Ignored
  // in `full` variant (no icon).
  activeColor?: string
  // requireExplicit drops the "Default (env)" fallback option from
  // the dropdown AND disables the trigger when the connection list
  // is empty. Workers that don't have an env fallback (trigger
  // consumers, AI agent's required LLM) must use this so the user
  // can't ship a workflow without a real connection wired.
  requireExplicit?: boolean
  // Optional placeholder override when nothing is picked. Falls back
  // to a sensible default keyed off requireExplicit.
  placeholder?: string
}

const DEFAULT_FIELD = 'connection_id'
const DEFAULT_SENTINEL = '__default__'

export function ConnectionPicker({
  nodeId,
  connectionType,
  data,
  field = DEFAULT_FIELD,
  variant = 'icon',
  activeColor = 'text-green-500',
  requireExplicit,
  placeholder,
}: Props) {
  const { updateNodeData } = useReactFlow()
  const connections = useWorkflowStore((s) => s.connections)
  const acceptedTypes = Array.isArray(connectionType) ? connectionType : [connectionType]
  const multiType = acceptedTypes.length > 1
  const filtered = connections.filter((c) => acceptedTypes.includes(c.type))
  const rawCurrentId = (data?.[field] as string) ?? ''
  // A connection_id only counts as "selected" when it actually maps
  // to one of the available connections. Stale ids from a deleted
  // connection or a not-yet-replaced template placeholder
  // (REPLACE_WITH_*_CONNECTION_ID) shouldn't make the icon look
  // active or trip the controlled-Select into thinking something's
  // chosen.
  const currentMatch = filtered.find((c) => c.id === rawCurrentId)
  const currentId = currentMatch ? rawCurrentId : ''
  const currentLabel = currentMatch
    ? multiType
      ? `${currentMatch.name} (${currentMatch.type})`
      : currentMatch.name
    : undefined

  const noConnections = filtered.length === 0
  const typeLabel = acceptedTypes.join(' / ')
  const computedPlaceholder = requireExplicit
    ? noConnections
      ? `No ${typeLabel} connection — add one in Settings → Connections`
      : 'Pick a connection'
    : 'Default'
  const finalPlaceholder = placeholder ?? computedPlaceholder

  const disabled = requireExplicit && noConnections

  const writeChange = (v: string) => {
    updateNodeData(nodeId, { [field]: v === DEFAULT_SENTINEL ? '' : v })
  }

  const triggerCls =
    variant === 'icon'
      ? 'nodrag h-6 max-w-[140px] gap-1 border-none bg-transparent px-1.5 text-xs shadow-none focus:ring-0 disabled:cursor-not-allowed disabled:opacity-60'
      : 'nodrag h-7 text-xs disabled:cursor-not-allowed disabled:opacity-60'

  return (
    <Select
      disabled={disabled}
      value={currentId || (requireExplicit ? '' : DEFAULT_SENTINEL)}
      onValueChange={writeChange}
    >
      <SelectTrigger
        className={triggerCls}
        title={disabled ? `No ${typeLabel} connections — add one under Settings → Connections.` : undefined}
      >
        {variant === 'icon' && (
          <Plug className={`h-3 w-3 shrink-0 ${currentId ? activeColor : 'text-muted-foreground'}`} />
        )}
        <span className="truncate"><SelectValue placeholder={finalPlaceholder}>{currentLabel}</SelectValue></span>
      </SelectTrigger>
      <SelectContent className="z-[9999]">
        {!requireExplicit && (
          <SelectItem value={DEFAULT_SENTINEL}>Default (env)</SelectItem>
        )}
        {filtered.map((c) => (
          <SelectItem key={c.id} value={c.id}>
            {multiType ? `${c.name} (${c.type})` : c.name}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

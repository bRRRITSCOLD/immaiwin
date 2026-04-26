import { Globe, Play, Code2, Database, Bell, RefreshCw, Radio } from 'lucide-react'
import { Separator } from '~/components/ui/separator'
import { Button } from '~/components/ui/button'
import { useWorkflowStore, type Workflow } from './useWorkflowStore'

interface Props {
  onSelect(id: string): void
}

export function WorkflowSidebar({ onSelect }: Props) {
  const { workflows, activeId } = useWorkflowStore()

  function onDragStart(e: React.DragEvent, nodeType: string) {
    e.dataTransfer.setData('application/workflow-node-type', nodeType)
    e.dataTransfer.effectAllowed = 'move'
  }

  return (
    <aside className="w-[280px] shrink-0 border-r flex flex-col h-full overflow-hidden">
      {/* Workflow list */}
      <div className="px-4 py-3 shrink-0">
        <p className="text-xs font-semibold text-muted-foreground uppercase tracking-wider">
          Workflows
        </p>
      </div>
      <div className="flex-1 overflow-y-auto px-2 pb-2 space-y-1">
        {workflows.length === 0 && (
          <p className="px-2 text-xs text-muted-foreground">No workflows yet.</p>
        )}
        {workflows.map((wf: Workflow) => (
          <Button
            key={wf.id}
            variant={activeId === wf.id ? 'secondary' : 'ghost'}
            size="sm"
            className="w-full justify-start text-sm"
            onClick={() => onSelect(wf.id)}
          >
            {wf.name}
          </Button>
        ))}
      </div>

      <Separator />

      {/* Node palette */}
      <div className="px-4 py-3 shrink-0">
        <p className="text-xs font-semibold text-muted-foreground uppercase tracking-wider mb-3">
          Node Palette
        </p>
        <div className="space-y-2">
          <PaletteItem
            icon={<Play className="h-3.5 w-3.5 text-blue-500" />}
            label="Trigger"
            nodeType="trigger"
            onDragStart={onDragStart}
          />
          <PaletteItem
            icon={<Globe className="h-3.5 w-3.5 text-sky-400" />}
            label="HTTP Fetch"
            nodeType="http_fetch"
            onDragStart={onDragStart}
          />
          <PaletteItem
            icon={<Code2 className="h-3.5 w-3.5 text-yellow-400" />}
            label="JS Transform"
            nodeType="js_transform"
            onDragStart={onDragStart}
          />
          <PaletteItem
            icon={<RefreshCw className="h-3.5 w-3.5 text-violet-400" />}
            label="For Each"
            nodeType="for_each"
            onDragStart={onDragStart}
          />
          <PaletteItem
            icon={<Database className="h-3.5 w-3.5 text-green-500" />}
            label="Mongo Upsert"
            nodeType="mongo_upsert"
            onDragStart={onDragStart}
          />
          <PaletteItem
            icon={<Radio className="h-3.5 w-3.5 text-orange-400" />}
            label="Redis Publish"
            nodeType="redis_publish"
            onDragStart={onDragStart}
          />
          <PaletteItem
            icon={<Bell className="h-3.5 w-3.5 text-amber-400" />}
            label="Notify"
            nodeType="notify"
            onDragStart={onDragStart}
          />
        </div>
      </div>
    </aside>
  )
}

function PaletteItem({
  icon,
  label,
  nodeType,
  onDragStart,
}: {
  icon: React.ReactNode
  label: string
  nodeType: string
  onDragStart(e: React.DragEvent, t: string): void
}) {
  return (
    <div
      draggable
      onDragStart={(e) => onDragStart(e, nodeType)}
      className="flex items-center gap-2 px-3 py-2 rounded-md border border-border bg-secondary text-secondary-foreground cursor-grab text-sm hover:bg-accent transition-colors"
    >
      {icon}
      <span>{label}</span>
    </div>
  )
}

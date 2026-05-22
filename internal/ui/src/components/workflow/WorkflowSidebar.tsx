import { Globe, Play, Container, Database, Bell, RefreshCw, Radio, ChevronDown, ChevronUp, CheckCircle2, XCircle, Circle, Plus, Pencil, Trash2, Plug, Download, Bot, Wrench, Copy, Power, PowerOff, Workflow as WorkflowIcon, CornerDownLeft } from 'lucide-react'
import { useState } from 'react'
import { toast } from 'sonner'
import { Separator } from '~/components/ui/separator'
import { Button } from '~/components/ui/button'
import { Badge } from '~/components/ui/badge'
import { useWorkflowStore, type Workflow, type Connection, type EdgePaletteType } from './useWorkflowStore'
import { ConnectionDialog } from './ConnectionDialog'
import { AddWorkflowDialog } from './AddWorkflowDialog'
import { api } from '~/lib/api'

interface Props {
  onSelect(id: string): void
  onReload(): void
}

const edgePaletteItems: { type: EdgePaletteType; label: string; icon: React.ReactNode; color: string; border: string }[] = [
  { type: 'start', label: 'Start', icon: <Play className="h-3.5 w-3.5" />, color: 'text-blue-500', border: 'border-blue-500' },
  { type: 'success', label: 'Success', icon: <CheckCircle2 className="h-3.5 w-3.5" />, color: 'text-green-500', border: 'border-green-500' },
  { type: 'error', label: 'Error', icon: <XCircle className="h-3.5 w-3.5" />, color: 'text-red-500', border: 'border-red-500' },
  { type: 'item', label: 'Item', icon: <RefreshCw className="h-3.5 w-3.5" />, color: 'text-violet-400', border: 'border-violet-400' },
  { type: 'tool', label: 'Tool', icon: <Wrench className="h-3.5 w-3.5" />, color: 'text-purple-400', border: 'border-purple-400' },
  { type: 'receive', label: 'Receive', icon: <Circle className="h-3.5 w-3.5" />, color: 'text-gray-400', border: 'border-gray-400' },
]

export function WorkflowSidebar({ onSelect, onReload }: Props) {
  const { workflows, connections, activeId, selectedEdgeType, setSelectedEdgeType, setConnections, setActive } = useWorkflowStore()
  const [paletteOpen, setPaletteOpen] = useState(true)
  const [edgePaletteOpen, setEdgePaletteOpen] = useState(true)
  const [connectionsOpen, setConnectionsOpen] = useState(true)
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editingConn, setEditingConn] = useState<Connection | null>(null)
  // Inline-rename state: which workflow row is in edit mode + the
  // current draft name. Enter saves, Esc / blur cancels.
  const [renameId, setRenameId] = useState<string | null>(null)
  const [renameDraft, setRenameDraft] = useState('')

  async function reloadConnections() {
    try {
      const conns = await api.get<Connection[]>('/api/v1/connections')
      setConnections(conns)
    } catch {
      toast.error('Failed to reload connections')
    }
  }

  async function handleDeleteWorkflow(e: React.MouseEvent, id: string) {
    e.stopPropagation()
    try {
      await api.delete(`/api/v1/workflows/${id}`)
      toast.success('Workflow deleted')
      if (activeId === id) setActive(null)
      onReload()
    } catch {
      toast.error('Failed to delete workflow')
    }
  }

  function beginRename(e: React.MouseEvent, wf: Workflow) {
    e.stopPropagation()
    setRenameId(wf.id)
    setRenameDraft(wf.name)
  }

  function cancelRename() {
    setRenameId(null)
    setRenameDraft('')
  }

  async function commitRename(wf: Workflow) {
    const next = renameDraft.trim()
    if (!next || next === wf.name) {
      cancelRename()
      return
    }
    if (next.length > 200) {
      toast.error('Name too long (max 200 chars)')
      return
    }
    try {
      await api.patch<Workflow>(`/api/v1/workflows/${wf.id}/name`, { name: next })
      toast.success('Workflow renamed')
      cancelRename()
      onReload()
    } catch {
      toast.error('Failed to rename workflow')
    }
  }

  async function handleToggleEnabled(e: React.MouseEvent, wf: Workflow) {
    e.stopPropagation()
    const next = wf.enabled === false // current disabled → enable
    try {
      await api.patch<Workflow>(`/api/v1/workflows/${wf.id}/enabled`, { enabled: next })
      toast.success(next ? 'Workflow enabled' : 'Workflow disabled — triggers paused, manual Run still works')
      onReload()
    } catch {
      toast.error(next ? 'Failed to enable workflow' : 'Failed to disable workflow')
    }
  }

  async function handleDuplicateWorkflow(e: React.MouseEvent, id: string) {
    e.stopPropagation()
    try {
      const dup = await api.post<Workflow>(`/api/v1/workflows/${id}/duplicate`)
      toast.success(`Duplicated as "${dup.name}"`)
      onReload()
      setActive(dup.id)
    } catch {
      toast.error('Failed to duplicate workflow')
    }
  }

  function handleExportWorkflow(e: React.MouseEvent, wf: Workflow) {
    e.stopPropagation()
    const bundle = { version: 1, workflow: wf }
    const blob = new Blob([JSON.stringify(bundle, null, 2)], { type: 'application/json' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `${wf.name.toLowerCase().replace(/\s+/g, '-')}.workflow.json`
    a.click()
    URL.revokeObjectURL(url)
    toast.success('Workflow exported')
  }

  async function handleDeleteConn(id: string) {
    try {
      await api.delete(`/api/v1/connections/${id}`)
      toast.success('Connection deleted')
      reloadConnections()
    } catch {
      toast.error('Failed to delete connection')
    }
  }

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
        {workflows.map((wf: Workflow) => {
          const enabled = wf.enabled !== false // undefined = enabled (legacy / new doc default)
          return (
          <div key={wf.id} className={`group flex items-center ${enabled ? '' : 'opacity-60'} ${!enabled ? 'border-l-2 border-amber-500/60 pl-0.5' : ''}`}>
            {renameId === wf.id ? (
              <form
                className="flex-1 flex items-center"
                onSubmit={(e) => { e.preventDefault(); void commitRename(wf) }}
              >
                <input
                  autoFocus
                  className="flex-1 bg-background border border-border rounded px-2 py-1 text-sm outline-none focus:border-primary"
                  value={renameDraft}
                  onChange={(e) => setRenameDraft(e.target.value)}
                  onKeyDown={(e) => { if (e.key === 'Escape') cancelRename() }}
                  onBlur={() => void commitRename(wf)}
                  maxLength={200}
                />
              </form>
            ) : (
              <Button
                variant={activeId === wf.id ? 'secondary' : 'ghost'}
                size="sm"
                className="flex-1 min-w-0 justify-start text-sm truncate gap-1"
                onClick={() => onSelect(wf.id)}
                onDoubleClick={(e) => beginRename(e, wf)}
                title={!enabled ? `${wf.name}\n(disabled — triggers paused; manual Run still works. Double-click to rename.)` : `${wf.name}\n(Double-click to rename)`}
              >
                <span className="truncate">{wf.name}</span>
                {typeof wf.version === 'number' && wf.version > 0 && (
                  <span className="text-[10px] text-muted-foreground font-mono shrink-0 opacity-0 group-hover:opacity-100 transition-opacity" title={`Saved revision ${wf.version}`}>
                    v{wf.version}
                  </span>
                )}
              </Button>
            )}
            {renameId !== wf.id && (
              <button
                className="opacity-0 group-hover:opacity-100 p-1 hover:text-foreground text-muted-foreground transition-opacity shrink-0"
                onClick={(e) => beginRename(e, wf)}
                title="Rename workflow"
              >
                <Pencil className="h-3.5 w-3.5" />
              </button>
            )}
            <button
              className={`opacity-0 group-hover:opacity-100 p-1 transition-opacity shrink-0 ${enabled ? 'text-muted-foreground hover:text-foreground' : 'text-amber-400 hover:text-amber-300'}`}
              onClick={(e) => handleToggleEnabled(e, wf)}
              title={enabled ? 'Disable workflow (pause triggers)' : 'Enable workflow (resume triggers)'}
            >
              {enabled ? <Power className="h-3.5 w-3.5" /> : <PowerOff className="h-3.5 w-3.5" />}
            </button>
            <button
              className="opacity-0 group-hover:opacity-100 p-1 hover:text-foreground text-muted-foreground transition-opacity shrink-0"
              onClick={(e) => handleDuplicateWorkflow(e, wf.id)}
              title="Save as new workflow"
            >
              <Copy className="h-3.5 w-3.5" />
            </button>
            <button
              className="opacity-0 group-hover:opacity-100 p-1 hover:text-foreground text-muted-foreground transition-opacity shrink-0"
              onClick={(e) => handleExportWorkflow(e, wf)}
              title="Export workflow"
            >
              <Download className="h-3.5 w-3.5" />
            </button>
            <button
              className="opacity-0 group-hover:opacity-100 p-1 hover:text-destructive text-muted-foreground transition-opacity shrink-0"
              onClick={(e) => handleDeleteWorkflow(e, wf.id)}
              title="Delete workflow"
            >
              <Trash2 className="h-3.5 w-3.5" />
            </button>
          </div>
          )
        })}
        <AddWorkflowDialog onCreated={onReload} />
      </div>

      <Separator />

      {/* Connections */}
      <div className="px-4 py-3 shrink-0">
        <button
          className="flex items-center justify-between w-full text-xs font-semibold text-muted-foreground uppercase tracking-wider"
          onClick={() => setConnectionsOpen((v) => !v)}
        >
          Connections
          {connectionsOpen ? <ChevronDown className="h-3.5 w-3.5" /> : <ChevronUp className="h-3.5 w-3.5" />}
        </button>
        {connectionsOpen && (
          <div className="space-y-1 mt-2">
            {connections.length === 0 && (
              <p className="text-[10px] text-muted-foreground">No connections yet.</p>
            )}
            {connections.map((conn) => (
              <div key={conn.id} className="flex items-center gap-1.5 px-2 py-1.5 rounded-md hover:bg-accent text-xs group">
                {conn.type === 'mongodb' ? (
                  <Database className="h-3 w-3 text-green-500 shrink-0" />
                ) : (
                  <Radio className="h-3 w-3 text-orange-400 shrink-0" />
                )}
                <span className="flex-1 truncate">{conn.name}</span>
                <Badge variant="outline" className="text-[9px] px-1 py-0">{conn.type}</Badge>
                <button
                  className="opacity-0 group-hover:opacity-100 p-0.5 hover:text-foreground text-muted-foreground transition-opacity"
                  onClick={() => { setEditingConn(conn); setDialogOpen(true) }}
                >
                  <Pencil className="h-3 w-3" />
                </button>
                <button
                  className="opacity-0 group-hover:opacity-100 p-0.5 hover:text-destructive text-muted-foreground transition-opacity"
                  onClick={() => handleDeleteConn(conn.id)}
                >
                  <Trash2 className="h-3 w-3" />
                </button>
              </div>
            ))}
            <button
              className="flex items-center gap-1.5 w-full px-2 py-1.5 rounded-md border border-dashed border-border text-xs text-muted-foreground hover:bg-accent transition-colors"
              onClick={() => { setEditingConn(null); setDialogOpen(true) }}
            >
              <Plus className="h-3 w-3" />
              Add Connection
            </button>
          </div>
        )}
      </div>

      <ConnectionDialog
        open={dialogOpen}
        onOpenChange={setDialogOpen}
        connection={editingConn}
        onSaved={reloadConnections}
      />

      <Separator />

      {/* Edge palette */}
      <div className="px-4 py-3 shrink-0">
        <button
          className="flex items-center justify-between w-full text-xs font-semibold text-muted-foreground uppercase tracking-wider"
          onClick={() => setEdgePaletteOpen((v) => !v)}
        >
          Edge Palette
          {edgePaletteOpen ? <ChevronDown className="h-3.5 w-3.5" /> : <ChevronUp className="h-3.5 w-3.5" />}
        </button>
        {edgePaletteOpen && (
          <div className="space-y-2 mt-3">
            <div className="grid grid-cols-2 gap-2">
              {edgePaletteItems.map((item) => (
                <button
                  key={item.type}
                  onClick={() => setSelectedEdgeType(item.type)}
                  className={`flex items-center gap-1.5 px-2.5 py-1.5 rounded-md border text-xs font-medium transition-colors ${
                    selectedEdgeType === item.type
                      ? `${item.border} ${item.color} bg-accent border-2`
                      : 'border-border text-muted-foreground hover:bg-accent'
                  }`}
                >
                  <span className={item.color}>{item.icon}</span>
                  {item.label}
                </button>
              ))}
            </div>
            {selectedEdgeType && (
              <p className="text-[10px] text-muted-foreground">
                Drag between handles. <kbd className="text-[10px] bg-muted px-1 rounded">Esc</kbd> to cancel.
              </p>
            )}
          </div>
        )}
      </div>

      <Separator />

      {/* Node palette */}
      <div className="px-4 py-3 shrink-0">
        <button
          className="flex items-center justify-between w-full text-xs font-semibold text-muted-foreground uppercase tracking-wider"
          onClick={() => setPaletteOpen((v) => !v)}
        >
          Node Palette
          {paletteOpen ? <ChevronDown className="h-3.5 w-3.5" /> : <ChevronUp className="h-3.5 w-3.5" />}
        </button>
        {paletteOpen && <div className="space-y-2 mt-3">
          <PaletteItem
            icon={<Play className="h-3.5 w-3.5 text-blue-500" />}
            label="Trigger"
            nodeType="trigger"
            onDragStart={onDragStart}
          />
          <PaletteItem
            icon={<Globe className="h-3.5 w-3.5 text-sky-400" />}
            label="HTTP Request"
            nodeType="http_request"
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
            label="Mongo Request"
            nodeType="mongo_request"
            onDragStart={onDragStart}
          />
          <PaletteItem
            icon={<Radio className="h-3.5 w-3.5 text-orange-400" />}
            label="Redis Request"
            nodeType="redis_request"
            onDragStart={onDragStart}
          />
          <PaletteItem
            icon={<Container className="h-3.5 w-3.5 text-cyan-500" />}
            label="Sandbox Script"
            nodeType="sandbox_script"
            onDragStart={onDragStart}
          />
          <PaletteItem
            icon={<Bell className="h-3.5 w-3.5 text-amber-400" />}
            label="Notify"
            nodeType="notify"
            onDragStart={onDragStart}
          />
          <PaletteItem
            icon={<Bot className="h-3.5 w-3.5 text-purple-400" />}
            label="AI Agent"
            nodeType="ai_agent"
            onDragStart={onDragStart}
          />
          <PaletteItem
            icon={<WorkflowIcon className="h-3.5 w-3.5 text-fuchsia-400" />}
            label="Sub-workflow"
            nodeType="sub_workflow"
            onDragStart={onDragStart}
          />
          <PaletteItem
            icon={<CornerDownLeft className="h-3.5 w-3.5 text-emerald-400" />}
            label="Return"
            nodeType="return"
            onDragStart={onDragStart}
          />
        </div>}
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

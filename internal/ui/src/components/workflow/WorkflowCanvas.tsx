import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { WSPreviewPanel } from './WSPreviewPanel'
import {
  ReactFlow,
  ReactFlowProvider,
  Background,
  Controls,
  MiniMap,
  addEdge,
  useNodesState,
  useEdgesState,
  useReactFlow,
  type Connection,
  type Node,
  type Edge,
  type NodeTypes,
  type EdgeTypes,
} from '@xyflow/react'
import { WaypointEdge } from './WaypointEdge'
import { TriggerNode } from './nodes/TriggerNode'
import { HTTPFetchNode } from './nodes/HTTPFetchNode'
import { ForEachNode } from './nodes/ForEachNode'
import { MongoUpsertNode } from './nodes/MongoUpsertNode'
import { RedisPublishNode } from './nodes/RedisPublishNode'
import { NotifyNode } from './nodes/NotifyNode'
import { SandboxScriptNode } from './nodes/SandboxScriptNode'
import { useWorkflowStore, type Workflow } from './useWorkflowStore'
import { WorkflowParamsPanel } from './WorkflowParamsPanel'
import { WorkflowHelpLegend } from './WorkflowHelpLegend'
import { RunResultsContext, DebugContext, type RunResults } from './RunResultsContext'

const nodeTypes: NodeTypes = {
  trigger: TriggerNode,
  http_fetch: HTTPFetchNode,
  for_each: ForEachNode,
  mongo_upsert: MongoUpsertNode,
  redis_publish: RedisPublishNode,
  notify: NotifyNode,
  sandbox_script: SandboxScriptNode,
}

const edgeTypes: EdgeTypes = {
  default: WaypointEdge,
  smoothstep: WaypointEdge,
  step: WaypointEdge,
  straight: WaypointEdge,
}

const defaultNodeData: Record<string, Record<string, unknown>> = {
  trigger: { trigger_type: 'manual', name: '' },
  http_fetch: { url: '', name: '' },
  for_each: { name: '' },
  mongo_upsert: { collection: '', filter_field: '', name: '' },
  redis_publish: { channel: '', name: '' },
  notify: { message: '', name: '' },
  sandbox_script: { script: '', language: 'javascript', timeout: 30, mem_limit: 128, cpu_limit: 0.5, network: false, name: '' },
}

/**
 * BFS-marks all nodes reachable via "item" edges from for_each nodes.
 * Mirrors Go buildForEachBodies.
 */
function getForEachBodyIds(nodes: Node[], edges: Edge[]): Set<string> {
  const bodies = new Set<string>()
  const forEachIds = new Set(nodes.filter((n) => n.type === 'for_each').map((n) => n.id))

  const adj = new Map<string, string[]>()
  for (const e of edges) {
    if (!adj.has(e.source)) adj.set(e.source, [])
    adj.get(e.source)!.push(e.target)
  }

  const itemTargets: string[] = []
  for (const e of edges) {
    const isItem = e.sourceHandle === 'item' || (e.data?.paletteType as string) === 'item'
    if (forEachIds.has(e.source) && isItem) {
      itemTargets.push(e.target)
    }
  }

  const q = [...itemTargets]
  while (q.length > 0) {
    const id = q.shift()!
    if (bodies.has(id)) continue
    bodies.add(id)
    for (const next of adj.get(id) ?? []) {
      if (!bodies.has(next)) q.push(next)
    }
  }

  return bodies
}

/**
 * Derive edge stroke + label from semantic edge type.
 * Uses edge.selected for lighter selected-state color.
 * Never spreads existing style — always computes fresh so DB-loaded edges render correctly.
 *
 * Types:
 *  body error             → dashed purple+red  "error (item)"   [custom edge]
 *  error                  → solid red           "error"
 *  item                   → solid violet        "item"
 *  trigger source         → solid blue          "start"
 *  body success (in body) → dashed purple+green "success (item)" [custom edge]
 *  default/success        → solid green         "success"
 */
const paletteColorMap: Record<string, { normal: string; selected: string }> = {
  start:   { normal: '#3b82f6', selected: '#93c5fd' },
  success: { normal: '#22c55e', selected: '#86efac' },
  error:   { normal: '#ef4444', selected: '#fca5a5' },
  item:    { normal: '#a78bfa', selected: '#ddd6fe' },
  receive: { normal: '#888888', selected: '#bbbbbb' },
}

function applyEdgeStyle(
  edge: Edge,
  triggerIds: Set<string>,
  bodyIds: Set<string>,
): Edge {
  const h = (edge.sourceHandle ?? '').toLowerCase()
  const sel = edge.selected ?? false

  const routing = (edge.data?.routing as string) || 'default'

  const labelStyle = { fill: '#ffffff' }
  const dash = '8 4'

  // Palette-typed edge — fast path
  const pt = edge.data?.paletteType as string | undefined
  if (pt && paletteColorMap[pt]) {
    const c = paletteColorMap[pt]
    return {
      ...edge,
      type: routing,
      style: { stroke: sel ? c.selected : c.normal },
      labelStyle,
      label: edge.label ?? pt,
    }
  }

  // Body error — dashed red
  if (h === 'error' && bodyIds.has(edge.source)) {
    return {
      ...edge,
      type: routing,
      style: { stroke: sel ? '#fca5a5' : '#ef4444', strokeDasharray: dash },
      labelStyle,
      label: edge.label ?? 'error (item)',
    }
  }

  if (h === 'error') {
    return {
      ...edge,
      type: routing,
      style: { stroke: sel ? '#fca5a5' : '#ef4444' },
      labelStyle,
      label: edge.label ?? 'error',
    }
  }

  if (h === 'item') {
    return {
      ...edge,
      type: routing,
      style: { stroke: sel ? '#ddd6fe' : '#a78bfa' },
      labelStyle,
      label: edge.label ?? 'item',
    }
  }

  if (triggerIds.has(edge.source)) {
    return {
      ...edge,
      type: routing,
      style: { stroke: sel ? '#93c5fd' : '#3b82f6' },
      labelStyle,
      label: edge.label ?? 'start',
    }
  }

  // Body success — dashed green
  if (bodyIds.has(edge.source)) {
    return {
      ...edge,
      type: routing,
      style: { stroke: sel ? '#86efac' : '#22c55e', strokeDasharray: dash },
      labelStyle,
      label: edge.label ?? 'success (item)',
    }
  }

  return {
    ...edge,
    type: routing,
    style: { stroke: sel ? '#86efac' : '#22c55e' },
    labelStyle,
    label: edge.label ?? 'success',
  }
}

interface Props {
  workflow: Workflow
  onSave(nodes: Node[], edges: Edge[], params: Record<string, string>): void
  onRun(stopAt?: string, input?: unknown): void
  onClearRun(): void
  lastRun?: RunResults
}

const routingCycle = ['default', 'smoothstep', 'step', 'straight'] as const

let nodeIdCounter = Date.now()
function nextNodeId() {
  return `n-${++nodeIdCounter}`
}

/**
 * Validate palette edge type against source node type.
 * Returns false → connection blocked.
 */
function isPaletteConnectionValid(
  sourceNodeType: string | undefined,
  paletteType: string,
): boolean {
  if (!sourceNodeType) return false
  switch (paletteType) {
    case 'start': return sourceNodeType === 'trigger'
    case 'item': return sourceNodeType === 'for_each'
    case 'success':
    case 'error': return sourceNodeType !== 'trigger'
    case 'receive': return true
    default: return true
  }
}

export function WorkflowCanvas(props: Props) {
  return (
    <ReactFlowProvider>
      <WorkflowCanvasInner {...props} />
    </ReactFlowProvider>
  )
}

interface EdgeMenuState {
  edgeId: string
  screenX: number
  screenY: number
  flowX: number
  flowY: number
}

function WorkflowCanvasInner({ workflow, onSave, onRun, onClearRun, lastRun }: Props) {
  const { updateActiveGraph, selectedEdgeType, setSelectedEdgeType, attachingFrom, setAttachingFrom } = useWorkflowStore()
  const { screenToFlowPosition } = useReactFlow()
  const [nodes, setNodes, onNodesChange] = useNodesState(workflow.nodes)
  const [edges, setEdges, onEdgesChange] = useEdgesState(workflow.edges)
  const [params, setParams] = useState<Record<string, string>>(workflow.params ?? {})
  const reactFlowWrapper = useRef<HTMLDivElement>(null)
  const [debugMode, setDebugMode] = useState(false)
  const [breakpointId, setBreakpointId] = useState<string | null>(null)
  const [edgeMenu, setEdgeMenu] = useState<EdgeMenuState | null>(null)
  const [wsPreviewOpen, setWSPreviewOpen] = useState(false)

  // Escape clears edge palette + edge context menu
  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        setSelectedEdgeType(null)
        setEdgeMenu(null)
        setAttachingFrom(null)
      }
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [setSelectedEdgeType, setAttachingFrom])

  // Ref keeps latest nodes + palette type for isValidConnection (no re-render dep)
  const nodesRef = useRef(nodes)
  nodesRef.current = nodes
  const paletteRef = useRef(selectedEdgeType)
  paletteRef.current = selectedEdgeType

  const isValidConnection = useCallback((connection: Edge | Connection) => {
    const pt = paletteRef.current
    if (!pt) return true // no palette → allow (legacy behavior)
    const srcNode = nodesRef.current.find((n) => n.id === connection.source)
    return isPaletteConnectionValid(srcNode?.type, pt)
  }, [])

  const triggerIds = useMemo(
    () => new Set(nodes.filter((n) => n.type === 'trigger').map((n) => n.id)),
    [nodes],
  )
  const bodyIds = useMemo(() => getForEachBodyIds(nodes, edges), [nodes, edges])

  const hasWSTrigger = useMemo(
    () => nodes.some((n) => n.type === 'trigger' && ['polymarket_ws', 'schwab_ws'].includes(n.data?.trigger_type as string)),
    [nodes],
  )

  // Recompute styles from sourceHandle + selected every render
  const styledEdges = useMemo(
    () => edges.map((e) => applyEdgeStyle(e, triggerIds, bodyIds)),
    [edges, triggerIds, bodyIds],
  )

  // Pass nodes through — breakpoint indicator is now inside node via BreakpointMarker
  const displayNodes = nodes

  const onConnect = useCallback(
    (connection: Connection) => {
      // Guard: validate palette type vs source node
      if (selectedEdgeType) {
        const srcNode = nodes.find((n) => n.id === connection.source)
        if (!isPaletteConnectionValid(srcNode?.type, selectedEdgeType)) return
      }

      const edge: Connection & { id: string; sourceHandle?: string | null; data?: Record<string, unknown> } = {
        ...connection,
        id: `e-${Date.now()}`,
      }
      // All handles now live in data.handles — resolve paletteType from there
      const srcNode = nodes.find((n) => n.id === connection.source)
      const dh = ((srcNode?.data?.handles as any[]) ?? []).find(
        (h: any) => h.id === connection.sourceHandle,
      )
      if (dh?.paletteType) {
        edge.data = { paletteType: dh.paletteType }
      } else if (selectedEdgeType) {
        edge.sourceHandle = selectedEdgeType === 'start' ? '' : selectedEdgeType
        edge.data = { paletteType: selectedEdgeType }
      }
      setEdges((eds) => addEdge(edge, eds as any))
    },
    [setEdges, selectedEdgeType, nodes],
  )

  const onDragOver = useCallback((e: React.DragEvent) => {
    e.preventDefault()
    e.dataTransfer.dropEffect = 'move'
  }, [])

  const onDrop = useCallback(
    (e: React.DragEvent) => {
      e.preventDefault()
      const nodeType = e.dataTransfer.getData('application/workflow-node-type')
      if (!nodeType || !reactFlowWrapper.current) return

      const bounds = reactFlowWrapper.current.getBoundingClientRect()
      const position = {
        x: e.clientX - bounds.left - 130,
        y: e.clientY - bounds.top - 30,
      }

      const newNode: Node = {
        id: nextNodeId(),
        type: nodeType,
        position,
        data: { ...(defaultNodeData[nodeType] ?? {}) },
      }
      setNodes((nds) => [...nds, newNode])
    },
    [setNodes],
  )

  const onEdgeDoubleClick = useCallback(
    (_: React.MouseEvent, edge: Edge) => {
      setEdges((eds) =>
        eds.map((e) => {
          if (e.id !== edge.id) return e
          const cur = (e.data?.routing as string) || 'default'
          const idx = routingCycle.indexOf(cur as (typeof routingCycle)[number])
          const next = routingCycle[(idx + 1) % routingCycle.length]
          return { ...e, data: { ...e.data, routing: next } }
        }),
      )
    },
    [setEdges],
  )

  const onEdgeContextMenu = useCallback(
    (e: React.MouseEvent, edge: Edge) => {
      e.preventDefault()
      const flow = screenToFlowPosition({ x: e.clientX, y: e.clientY })
      setEdgeMenu({
        edgeId: edge.id,
        screenX: e.clientX,
        screenY: e.clientY,
        flowX: flow.x,
        flowY: flow.y,
      })
    },
    [screenToFlowPosition],
  )

  const onPaneClick = useCallback(() => {
    setEdgeMenu(null)
    setAttachingFrom(null)
  }, [setAttachingFrom])

  function handleAddWaypoint() {
    if (!edgeMenu) return
    setEdges((eds) =>
      eds.map((e) => {
        if (e.id !== edgeMenu.edgeId) return e
        const wps = [...((e.data?.waypoints as Array<{ x: number; y: number }>) ?? [])]
        wps.push({ x: edgeMenu.flowX, y: edgeMenu.flowY })
        return { ...e, data: { ...e.data, waypoints: wps } }
      }),
    )
    setEdgeMenu(null)
  }

  function handleDeleteEdge() {
    if (!edgeMenu) return
    setEdges((eds) => eds.filter((e) => e.id !== edgeMenu.edgeId))
    setEdgeMenu(null)
  }

  const toggleBreakpoint = useCallback(
    (nodeId: string) => {
      setBreakpointId((prev) => (prev === nodeId ? null : nodeId))
    },
    [],
  )

  function handleSave() {
    updateActiveGraph(nodes, edges, params)
    onSave(nodes, edges, params)
  }

  function toggleDebugMode() {
    if (debugMode) {
      setBreakpointId(null)
    } else {
      onClearRun() // entering debug mode — stale full-run results would be misleading
    }
    setDebugMode((v) => !v)
  }

  return (
    <DebugContext.Provider value={{ debugMode, breakpointId, toggleBreakpoint }}>
    <RunResultsContext.Provider value={lastRun ?? null}>
      <div className="w-full h-full flex flex-col">
      <div ref={reactFlowWrapper} className={`flex-1 relative ${selectedEdgeType ? 'cursor-crosshair' : ''}`}>
        <div className="absolute top-3 left-3 z-10 flex flex-col gap-2">
          <WorkflowParamsPanel params={params} onChange={setParams} />
          <WorkflowHelpLegend />
        </div>

        {selectedEdgeType && (
          <div className="absolute top-3 left-1/2 -translate-x-1/2 z-10 text-xs bg-accent/80 text-accent-foreground px-3 py-1.5 rounded-md border border-border pointer-events-none">
            Drawing <span className="font-semibold">{selectedEdgeType}</span> edges — <kbd className="text-[10px] bg-muted px-1 rounded">Esc</kbd> to cancel
          </div>
        )}

        {debugMode && (
          <div className="absolute top-3 left-1/2 -translate-x-1/2 z-10 text-xs bg-red-900/60 text-red-200 px-3 py-1.5 rounded-md border border-red-700 pointer-events-none">
            {breakpointId
              ? 'Breakpoint set — click Run ↓ to run to this node'
              : 'Click a node to set breakpoint'}
          </div>
        )}

        <ReactFlow
          nodes={displayNodes}
          edges={styledEdges}
          onNodesChange={onNodesChange}
          onEdgesChange={onEdgesChange}
          onConnect={onConnect}
          isValidConnection={isValidConnection}
          onDragOver={onDragOver}
          onDrop={onDrop}
          onEdgeDoubleClick={onEdgeDoubleClick}
          onEdgeContextMenu={onEdgeContextMenu}
          onPaneClick={onPaneClick}
          nodeTypes={nodeTypes}
          edgeTypes={edgeTypes}
          colorMode="dark"
          fitView
          className="bg-background"
        >
          <Background />
          <Controls />
          <MiniMap />
        </ReactFlow>

        <div className="absolute top-3 right-3 z-10 flex gap-2">
          <button
            onClick={toggleDebugMode}
            className={`rounded-md px-3 py-1.5 text-sm font-medium transition-colors ${
              debugMode
                ? 'bg-red-600 text-white hover:bg-red-700'
                : 'bg-muted text-muted-foreground hover:bg-muted/80'
            }`}
          >
            {debugMode ? 'Exit Debug' : 'Debug'}
          </button>
          {hasWSTrigger && !debugMode && (
            <button
              onClick={() => setWSPreviewOpen((v) => !v)}
              className={`rounded-md px-3 py-1.5 text-sm font-medium transition-colors ${
                wsPreviewOpen
                  ? 'bg-cyan-600 text-white hover:bg-cyan-700'
                  : 'bg-muted text-muted-foreground hover:bg-muted/80'
              }`}
            >
              {wsPreviewOpen ? 'Close Preview' : 'Live Preview'}
            </button>
          )}
          {debugMode && breakpointId && (
            <button
              onClick={() => { updateActiveGraph(nodes, edges, params); onRun(breakpointId) }}
              className="rounded-md bg-orange-600 text-white px-3 py-1.5 text-sm font-medium hover:bg-orange-700 transition-colors"
            >
              Run ↓
            </button>
          )}
          {lastRun && Object.keys(lastRun).length > 0 && (
            <button
              onClick={() => navigator.clipboard.writeText(JSON.stringify(lastRun, null, 2))}
              className="rounded-md bg-muted text-muted-foreground px-3 py-1.5 text-sm font-medium hover:bg-muted/80 transition-colors"
            >
              Copy Run
            </button>
          )}
          <button
            onClick={handleSave}
            className="rounded-md bg-primary text-primary-foreground px-3 py-1.5 text-sm font-medium hover:bg-primary/90 transition-colors"
          >
            Save
          </button>
          {!debugMode && (
            <button
              onClick={() => { updateActiveGraph(nodes, edges, params); onRun() }}
              className="rounded-md bg-green-700 text-white px-3 py-1.5 text-sm font-medium hover:bg-green-800 transition-colors"
            >
              Run
            </button>
          )}
        </div>
        {/* edge right-click context menu */}
        {edgeMenu && (
          <>
            <div className="fixed inset-0 z-40" onClick={() => setEdgeMenu(null)} />
            <div
              className="fixed z-50 bg-popover border border-border rounded-md shadow-lg py-1 min-w-[140px]"
              style={{ left: edgeMenu.screenX, top: edgeMenu.screenY }}
            >
              <button
                className="w-full text-left px-3 py-1.5 text-sm hover:bg-accent hover:text-accent-foreground transition-colors"
                onClick={handleAddWaypoint}
              >
                Add Waypoint
              </button>
              <button
                className="w-full text-left px-3 py-1.5 text-sm text-red-400 hover:bg-accent hover:text-red-300 transition-colors"
                onClick={handleDeleteEdge}
              >
                Delete Edge
              </button>
            </div>
          </>
        )}
      </div>
      {wsPreviewOpen && (
        <WSPreviewPanel
          workflowId={workflow.id}
          onRunWithInput={async (input) => onRun(
            debugMode && breakpointId ? breakpointId : undefined,
            input,
          )}
          onClose={() => setWSPreviewOpen(false)}
        />
      )}
      </div>
    </RunResultsContext.Provider>
    </DebugContext.Provider>
  )
}

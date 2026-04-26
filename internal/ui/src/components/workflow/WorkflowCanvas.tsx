import { useCallback, useMemo, useRef, useState } from 'react'
import {
  ReactFlow,
  Background,
  Controls,
  MiniMap,
  addEdge,
  useNodesState,
  useEdgesState,
  type Connection,
  type Node,
  type Edge,
  type NodeTypes,
} from '@xyflow/react'
import { TriggerNode } from './nodes/TriggerNode'
import { HTTPFetchNode } from './nodes/HTTPFetchNode'
import { JSTransformNode } from './nodes/JSTransformNode'
import { ForEachNode } from './nodes/ForEachNode'
import { MongoUpsertNode } from './nodes/MongoUpsertNode'
import { RedisPublishNode } from './nodes/RedisPublishNode'
import { NotifyNode } from './nodes/NotifyNode'
import { useWorkflowStore, type Workflow } from './useWorkflowStore'
import { WorkflowParamsPanel } from './WorkflowParamsPanel'
import { WorkflowHelpLegend } from './WorkflowHelpLegend'
import { RunResultsContext, type RunResults } from './RunResultsContext'

const nodeTypes: NodeTypes = {
  trigger: TriggerNode,
  http_fetch: HTTPFetchNode,
  js_transform: JSTransformNode,
  for_each: ForEachNode,
  mongo_upsert: MongoUpsertNode,
  redis_publish: RedisPublishNode,
  notify: NotifyNode,
}

const defaultNodeData: Record<string, Record<string, unknown>> = {
  trigger: { name: '' },
  http_fetch: { url: '', name: '' },
  js_transform: { script: '', name: '' },
  for_each: { name: '' },
  mongo_upsert: { collection: '', filter_field: '', name: '' },
  redis_publish: { channel: '', name: '' },
  notify: { message: '', name: '' },
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
    if (forEachIds.has(e.source) && e.sourceHandle === 'item') {
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
 *  error                  → red
 *  item                   → violet
 *  trigger source         → blue  "start"
 *  body success (in body) → dashed violet  "success (item)"
 *  default/success        → green "success"
 */
function applyEdgeStyle(
  edge: Edge,
  triggerIds: Set<string>,
  bodyIds: Set<string>,
): Edge {
  const h = (edge.sourceHandle ?? '').toLowerCase()
  const sel = edge.selected ?? false

  if (h === 'error') {
    return {
      ...edge,
      style: { stroke: sel ? '#fca5a5' : '#ef4444' },
      label: edge.label ?? 'error',
    }
  }

  if (h === 'item') {
    return {
      ...edge,
      style: { stroke: sel ? '#ddd6fe' : '#a78bfa' },
      label: edge.label ?? 'item',
    }
  }

  if (triggerIds.has(edge.source)) {
    return {
      ...edge,
      style: { stroke: sel ? '#93c5fd' : '#3b82f6' },
      label: edge.label ?? 'start',
    }
  }

  if (bodyIds.has(edge.source)) {
    return {
      ...edge,
      style: { stroke: sel ? '#ddd6fe' : '#a78bfa', strokeDasharray: '8 4' },
      label: edge.label ?? 'success (item)',
    }
  }

  return {
    ...edge,
    style: { stroke: sel ? '#86efac' : '#22c55e' },
    label: edge.label ?? 'success',
  }
}

interface Props {
  workflow: Workflow
  onSave(nodes: Node[], edges: Edge[], params: Record<string, string>): void
  onRun(stopAt?: string): void
  onClearRun(): void
  lastRun?: RunResults
}

let nodeIdCounter = Date.now()
function nextNodeId() {
  return `n-${++nodeIdCounter}`
}

export function WorkflowCanvas({ workflow, onSave, onRun, onClearRun, lastRun }: Props) {
  const { updateActiveGraph } = useWorkflowStore()
  const [nodes, setNodes, onNodesChange] = useNodesState(workflow.nodes)
  const [edges, setEdges, onEdgesChange] = useEdgesState(workflow.edges)
  const [params, setParams] = useState<Record<string, string>>(workflow.params ?? {})
  const reactFlowWrapper = useRef<HTMLDivElement>(null)
  const [debugMode, setDebugMode] = useState(false)
  const [breakpointId, setBreakpointId] = useState<string | null>(null)

  const triggerIds = useMemo(
    () => new Set(nodes.filter((n) => n.type === 'trigger').map((n) => n.id)),
    [nodes],
  )
  const bodyIds = useMemo(() => getForEachBodyIds(nodes, edges), [nodes, edges])

  // Recompute styles from sourceHandle + selected every render
  const styledEdges = useMemo(
    () => edges.map((e) => applyEdgeStyle(e, triggerIds, bodyIds)),
    [edges, triggerIds, bodyIds],
  )

  // Overlay breakpoint outline without mutating saved node state
  const displayNodes = useMemo(
    () =>
      nodes.map((n) => ({
        ...n,
        style:
          n.id === breakpointId
            ? { ...n.style, outline: '2px solid #ef4444', outlineOffset: '2px', borderRadius: '8px' }
            : n.style,
      })),
    [nodes, breakpointId],
  )

  const onConnect = useCallback(
    (connection: Connection) => {
      setEdges((eds) => addEdge({ ...connection, id: `e-${Date.now()}` }, eds))
    },
    [setEdges],
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

  const onNodeClick = useCallback(
    (_: React.MouseEvent, node: Node) => {
      if (!debugMode) return
      setBreakpointId((prev) => (prev === node.id ? null : node.id))
    },
    [debugMode],
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
    <RunResultsContext.Provider value={lastRun ?? null}>
      <div ref={reactFlowWrapper} className="w-full h-full relative">
        <div className="absolute top-3 left-3 z-10 flex flex-col gap-2">
          <WorkflowParamsPanel params={params} onChange={setParams} />
          <WorkflowHelpLegend />
        </div>

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
          onDragOver={onDragOver}
          onDrop={onDrop}
          onNodeClick={onNodeClick}
          nodeTypes={nodeTypes}
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
          {debugMode && breakpointId && (
            <button
              onClick={() => onRun(breakpointId)}
              className="rounded-md bg-orange-600 text-white px-3 py-1.5 text-sm font-medium hover:bg-orange-700 transition-colors"
            >
              Run ↓
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
              onClick={() => onRun()}
              className="rounded-md bg-green-700 text-white px-3 py-1.5 text-sm font-medium hover:bg-green-800 transition-colors"
            >
              Run
            </button>
          )}
        </div>
      </div>
    </RunResultsContext.Provider>
  )
}

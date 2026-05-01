import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Play, Square, Terminal, Bug } from 'lucide-react'
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
import { HTTPRequestNode } from './nodes/HTTPRequestNode'
import { ForEachNode } from './nodes/ForEachNode'
import { MongoRequestNode } from './nodes/MongoRequestNode'
import { RedisRequestNode } from './nodes/RedisRequestNode'
import { NotifyNode } from './nodes/NotifyNode'
import { SandboxScriptNode } from './nodes/SandboxScriptNode'
import { AIAgentNode } from './nodes/AIAgentNode'
import { useWorkflowStore, type Workflow } from './useWorkflowStore'
import { WorkflowParamsPanel } from './WorkflowParamsPanel'
import { WorkflowCostLimitsPanel } from './WorkflowCostLimitsPanel'
import { WorkflowApprovalChannelPanel } from './WorkflowApprovalChannelPanel'
import { WorkflowHelpLegend } from './WorkflowHelpLegend'
import { RunResultsContext, RunStatusContext, AgentRunContext, DebugContext, ToolApprovalContext, type RunResults, type AgentIterSummary } from './RunResultsContext'

const nodeTypes: NodeTypes = {
  trigger: TriggerNode,
  http_request: HTTPRequestNode,
  for_each: ForEachNode,
  mongo_request: MongoRequestNode,
  redis_request: RedisRequestNode,
  notify: NotifyNode,
  sandbox_script: SandboxScriptNode,
  ai_agent: AIAgentNode,
}

const edgeTypes: EdgeTypes = {
  default: WaypointEdge,
  smoothstep: WaypointEdge,
  step: WaypointEdge,
  straight: WaypointEdge,
}

const defaultNodeData: Record<string, Record<string, unknown>> = {
  trigger: { trigger_type: 'manual', name: '' },
  http_request: {
    url: '',
    method: 'GET',
    name: '',
    timeout_seconds: 30,
    follow_redirects: true,
    max_redirects: 10,
    parse_json: false,
    accept_any_status: false,
    max_response_bytes: 10485760,
  },
  for_each: { name: '' },
  mongo_request: { collection: '', operation: 'find', batch_size: 100, name: '' },
  redis_request: { operation: 'publish', channel: '', name: '' },
  notify: { message: '', name: '' },
  sandbox_script: { script: '', language: 'javascript', timeout: 30, mem_limit: 128, cpu_limit: 0.5, network: false, name: '' },
  ai_agent: {
    name: '',
    system_prompt: 'You are a helpful AI assistant. Use the available tools to accomplish the task.',
    user_input: '',
    llm_connection_id: '',
    model_override: '',
    memory_session_id: '',
    max_iterations: 8,
    max_tool_calls_per_iter: 5,
    max_tokens: 4096,
    temperature: 1,
    timeout_seconds: 300,
    require_approval: false,
    output_schema: '',
    skills: [],
  },
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
  tool:    { normal: '#c084fc', selected: '#e9d5ff' },
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
  onRun(stopAt?: string | string[], input?: unknown): void
  onCancel?: () => void
  onContinue?: () => void
  onClearRun(): void
  lastRun?: RunResults
  // True when at least one node is paused on a live pre-exec breakpoint.
  // Used to flip the running state's red Cancel into a green Continue
  // (the run is *technically* in flight from the server's perspective —
  // it's blocked on continueCh — but the user wants to advance, not abort).
  hasLivePause?: boolean
  // True while a workflow run is in flight. Threaded into RunStatusContext
  // so child nodes can render "idle" vs "not executed" correctly.
  runRunning?: boolean
  // Per-agent-node iter timelines from the live WS stream. Threaded into
  // AgentRunContext so AIAgentNode → AgentTimelinePanel renders without
  // a parallel WS subscription per node.
  agentRuns?: Record<string, AgentIterSummary[]>
  // Hook function to send an approve_tool decision over the live WS for
  // a paused tool call. Threaded into ToolApprovalContext so the agent
  // timeline panel renders Approve/Reject buttons. Null/undefined when
  // there's no live WS (post-run snapshot view).
  onApproveTool?: (toolId: string, approved: boolean, reason?: string) => void
  // Hook function to mirror breakpoint-set changes onto the live WS so
  // the running executor honours mid-run breakpoint toggles. No-op when
  // no live WS / not in a debug run.
  onSetBreakpoints?: (nodeIds: string[]) => void
  // Set when the most-recent run paused (a stopAt-bound tool fired inside
  // an agent's ReAct loop). Truthy flips the Run button into "Continue"
  // mode; the parent passes the run_id back via onRun on the next click.
  pausedRunID?: string | null
  // Set when an OOB approval gate fires during the live run. The
  // dispatcher (Slack / email) may still deliver a magic-link, but
  // when it fails the user has no inline path to resolve the gate
  // — surface a banner pointing at /runs/:id so they always have a
  // way out. Cleared by run_done / reset.
  pendingApprovalRunID?: string | null
  // Set when the most-recent run terminated with a pre-step error (cost
  // cap, missing config, etc) — there's no per-node step_done to attach
  // the message to, so we render a canvas-level banner instead. Cleared
  // by onClearRun.
  runError?: string | null
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

function WorkflowCanvasInner({ workflow, onSave, onRun, onCancel, onContinue, onClearRun, lastRun, runRunning, agentRuns, pausedRunID, pendingApprovalRunID, hasLivePause, runError, onApproveTool, onSetBreakpoints }: Props) {
  const { updateActiveGraph, updateActiveCostLimits, updateActiveParamsSchema, updateActiveApprovalChannel, selectedEdgeType, setSelectedEdgeType, attachingFrom, setAttachingFrom } = useWorkflowStore()
  const { screenToFlowPosition } = useReactFlow()
  const [nodes, setNodes, onNodesChange] = useNodesState(workflow.nodes)
  const [edges, setEdges, onEdgesChange] = useEdgesState(workflow.edges)
  const [params, setParams] = useState<Record<string, string>>(workflow.params ?? {})
  const reactFlowWrapper = useRef<HTMLDivElement>(null)
  const [debugMode, setDebugMode] = useState(false)
  const [breakpointIds, setBreakpointIds] = useState<Set<string>>(new Set())
  const [edgeMenu, setEdgeMenu] = useState<EdgeMenuState | null>(null)

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
      setBreakpointIds((prev) => {
        const next = new Set(prev)
        if (next.has(nodeId)) next.delete(nodeId)
        else next.add(nodeId)
        return next
      })
    },
    [],
  )

  // Mid-run breakpoint sync: whenever the breakpoint set changes during
  // a live run, push the new IDs over the WS so the executor honours
  // toggles without restarting. Skipped when no run is in flight (the
  // run kickoff already sends the set as part of the initial frame).
  useEffect(() => {
    if (!onSetBreakpoints) return
    if (!runRunning && !pausedRunID && !hasLivePause) return
    onSetBreakpoints(Array.from(breakpointIds))
  }, [breakpointIds, runRunning, pausedRunID, hasLivePause, onSetBreakpoints])

  function handleSave() {
    updateActiveGraph(nodes, edges, params)
    onSave(nodes, edges, params)
  }

  function toggleDebugMode() {
    if (debugMode) {
      setBreakpointIds(new Set())
    } else {
      onClearRun() // entering debug mode — stale full-run results would be misleading
    }
    setDebugMode((v) => !v)
  }

  return (
    <DebugContext.Provider value={{ debugMode, breakpointIds, toggleBreakpoint }}>
    <RunStatusContext.Provider value={{ running: !!runRunning }}>
    <AgentRunContext.Provider value={agentRuns ?? null}>
    <ToolApprovalContext.Provider value={onApproveTool ?? null}>
    <RunResultsContext.Provider value={lastRun ?? null}>
      <div className="w-full h-full flex flex-col">
      <div ref={reactFlowWrapper} className={`flex-1 relative ${selectedEdgeType ? 'cursor-crosshair' : ''}`}>
        <div className="absolute top-3 left-3 z-10 flex flex-col gap-2">
          <WorkflowParamsPanel
            params={params}
            onChange={setParams}
            onSave={handleSave}
            schema={workflow.params_schema}
            onSchemaChange={(s) => updateActiveParamsSchema(s)}
          />
          {/* Cost limits only matter when an AI Agent node exists — caps
              gate LLM token spend, no agent = nothing to gate. Hide the
              panel reactively as the user adds/removes ai_agent nodes so
              the toolbar doesn't carry config that has no current effect. */}
          {nodes.some((n) => n.type === 'ai_agent') && (
            <WorkflowCostLimitsPanel
              costLimits={workflow.cost_limits ?? null}
              onChange={(cl) => updateActiveCostLimits(cl)}
              onSave={handleSave}
            />
          )}
          {/* Approval channel only meaningful if at least one node is
              gated. Surface the panel reactively so workflows without
              any approval gates don't carry routing config. Reads
              `require_node_approval` (any node) and `require_approval`
              (ai_agent per-tool flag) — either qualifies. */}
          {nodes.some((n) =>
            (n.data as Record<string, unknown> | undefined)?.require_node_approval === true ||
            (n.type === 'ai_agent' && (n.data as Record<string, unknown> | undefined)?.require_approval === true),
          ) && (
            <WorkflowApprovalChannelPanel
              channel={workflow.approval_channel ?? null}
              onChange={(ch) => updateActiveApprovalChannel(ch)}
              onSave={handleSave}
            />
          )}
          <WorkflowHelpLegend />
        </div>

        {selectedEdgeType && (
          <div className="absolute top-3 left-1/2 -translate-x-1/2 z-10 text-xs bg-accent/80 text-accent-foreground px-3 py-1.5 rounded-md border border-border pointer-events-none">
            Drawing <span className="font-semibold">{selectedEdgeType}</span> edges — <kbd className="text-[10px] bg-muted px-1 rounded">Esc</kbd> to cancel
          </div>
        )}

        {debugMode && (
          <div className="absolute top-3 left-1/2 -translate-x-1/2 z-10 text-xs bg-red-900/60 text-red-200 px-3 py-1.5 rounded-md border border-red-700 pointer-events-none">
            {breakpointIds.size === 0
              ? 'Click a node to set breakpoint'
              : `${breakpointIds.size} breakpoint${breakpointIds.size === 1 ? '' : 's'} set — click Run ↓ to run; Continue advances to next`}
          </div>
        )}

        {/* Run-level error banner — surfaces failures that abort BEFORE any
            node executes (cost-cap breach, missing config, malformed
            graph). Per-node errors still attach via NodeDebugPanel; this
            slot covers the gap where step_done never fired. Sits BELOW
            the toolbar (top-16 ≈ 64px past the buttons row) so the close
            button stays clickable. */}
        {runError && !runRunning && (
          <div className="absolute top-16 right-3 z-20 max-w-[480px] text-xs bg-red-700 text-white px-3 py-2 rounded-md border border-red-300 shadow-lg flex items-start gap-2">
            <span className="font-semibold shrink-0">Run failed:</span>
            <span className="flex-1 break-words">{runError}</span>
            <button
              type="button"
              onClick={onClearRun}
              className="shrink-0 text-red-200 hover:text-white px-1 leading-none"
              title="Dismiss + clear last run"
            >
              ✕
            </button>
          </div>
        )}

        {/* Pending-approval banner — fires while the run is paused on
            a require_node_approval gate or per-tool agent gate. Even if
            the OOB dispatcher (Slack / email) succeeded, surfacing a
            direct link lets the user resolve the gate from the canvas
            without having to navigate the Runs tab manually. When the
            dispatcher FAILED (token type wrong, channel unreachable,
            etc.), this banner is the only visible recovery path. */}
        {pendingApprovalRunID && runRunning && (
          <div className="absolute top-16 right-3 z-20 max-w-[480px] text-xs bg-amber-600 text-white px-3 py-2 rounded-md border border-amber-300 shadow-lg flex items-start gap-2">
            <span className="font-semibold shrink-0">Awaiting approval:</span>
            <span className="flex-1 break-words">
              Run paused on an approval gate.{' '}
              <a
                href={`/runs/${pendingApprovalRunID}`}
                target="_blank"
                rel="noreferrer"
                className="underline font-medium hover:text-amber-100"
              >
                Open /runs/{pendingApprovalRunID.slice(0, 8)}…
              </a>{' '}
              to Approve / Reject.
            </span>
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
          {/* Run / Debug mode tabs — mirrors SandboxDebugDialog so the
              canvas toolbar speaks the same visual language as the Monaco
              debug dialog. Switching modes is disabled while a run is in
              flight to avoid an inconsistent debugger state. */}
          <div className="flex items-center text-xs rounded border border-border overflow-hidden font-medium">
            <button
              onClick={() => { if (!runRunning && debugMode) toggleDebugMode() }}
              disabled={runRunning}
              className={`px-3 py-1.5 flex items-center gap-1 transition-colors ${
                !debugMode
                  ? 'bg-green-600/20 text-green-400'
                  : 'text-muted-foreground hover:text-foreground'
              } ${runRunning ? 'opacity-50 cursor-not-allowed' : ''}`}
              title="Run mode — execute the workflow without breakpoints"
            >
              <Terminal className="h-3 w-3" /> Run
            </button>
            <button
              onClick={() => { if (!runRunning && !debugMode) toggleDebugMode() }}
              disabled={runRunning}
              className={`px-3 py-1.5 flex items-center gap-1 border-l border-border transition-colors ${
                debugMode
                  ? 'bg-red-600/20 text-red-400'
                  : 'text-muted-foreground hover:text-foreground'
              } ${runRunning ? 'opacity-50 cursor-not-allowed' : ''}`}
              title="Debug mode — set breakpoints, step through agent loop"
            >
              <Bug className="h-3 w-3" /> Debug
            </button>
          </div>
          {/* Primary run action — single slot, label switches by mode +
              run state. Always sits on the LEFT of Save so the eye finds
              the green action button in the same place across modes. */}
          {runRunning && hasLivePause && onContinue && (
            <button
              onClick={onContinue}
              className="flex items-center gap-1.5 rounded-md px-3 py-1.5 text-sm font-medium text-white bg-green-600 hover:bg-green-700 transition-colors"
              title="Release the pre-exec breakpoint and let the next node fire"
            >
              <Play className="h-3.5 w-3.5" /> Continue
            </button>
          )}
          {runRunning && onCancel && (
            <button
              onClick={onCancel}
              className="flex items-center gap-1.5 rounded-md bg-red-700 text-white px-3 py-1.5 text-sm font-medium hover:bg-red-800 transition-colors"
              title="Cancel the in-flight run (closes the WS connection; server cancels via context propagation)"
            >
              <Square className="h-3.5 w-3.5" /> Cancel
            </button>
          )}
          {!runRunning && pausedRunID && (
            <>
              <button
                onClick={() => { updateActiveGraph(nodes, edges, params); onRun() }}
                className="flex items-center gap-1.5 rounded-md px-3 py-1.5 text-sm font-medium text-white bg-green-600 hover:bg-green-700 transition-colors"
                title={`Resume paused run ${pausedRunID.slice(0, 8)}…`}
              >
                <Play className="h-3.5 w-3.5" /> Continue
              </button>
              <button
                onClick={onClearRun}
                className="flex items-center gap-1.5 rounded-md bg-red-700 text-white px-3 py-1.5 text-sm font-medium hover:bg-red-800 transition-colors"
                title="Discard the paused run; next click of Run starts fresh from the trigger"
              >
                <Square className="h-3.5 w-3.5" /> Cancel
              </button>
            </>
          )}
          {debugMode && !runRunning && !pausedRunID && (
            <button
              onClick={() => {
                updateActiveGraph(nodes, edges, params)
                const ids = Array.from(breakpointIds)
                onRun(ids.length > 0 ? ids : undefined)
              }}
              className="flex items-center gap-1.5 px-3 py-1.5 rounded bg-green-600 hover:bg-green-700 text-white text-xs font-medium transition-colors"
              title={breakpointIds.size > 0 ? 'Run to first breakpoint' : 'Debug run'}
            >
              <Play className="h-3.5 w-3.5" /> {breakpointIds.size > 0 ? 'Debug ↓' : 'Debug'}
            </button>
          )}
          {!debugMode && !runRunning && !pausedRunID && (
            <button
              onClick={() => { updateActiveGraph(nodes, edges, params); onRun() }}
              className="flex items-center gap-1.5 px-3 py-1.5 rounded bg-green-600 hover:bg-green-700 text-white text-xs font-medium transition-colors"
              title="Run workflow"
            >
              <Play className="h-3.5 w-3.5" /> Run
            </button>
          )}
          {/* Status dot strip — mirrors the SandboxDebugDialog convention so
              users see a single visual language across the workflow canvas
              and the Monaco debug dialog. */}
          <div className="flex items-center gap-1.5">
            <span
              className={`inline-block h-2 w-2 rounded-full ${
                pausedRunID || hasLivePause
                  ? 'bg-yellow-400'
                  : runRunning
                  ? 'bg-green-400 animate-pulse'
                  : 'bg-gray-500'
              }`}
            />
            <span className="text-xs text-muted-foreground">
              {pausedRunID || hasLivePause ? 'paused' : runRunning ? 'running' : 'idle'}
            </span>
          </div>
          {lastRun && Object.keys(lastRun).length > 0 && (
            <button
              onClick={() => navigator.clipboard.writeText(JSON.stringify(lastRun, null, 2))}
              className="rounded-md bg-muted text-muted-foreground px-3 py-1.5 text-sm font-medium hover:bg-muted/80 transition-colors"
            >
              Copy Run
            </button>
          )}
          <RunCostTotal agentRuns={agentRuns} />

          <button
            onClick={handleSave}
            className="rounded-md bg-primary text-primary-foreground px-3 py-1.5 text-sm font-medium hover:bg-primary/90 transition-colors"
          >
            Save
          </button>
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
      </div>
    </RunResultsContext.Provider>
    </ToolApprovalContext.Provider>
    </AgentRunContext.Provider>
    </RunStatusContext.Provider>
    </DebugContext.Provider>
  )
}

// RunCostTotal sums tokens + cost across every agent node's iters and
// renders a compact toolbar badge. Hidden when no agent has emitted a
// usage event yet (idle / non-agent runs). Useful as a quick "is this
// run affordable" signal next to the Run/Cancel button.
function RunCostTotal({ agentRuns }: { agentRuns?: Record<string, AgentIterSummary[]> }) {
  if (!agentRuns) return null
  let inTok = 0
  let outTok = 0
  let cost = 0
  for (const id of Object.keys(agentRuns)) {
    for (const iter of agentRuns[id]!) {
      inTok += iter.llm?.usage?.input_tokens ?? 0
      outTok += iter.llm?.usage?.output_tokens ?? 0
      cost += iter.llm?.usage?.cost_usd ?? 0
    }
  }
  if (inTok === 0 && outTok === 0 && cost === 0) return null
  return (
    <div className="flex items-center gap-2 px-3 py-1.5 rounded-md bg-muted/40 text-[11px] text-muted-foreground tabular-nums">
      <span title="input → output tokens (sum across all agent nodes)">
        {inTok.toLocaleString()}/{outTok.toLocaleString()} tok
      </span>
      {cost > 0 && (
        <span className="text-foreground font-medium" title="run cost estimate (provider pricing table)">
          ${cost.toFixed(4)}
        </span>
      )}
    </div>
  )
}

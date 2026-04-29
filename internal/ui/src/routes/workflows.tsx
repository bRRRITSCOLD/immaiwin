import { createFileRoute, Link } from '@tanstack/react-router'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { toast } from 'sonner'
import { Separator } from '~/components/ui/separator'
import { WorkflowSidebar } from '~/components/workflow/WorkflowSidebar'
import { WorkflowCanvas } from '~/components/workflow/WorkflowCanvas'
import { useWorkflowStore, type Workflow, type Connection } from '~/components/workflow/useWorkflowStore'
import type { RunResults, AgentIterSummary } from '~/components/workflow/RunResultsContext'
import type { Node, Edge } from '@xyflow/react'
import { useQueryState } from '~/hooks/useQueryState'
import { useWorkflowRunStream } from '~/hooks/useWorkflowRunStream'

export const Route = createFileRoute('/workflows')({
  component: WorkflowsPage,
})

const API_BASE = import.meta.env['VITE_API_URL'] ?? 'http://localhost:8080'

function WorkflowsPage() {
  const { workflows, activeId, setWorkflows, setConnections, setActive, activeWorkflow } = useWorkflowStore()
  const [lastRun, setLastRun] = useState<RunResults | null>(null)
  const [qsWorkflow, setQsWorkflow] = useQueryState('workflow')
  const stream = useWorkflowRunStream()

  // Mirror the live stream's per-node state into the existing RunResults
  // shape so NodeDebugPanel keeps working untouched. Done as a derived
  // memo so each event tick re-renders the canvas.
  const liveRun = useMemo<RunResults | null>(() => {
    const ids = Object.keys(stream.nodes)
    if (ids.length === 0) return null
    const out: RunResults = {}
    for (const id of ids) {
      const n = stream.nodes[id]!
      // Map NodeRunState.status → StepResult.status so NodeDebugPanel
      // can show a live "running" indicator until the corresponding
      // step_done event lands. Pending nodes (only step_start seen) read
      // as 'running'; that matches user expectation that a node which
      // has begun but not completed should NOT show green.
      let status: 'running' | 'done' | 'error' | 'paused' | 'cancelled' | undefined
      if (n.status === 'pending' || n.status === 'running') {
        status = 'running'
      } else if (n.status === 'error') {
        status = 'error'
      } else if (n.status === 'paused') {
        status = 'paused'
      } else if (n.status === 'cancelled') {
        status = 'cancelled'
      } else if (n.status === 'done') {
        status = 'done'
      }
      out[id] = [
        {
          node_id: n.nodeId,
          node_type: n.nodeType ?? '',
          output: n.output,
          error: n.error,
          status,
        },
      ]
    }
    return out
  }, [stream.nodes])

  // When a stream is active, prefer its live snapshot; otherwise fall
  // back to whatever the last completed run produced.
  const displayedRun = stream.status === 'idle' ? lastRun : liveRun

  // Per-agent-node iter timelines. Surfaces the ReAct breakdown to the
  // canvas via AgentRunContext (read by AgentTimelinePanel). Only
  // populated for nodes the agent loop actually drove (iters non-empty).
  const agentRuns = useMemo<Record<string, AgentIterSummary[]>>(() => {
    const out: Record<string, AgentIterSummary[]> = {}
    for (const id of Object.keys(stream.nodes)) {
      const n = stream.nodes[id]!
      if (n.iters.length > 0) out[id] = n.iters
    }
    return out
  }, [stream.nodes])

  // On `run_done`, freeze the live snapshot into lastRun and let the
  // stream reset (next run starts from a clean slate). Toast errors
  // surfaced via stream.events.
  useEffect(() => {
    if (stream.status !== 'done' && stream.status !== 'error') return
    setLastRun(liveRun)
    let hasError = false
    // Cost cap fires its own dedicated `cost_exceeded` event AND the
    // executor follows up with a step_done(is_error=true) carrying the
    // identical message. Toasting both causes two near-duplicate red
    // notifications. Track whether a cost_exceeded landed and suppress
    // the matching step_done toast — keep the dedicated one because it
    // has the friendlier "Cost cap exceeded — ..." prefix.
    const costExceededSeen = new Set<string>()
    for (const ev of stream.events) {
      if (ev.type === 'cost_exceeded' && ev.error) {
        costExceededSeen.add(ev.error)
      }
    }
    for (const ev of stream.events) {
      if (ev.type === 'step_done' && ev.is_error) {
        if (ev.error && costExceededSeen.has(ev.error)) {
          hasError = true
          continue
        }
        toast.error(`[${ev.node_type}] ${ev.error ?? 'error'}`, { duration: 8000 })
        hasError = true
      }
      if (ev.type === 'error') {
        if (ev.error && costExceededSeen.has(ev.error)) {
          hasError = true
          continue
        }
        toast.error(ev.error ?? 'run error', { duration: 8000 })
        hasError = true
      }
      if (ev.type === 'cost_exceeded') {
        toast.error(`Cost cap exceeded — ${ev.error ?? 'see logs'}`, { duration: 10000 })
        hasError = true
      }
    }
    if (!hasError && stream.status === 'done') {
      toast.success('Workflow completed')
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [stream.status])

  // Single function updates both store + URL atomically — no ping-pong
  const selectWorkflow = useCallback(
    (id: string | null) => {
      // Switching workflows wipes the live stream + last-run snapshot —
      // toolbar cost meter / per-node iter timelines were keyed on the
      // previous workflow's nodes; carrying them across is misleading.
      stream.reset()
      setLastRun(null)
      setActive(id)
      setQsWorkflow(id)
    },
    // Stream's reset is stable (useCallback inside the hook).
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [setActive, setQsWorkflow],
  )

  const load = useCallback(async () => {
    try {
      const [wfRes, connRes] = await Promise.all([
        fetch(`${API_BASE}/api/v1/workflows`),
        fetch(`${API_BASE}/api/v1/connections`),
      ])
      const wfs: Workflow[] = await wfRes.json()
      const conns: Connection[] = await connRes.json()
      setWorkflows(wfs)
      setConnections(conns)
    } catch {
      toast.error('Failed to load workflows')
    }
  }, [setWorkflows, setConnections])

  useEffect(() => {
    load()
  }, [load])

  // Restore from URL on load, or auto-select first
  useEffect(() => {
    if (workflows.length === 0) return
    if (qsWorkflow && workflows.some((w) => w.id === qsWorkflow)) {
      if (activeId !== qsWorkflow) setActive(qsWorkflow)
    } else if (!activeId) {
      selectWorkflow(workflows[0]!.id)
    }
    // Only run on workflows load — not on activeId/qsWorkflow changes
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [workflows])

  async function handleSave(nodes: Node[], edges: Edge[], params: Record<string, string>) {
    const wf = activeWorkflow()
    if (!wf) return
    try {
      const res = await fetch(`${API_BASE}/api/v1/workflows/${wf.id}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ...wf, nodes, edges, params }),
      })
      if (!res.ok) {
        const data = await res.json()
        toast.error(`Save failed: ${data.error}`)
      } else {
        toast.success('Workflow saved')
      }
    } catch {
      toast.error('Network error saving workflow')
    }
  }

  async function handleRun(stopAt?: string, input?: unknown) {
    const wf = activeWorkflow()
    if (!wf) return

    // Auto-save before run so the server has the latest graph. Streaming
    // run won't pick up unsaved canvas changes either, so the auto-save
    // step is unchanged from the legacy POST flow.
    try {
      const saveRes = await fetch(`${API_BASE}/api/v1/workflows/${wf.id}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(wf),
      })
      if (!saveRes.ok) {
        const data = await saveRes.json()
        toast.error(`Auto-save failed: ${data.error}`)
        return
      }
    } catch {
      toast.error('Network error auto-saving before run')
      return
    }

    // Hand off to the WS stream — the hook resets prior state internally.
    // When the previous run paused, automatically pass its run_id so the
    // server resumes mid-conversation instead of restarting from trigger.
    // Selecting a different stopAt (or none) still goes through this same
    // path; the executor either resumes the saved agent and skips other
    // completed nodes, or — if the user wants a clean restart — they can
    // hit "Clear Run" first.
    setLastRun(null)
    const resumeRunID = stream.pausedRunID ?? undefined
    stream.run(wf.id, input, stopAt, resumeRunID)
  }

  const active = activeWorkflow()

  return (
    <div className="h-screen overflow-hidden bg-background text-foreground flex flex-col">
      <header className="sticky top-0 z-10 border-b bg-background/90 backdrop-blur-sm px-6 py-3 flex items-center gap-4 shrink-0">
        <h1 className="text-lg font-semibold tracking-tight">immaiwin</h1>
        <Separator orientation="vertical" className="h-5" />
        <nav className="flex items-center gap-3 text-sm">
          <Link to="/" className="text-muted-foreground hover:text-foreground transition-colors">Polymarket</Link>
          <Link to="/workflows" className="text-foreground font-medium">Workflows</Link>
          <Link to="/runs" className="text-muted-foreground hover:text-foreground transition-colors">Runs</Link>
          <Link to="/evals" className="text-muted-foreground hover:text-foreground transition-colors">Evals</Link>
        </nav>
        {active && (
          <div className="ml-auto flex items-center gap-2">
            <span className="text-sm text-muted-foreground">{active.name}</span>
          </div>
        )}
      </header>

      <div className="flex flex-1 overflow-hidden">
        <WorkflowSidebar onSelect={selectWorkflow} onReload={load} />
        <main className="flex-1 overflow-hidden h-full">
          {active ? (
            <WorkflowCanvas
              key={active.id}
              workflow={active}
              onSave={handleSave}
              onRun={handleRun}
              onCancel={() => stream.cancel()}
              onContinue={() => {
                // Live pre-exec pause has the highest priority: a still-
                // open WS lets us release the in-process breakpoint with
                // one frame, no full re-run. Falls back to the persisted-
                // resume path when no live connection (process restart,
                // tab refresh, etc.).
                if (!stream.continue_()) {
                  handleRun()
                }
              }}
              onClearRun={() => { stream.reset(); setLastRun(null) }}
              lastRun={displayedRun ?? undefined}
              runRunning={stream.status === 'connecting' || stream.status === 'running'}
              hasLivePause={Object.values(stream.nodes).some((n) => n.status === 'paused')}
              agentRuns={agentRuns}
              pausedRunID={stream.pausedRunID}
              runError={stream.error}
            />
          ) : (
            <div className="flex h-full items-center justify-center text-muted-foreground text-sm">
              Select a workflow to view its canvas
            </div>
          )}
        </main>
      </div>
    </div>
  )
}

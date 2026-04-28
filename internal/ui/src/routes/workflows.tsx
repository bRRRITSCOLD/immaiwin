import { createFileRoute, Link } from '@tanstack/react-router'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { toast } from 'sonner'
import { Separator } from '~/components/ui/separator'
import { WorkflowSidebar } from '~/components/workflow/WorkflowSidebar'
import { WorkflowCanvas } from '~/components/workflow/WorkflowCanvas'
import { useWorkflowStore, type Workflow, type Connection } from '~/components/workflow/useWorkflowStore'
import type { RunResults } from '~/components/workflow/RunResultsContext'
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
      let status: 'running' | 'done' | 'error' | undefined
      if (n.status === 'pending' || n.status === 'running') {
        status = 'running'
      } else if (n.status === 'error') {
        status = 'error'
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

  // On `run_done`, freeze the live snapshot into lastRun and let the
  // stream reset (next run starts from a clean slate). Toast errors
  // surfaced via stream.events.
  useEffect(() => {
    if (stream.status !== 'done' && stream.status !== 'error') return
    setLastRun(liveRun)
    let hasError = false
    for (const ev of stream.events) {
      if (ev.type === 'step_done' && ev.is_error) {
        toast.error(`[${ev.node_type}] ${ev.error ?? 'error'}`)
        hasError = true
      }
      if (ev.type === 'error') {
        toast.error(ev.error ?? 'run error')
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
      setActive(id)
      setQsWorkflow(id)
    },
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
    setLastRun(null)
    stream.run(wf.id, input, stopAt)
  }

  const active = activeWorkflow()

  return (
    <div className="h-screen overflow-hidden bg-background text-foreground flex flex-col">
      <header className="sticky top-0 z-10 border-b bg-background/90 backdrop-blur-sm px-6 py-3 flex items-center gap-4 shrink-0">
        <h1 className="text-lg font-semibold tracking-tight">immaiwin</h1>
        <Separator orientation="vertical" className="h-5" />
        <nav className="flex items-center gap-3 text-sm">
          <Link to="/" className="text-muted-foreground hover:text-foreground transition-colors">Polymarket</Link>
          <Link to="/news" className="text-muted-foreground hover:text-foreground transition-colors">News</Link>
          <Link to="/options" className="text-muted-foreground hover:text-foreground transition-colors">Options</Link>
          <Link to="/futures" className="text-muted-foreground hover:text-foreground transition-colors">Futures</Link>
          <Link to="/dashboard" className="text-muted-foreground hover:text-foreground transition-colors">Dashboard</Link>
          <Link to="/scrapers" className="text-muted-foreground hover:text-foreground transition-colors">Scrapers</Link>
          <Link to="/workflows" className="text-foreground font-medium">Workflows</Link>
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
              onClearRun={() => { stream.reset(); setLastRun(null) }}
              lastRun={displayedRun ?? undefined}
              runRunning={stream.status === 'connecting' || stream.status === 'running'}
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

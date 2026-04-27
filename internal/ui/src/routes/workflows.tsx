import { createFileRoute, Link } from '@tanstack/react-router'
import { useCallback, useEffect, useState } from 'react'
import { toast } from 'sonner'
import { Separator } from '~/components/ui/separator'
import { WorkflowSidebar } from '~/components/workflow/WorkflowSidebar'
import { WorkflowCanvas } from '~/components/workflow/WorkflowCanvas'
import { useWorkflowStore, type Workflow, type Connection } from '~/components/workflow/useWorkflowStore'
import type { RunResults } from '~/components/workflow/RunResultsContext'
import type { Node, Edge } from '@xyflow/react'
import { useQueryState } from '~/hooks/useQueryState'

export const Route = createFileRoute('/workflows')({
  component: WorkflowsPage,
})

const API_BASE = import.meta.env['VITE_API_URL'] ?? 'http://localhost:8080'

function WorkflowsPage() {
  const { workflows, activeId, setWorkflows, setConnections, setActive, activeWorkflow } = useWorkflowStore()
  const [lastRun, setLastRun] = useState<RunResults | null>(null)
  const [qsWorkflow, setQsWorkflow] = useQueryState('workflow')

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

    // Auto-save before run so server has latest graph
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

    setLastRun(null) // clear stale results immediately before new run
    try {
      const bodyObj: Record<string, unknown> = {}
      if (stopAt) bodyObj.stop_at = stopAt
      if (input !== undefined) bodyObj.input = input

      const res = await fetch(`${API_BASE}/api/v1/workflows/${wf.id}/run`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: Object.keys(bodyObj).length > 0 ? JSON.stringify(bodyObj) : undefined,
      })
      const data = await res.json()
      if (!res.ok) {
        toast.error(`Run failed: ${data.error}`)
        return
      }
      const steps: Array<{ node_id: string; node_type: string; output?: unknown; error?: string }> =
        data.steps ?? []

      // group results by node_id for debug panels
      const grouped: RunResults = {}
      for (const step of steps) {
        if (!grouped[step.node_id]) grouped[step.node_id] = []
        grouped[step.node_id]!.push(step)
      }
      setLastRun(grouped)

      let hasError = false
      for (const step of steps) {
        if (step.error) {
          toast.error(`[${step.node_type}] ${step.error}`)
          hasError = true
        }
      }
      // summarise mongo_upsert results — each step upserts one doc; count trues
      const upsertSteps = steps.filter((s) => s.node_type === 'mongo_upsert' && !s.error)
      if (upsertSteps.length > 0) {
        const inserted = upsertSteps.filter((s) => (s.output as { upserted?: boolean } | undefined)?.upserted).length
        toast.success(`Upserted ${inserted} / ${upsertSteps.length} docs`)
      }
      if (!hasError && steps.every((s) => !s.error)) {
        toast.success('Workflow completed')
      }
    } catch {
      toast.error('Network error running workflow')
    }
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
            <WorkflowCanvas key={active.id} workflow={active} onSave={handleSave} onRun={handleRun} onClearRun={() => setLastRun(null)} lastRun={lastRun ?? undefined} />
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

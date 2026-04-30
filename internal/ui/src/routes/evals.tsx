// /evals — eval harness UI.
//
// Phase-1 scope: a list of evals, an in-page JSON editor to author /
// edit one (intentionally raw JSON for now — typed case-editor lands in
// phase 2), a Run button that fires POST /api/v1/evals/:id/run and
// renders the resulting EvalRun (pass/fail counts, total cost, p95
// latency, per-case results with deep-links to the underlying
// workflow_run for full trace inspection).
//
// Backend route map: see internal/api/handler/eval.go.

import { createFileRoute, Link } from '@tanstack/react-router'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { toast } from 'sonner'
import { Play, Plus, Trash2, RefreshCw } from 'lucide-react'
import { Badge } from '~/components/ui/badge'
import { Button } from '~/components/ui/button'
import { Textarea } from '~/components/ui/textarea'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '~/components/ui/select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '~/components/ui/table'
import type { Workflow } from '~/components/workflow/useWorkflowStore'

export const Route = createFileRoute('/evals')({
  component: EvalsPage,
})

const API_BASE = import.meta.env['VITE_API_URL'] ?? 'http://localhost:8080'

interface Assertion {
  target?: 'agent_output' | 'step'
  node_id?: string
  type: 'contains' | 'not_contains' | 'regex' | 'json_path_eq' | 'json_path_exists'
  path?: string
  value?: string
}

interface EvalCase {
  id: string
  name: string
  input?: unknown
  params?: Record<string, string>
  assertions: Assertion[]
}

interface Eval {
  id: string
  workflow_id: string
  name: string
  description?: string
  cases: EvalCase[]
  version?: number
  created_at: string
  updated_at: string
}

interface EvalCaseResult {
  case_id: string
  case_name: string
  workflow_run_id: string
  pass: boolean
  assertion_fails?: string[]
  error?: string
  duration_ms: number
  cost_usd: number
}

interface EvalRun {
  id: string
  eval_id: string
  workflow_id: string
  started_at: string
  finished_at?: string
  status: 'running' | 'done' | 'error'
  cases: EvalCaseResult[]
  pass_count: number
  fail_count: number
  error_count: number
  total_cost_usd: number
  p50_latency_ms: number
  p95_latency_ms: number
  error?: string
  // Snapshot of the eval doc as it existed when this run was kicked off.
  // Lets the UI render the exact cases / assertions used for any
  // historical row, even after the live Eval has been edited.
  eval_version?: number
  eval_snapshot?: Eval
}

function formatCost(usd?: number): string {
  if (!usd) return '$0.00'
  if (usd < 0.01) return `$${usd.toFixed(4)}`
  return `$${usd.toFixed(2)}`
}

// SnapshotViewer renders the EvalSnapshot stored on an EvalRun. Lets the
// user inspect the exact cases + assertions that produced a historical
// run, even after the live eval has been edited. Collapsed by default to
// keep the run-detail card compact.
function SnapshotViewer({ snapshot }: { snapshot: Eval }) {
  const [open, setOpen] = useState(false)
  return (
    <div className="border-t pt-2">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="text-xs text-muted-foreground hover:text-foreground"
      >
        {open ? '▾' : '▸'} Config used (v{snapshot.version ?? '?'}, {snapshot.cases?.length ?? 0} case{snapshot.cases?.length === 1 ? '' : 's'})
      </button>
      {open && (
        <pre className="mt-2 max-h-[300px] overflow-auto rounded bg-muted/30 p-2 text-[10px] font-mono whitespace-pre-wrap">
          {JSON.stringify({
            name: snapshot.name,
            description: snapshot.description,
            cases: snapshot.cases,
          }, null, 2)}
        </pre>
      )}
    </div>
  )
}

function EvalsPage() {
  const [workflows, setWorkflows] = useState<Workflow[]>([])
  const [evals, setEvals] = useState<Eval[]>([])
  const [selectedID, setSelectedID] = useState<string | null>(null)
  const [draft, setDraft] = useState<string>('')
  const [running, setRunning] = useState(false)
  const [lastRun, setLastRun] = useState<EvalRun | null>(null)
  const [history, setHistory] = useState<EvalRun[]>([])
  const [creatingFor, setCreatingFor] = useState<string>('')

  const wfNameByID = useMemo(() => {
    const out: Record<string, string> = {}
    for (const w of workflows) out[w.id] = w.name
    return out
  }, [workflows])

  const loadWorkflows = useCallback(async () => {
    try {
      const res = await fetch(`${API_BASE}/api/v1/workflows`)
      const wfs: Workflow[] = await res.json()
      setWorkflows(wfs)
    } catch {
      toast.error('Failed to load workflows')
    }
  }, [])

  const loadEvals = useCallback(async () => {
    try {
      const res = await fetch(`${API_BASE}/api/v1/evals`)
      if (!res.ok) {
        const data = await res.json()
        toast.error(data.error ?? 'Failed to load evals')
        return
      }
      const data: Eval[] = await res.json()
      setEvals(data ?? [])
    } catch {
      toast.error('Network error loading evals')
    }
  }, [])

  useEffect(() => {
    loadWorkflows()
    loadEvals()
  }, [loadWorkflows, loadEvals])

  const selected = useMemo(
    () => evals.find((e) => e.id === selectedID) ?? null,
    [evals, selectedID],
  )

  // When the selected eval changes, hydrate the JSON editor + load run
  // history. Latest 50; click a row to inspect.
  useEffect(() => {
    if (!selected) {
      setDraft('')
      setHistory([])
      setLastRun(null)
      return
    }
    setDraft(JSON.stringify({
      name: selected.name,
      workflow_id: selected.workflow_id,
      description: selected.description ?? '',
      cases: selected.cases ?? [],
    }, null, 2))
    loadHistory(selected.id)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selected])

  const loadHistory = useCallback(async (evalID: string) => {
    try {
      const res = await fetch(`${API_BASE}/api/v1/eval_runs?eval_id=${encodeURIComponent(evalID)}&limit=50`)
      if (!res.ok) return
      const data: EvalRun[] = await res.json()
      setHistory(data ?? [])
    } catch {
      // silent — history is informational
    }
  }, [])

  async function handleCreate() {
    if (!creatingFor) {
      toast.error('Pick a workflow first')
      return
    }
    const id = crypto.randomUUID()
    const body = {
      id,
      workflow_id: creatingFor,
      name: 'New eval',
      cases: [{
        id: crypto.randomUUID(),
        name: 'case 1',
        input: null,
        assertions: [
          { type: 'contains', value: 'expected text' },
        ],
      }],
    }
    try {
      const res = await fetch(`${API_BASE}/api/v1/evals/${id}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      if (!res.ok) {
        const data = await res.json()
        toast.error(data.error ?? 'Create failed')
        return
      }
      toast.success('Eval created')
      await loadEvals()
      setSelectedID(id)
    } catch {
      toast.error('Network error creating eval')
    }
  }

  async function handleSave() {
    if (!selected) return
    let parsed: Record<string, unknown>
    try {
      parsed = JSON.parse(draft)
    } catch (e) {
      toast.error(`Invalid JSON: ${(e as Error).message}`)
      return
    }
    try {
      const res = await fetch(`${API_BASE}/api/v1/evals/${selected.id}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ...parsed, id: selected.id }),
      })
      if (!res.ok) {
        const data = await res.json()
        toast.error(data.error ?? 'Save failed')
        return
      }
      toast.success('Eval saved')
      await loadEvals()
    } catch {
      toast.error('Network error saving eval')
    }
  }

  async function handleDelete(id: string) {
    if (!confirm('Delete this eval? Past runs are kept.')) return
    try {
      await fetch(`${API_BASE}/api/v1/evals/${id}`, { method: 'DELETE' })
      toast.success('Eval deleted')
      if (selectedID === id) setSelectedID(null)
      await loadEvals()
    } catch {
      toast.error('Network error deleting eval')
    }
  }

  async function handleRun() {
    if (!selected) return
    setRunning(true)
    setLastRun(null)
    try {
      const res = await fetch(`${API_BASE}/api/v1/evals/${selected.id}/run`, { method: 'POST' })
      const data = await res.json()
      if (!res.ok) {
        toast.error(data.error ?? 'Run failed')
      }
      // Whether 200 or 500, the backend echoes a (partial) run record so
      // we can render whatever we got.
      const run: EvalRun = (data?.run ?? data) as EvalRun
      setLastRun(run)
      if (res.ok) {
        toast.success(`${run.pass_count}/${run.cases.length} pass · ${formatCost(run.total_cost_usd)}`, { duration: 6000 })
      }
      // Refresh history so the just-completed run shows up at the top
      // without a manual reload.
      if (selected) loadHistory(selected.id)
    } catch {
      toast.error('Network error running eval')
    } finally {
      setRunning(false)
    }
  }

  function formatStarted(s: string): string {
    const d = new Date(s)
    if (Number.isNaN(d.getTime())) return s
    return d.toLocaleString(undefined, {
      month: 'short',
      day: 'numeric',
      hour: 'numeric',
      minute: '2-digit',
      second: '2-digit',
    })
  }

  return (
      <main className="flex-1 min-h-0 overflow-hidden flex">
        {/* Sidebar — eval list + create */}
        <aside className="w-[300px] shrink-0 border-r overflow-y-auto p-4 space-y-3">
          <div className="flex items-center gap-2">
            <Select value={creatingFor} onValueChange={setCreatingFor}>
              <SelectTrigger className="flex-1 h-8 text-xs">
                <SelectValue placeholder="Workflow…" />
              </SelectTrigger>
              <SelectContent>
                {workflows.map((w) => (
                  <SelectItem key={w.id} value={w.id}>{w.name}</SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Button size="sm" onClick={handleCreate} disabled={!creatingFor}>
              <Plus className="h-3.5 w-3.5 mr-1" /> New
            </Button>
          </div>

          <Button size="sm" variant="outline" onClick={loadEvals} className="w-full">
            <RefreshCw className="h-3.5 w-3.5 mr-1.5" /> Refresh
          </Button>

          <div className="space-y-1">
            {evals.length === 0 && (
              <p className="text-xs text-muted-foreground italic">No evals yet. Pick a workflow above and click New.</p>
            )}
            {evals.map((e) => (
              <div key={e.id} className={`flex items-center gap-2 rounded-md px-2 py-1.5 cursor-pointer ${selectedID === e.id ? 'bg-accent' : 'hover:bg-muted/50'}`}>
                <button onClick={() => setSelectedID(e.id)} className="flex-1 text-left text-xs truncate">
                  <div className="font-medium truncate">{e.name}</div>
                  <div className="text-[10px] text-muted-foreground truncate">{wfNameByID[e.workflow_id] ?? e.workflow_id} · {e.cases.length} case{e.cases.length === 1 ? '' : 's'}</div>
                </button>
                <button onClick={() => handleDelete(e.id)} className="text-muted-foreground hover:text-red-400 shrink-0" title="Delete">
                  <Trash2 className="h-3 w-3" />
                </button>
              </div>
            ))}
          </div>
        </aside>

        {/* Main pane — selected eval editor + run results */}
        <section className="flex-1 overflow-y-auto p-6 space-y-4">
          {!selected && (
            <div className="text-sm text-muted-foreground italic">Select an eval to edit, or create one from the sidebar.</div>
          )}
          {selected && (
            <>
              <div className="flex items-center gap-2">
                <h2 className="text-base font-semibold flex-1 truncate">{selected.name}</h2>
                {selected.version != null && (
                  <Badge variant="outline" title="Bumps on every Save. Each EvalRun captures the version it ran against.">
                    v{selected.version}
                  </Badge>
                )}
                <Button onClick={handleSave}>Save</Button>
                <Button onClick={handleRun} disabled={running} className="bg-green-700 hover:bg-green-800 text-white">
                  <Play className="h-4 w-4 mr-1" /> {running ? 'Running…' : 'Run'}
                </Button>
              </div>
              <p className="text-[10px] text-muted-foreground -mt-2">
                Edit <code>name</code> in the JSON below; Save persists.
              </p>

              <div>
                <p className="text-xs text-muted-foreground mb-1">
                  Eval definition (JSON). Schema: <code className="text-[10px]">{`{ name, workflow_id, cases: [{ id, name, input, params, assertions: [{ type, value, path?, target? }] }] }`}</code>
                </p>
                <Textarea
                  className="font-mono text-xs min-h-[300px]"
                  value={draft}
                  onChange={(e) => setDraft(e.target.value)}
                />
              </div>

              {lastRun && (
                <div className="border rounded-md bg-card p-4 space-y-3">
                  <div className="flex items-center gap-3 flex-wrap">
                    <Badge variant={lastRun.fail_count + lastRun.error_count === 0 ? 'default' : 'destructive'}>
                      {lastRun.pass_count}/{lastRun.cases.length} pass
                    </Badge>
                    {lastRun.eval_version != null && (
                      <Badge
                        variant={selected.version === lastRun.eval_version ? 'outline' : 'secondary'}
                        title={selected.version === lastRun.eval_version
                          ? `Ran against the current eval definition (v${lastRun.eval_version}).`
                          : `Ran against eval v${lastRun.eval_version}; current is v${selected.version ?? '?'}. Click "Show config" below to see the snapshot used.`}
                      >
                        eval v{lastRun.eval_version}{selected.version != null && selected.version !== lastRun.eval_version ? ` (current v${selected.version})` : ''}
                      </Badge>
                    )}
                    <Badge variant="outline">{formatCost(lastRun.total_cost_usd)}</Badge>
                    <Badge variant="outline">p50 {lastRun.p50_latency_ms}ms</Badge>
                    <Badge variant="outline">p95 {lastRun.p95_latency_ms}ms</Badge>
                    <span className="text-xs text-muted-foreground ml-auto">run_id: <code className="text-[10px]">{lastRun.id}</code></span>
                  </div>

                  {lastRun.eval_snapshot && (
                    <SnapshotViewer snapshot={lastRun.eval_snapshot} />
                  )}

                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>Case</TableHead>
                        <TableHead>Result</TableHead>
                        <TableHead>Cost</TableHead>
                        <TableHead>Duration</TableHead>
                        <TableHead>Workflow run</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {lastRun.cases.map((c) => (
                        <TableRow key={c.case_id}>
                          <TableCell className="text-xs">{c.case_name || c.case_id}</TableCell>
                          <TableCell className="text-xs">
                            {/* Case-level badge: pass/fail mirrors the Run
                                history Status column so the user sees the
                                same vocabulary on both surfaces. Executor-
                                level errors (cost cap, missing config) ALSO
                                read as "fail" here — the underlying error
                                message renders below the badge so context
                                isn't lost. */}
                            {c.pass ? (
                              <Badge variant="default" className="bg-green-700">pass</Badge>
                            ) : (
                              <Badge variant="destructive">fail</Badge>
                            )}
                            {(c.assertion_fails ?? []).map((f, i) => (
                              <div key={i} className="text-[10px] text-red-400 mt-1">{f}</div>
                            ))}
                            {c.error && <div className="text-[10px] text-red-400 mt-1">{c.error}</div>}
                          </TableCell>
                          <TableCell className="text-xs">{formatCost(c.cost_usd)}</TableCell>
                          <TableCell className="text-xs">{c.duration_ms}ms</TableCell>
                          <TableCell className="text-xs">
                            {c.workflow_run_id ? (
                              <Link to="/runs/$runId" params={{ runId: c.workflow_run_id }} className="text-blue-400 hover:underline">view trace</Link>
                            ) : '—'}
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </div>
              )}

              {/* Run history — every prior eval execution, newest first.
                  Click a row to load it back into the inspector above
                  (replaces lastRun with the historical run). Backed by
                  GET /api/v1/eval_runs?eval_id=<id>. */}
              {history.length > 0 && (
                <div className="border rounded-md bg-card">
                  <div className="px-4 py-2 border-b flex items-center justify-between">
                    <h3 className="text-sm font-semibold">Run history</h3>
                    <span className="text-[10px] text-muted-foreground">{history.length} run{history.length === 1 ? '' : 's'}</span>
                  </div>
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>Started</TableHead>
                        <TableHead>Version</TableHead>
                        <TableHead>Status</TableHead>
                        <TableHead>Pass</TableHead>
                        <TableHead>Cost</TableHead>
                        <TableHead>p95</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {history.map((r) => {
                        const total = r.cases?.length ?? 0
                        const isFocused = lastRun?.id === r.id
                        const stale = r.eval_version != null && selected.version != null && r.eval_version !== selected.version
                        return (
                          <TableRow
                            key={r.id}
                            onClick={() => setLastRun(r)}
                            className={`cursor-pointer ${isFocused ? 'bg-accent/40' : 'hover:bg-muted/50'}`}
                          >
                            <TableCell className="text-xs">{formatStarted(r.started_at)}</TableCell>
                            <TableCell className="text-xs">
                              <Badge variant={stale ? 'secondary' : 'outline'} title={stale ? `Eval has been edited since this run (current v${selected.version}). Click row to inspect snapshot used.` : 'Ran against the current eval definition.'}>
                                v{r.eval_version ?? '?'}
                              </Badge>
                            </TableCell>
                            <TableCell className="text-xs">
                              {r.status === 'error' ? (
                                <Badge variant="destructive">error</Badge>
                              ) : r.fail_count + r.error_count === 0 ? (
                                <Badge variant="default" className="bg-green-700">pass</Badge>
                              ) : (
                                <Badge variant="destructive">fail</Badge>
                              )}
                            </TableCell>
                            <TableCell className="text-xs">{r.pass_count}/{total}</TableCell>
                            <TableCell className="text-xs">{formatCost(r.total_cost_usd)}</TableCell>
                            <TableCell className="text-xs">{r.p95_latency_ms}ms</TableCell>
                          </TableRow>
                        )
                      })}
                    </TableBody>
                  </Table>
                </div>
              )}
            </>
          )}
        </section>
      </main>
  )
}

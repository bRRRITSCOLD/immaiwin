// /runs — workflow run history table.
//
// Backed by GET /api/v1/workflow_runs (filterable list) + the existing
// /api/v1/workflows endpoint for joining workflow names. Filters: workflow,
// status, limit. Pagination via skip+limit. Click a row → /runs/:id.
//
// Why a separate route (not a panel inside /workflows): cross-workflow
// visibility (debugging "what ran today, regardless of which workflow"),
// shareable URLs for run detail, foundation for the eval harness UI which
// will live next to this in /evals.

import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { toast } from 'sonner'
import { ChevronLeft, ChevronRight, RefreshCw } from 'lucide-react'
import { Badge } from '~/components/ui/badge'
import { Button } from '~/components/ui/button'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '~/components/ui/select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '~/components/ui/table'
import type { Workflow } from '~/components/workflow/useWorkflowStore'

export const Route = createFileRoute('/runs')({
  component: RunsPage,
})

const API_BASE = import.meta.env['VITE_API_URL'] ?? 'http://localhost:8080'

type RunStatus = 'running' | 'success' | 'error' | 'cancelled' | 'paused' | 'pending_approval'

interface UsageTotal {
  input_tokens?: number
  output_tokens?: number
  total_tokens?: number
  cost_usd?: number
}

interface PausedAgent {
  agent_node_id?: string
  iter?: number
}

interface WorkflowRun {
  id: string
  workflow_id: string
  tenant_id: string
  started_at: string
  finished_at?: string
  status: RunStatus
  usage?: UsageTotal
  error?: string
  paused_agent?: PausedAgent | null
}

function isCostExceeded(err?: string): boolean {
  return !!err && err.startsWith('cost_exceeded:')
}

const STATUS_OPTIONS: { value: 'all' | RunStatus; label: string }[] = [
  { value: 'all', label: 'All statuses' },
  { value: 'running', label: 'Running' },
  { value: 'success', label: 'Success' },
  { value: 'error', label: 'Error' },
  { value: 'cancelled', label: 'Cancelled' },
  { value: 'paused', label: 'Paused' },
  { value: 'pending_approval', label: 'Pending approval' },
]

const LIMIT_OPTIONS = [25, 50, 100, 200]

// statusBadgeClass maps a run status to a Tailwind className matching
// the NodeDebugPanel dot colors, so /runs + /runs/:id badges align
// visually with the canvas indicator users already learned.
function statusBadgeClass(s: RunStatus): string {
  switch (s) {
    case 'success':
      return 'bg-green-600 hover:bg-green-600 text-white border-transparent'
    case 'error':
      return 'bg-red-600 hover:bg-red-600 text-white border-transparent'
    case 'cancelled':
      return 'bg-zinc-500 hover:bg-zinc-500 text-white border-transparent'
    case 'paused':
      return 'bg-yellow-500 hover:bg-yellow-500 text-black border-transparent'
    case 'pending_approval':
      return 'bg-amber-500 hover:bg-amber-500 text-black border-transparent'
    case 'running':
    default:
      return 'bg-blue-500 hover:bg-blue-500 text-white border-transparent animate-pulse'
  }
}

function formatDuration(start: string, end?: string): string {
  if (!end) return '—'
  const ms = new Date(end).getTime() - new Date(start).getTime()
  if (!Number.isFinite(ms) || ms < 0) return '—'
  if (ms < 1000) return `${ms}ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`
  const m = Math.floor(ms / 60_000)
  const s = Math.floor((ms % 60_000) / 1000)
  return `${m}m ${s}s`
}

function formatCost(usd?: number): string {
  if (!usd) return '$0.00'
  if (usd < 0.01) return `$${usd.toFixed(4)}`
  return `$${usd.toFixed(2)}`
}

// Daily-cost chip color thresholds. Mirrors the executor's hard cap
// (>=100% blocks runs) so the chip turns red the same instant the agent
// loop starts rejecting calls.
function dailyChipClass(pct: number | undefined, hasCap: boolean): string {
  if (!hasCap) return 'bg-muted text-muted-foreground border border-border'
  const p = pct ?? 0
  if (p >= 100) return 'bg-red-700 text-white border border-red-300'
  if (p >= 80) return 'bg-red-700/70 text-white border border-red-400'
  if (p >= 50) return 'bg-amber-600 text-white border border-amber-300'
  return 'bg-green-700 text-white border border-green-300'
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

interface DailyTotal {
  workflow_id: string
  spent_usd: number
  cap_usd: number
  limit_pct?: number
  since: string
}

interface DailyTotalsRow {
  workflow_id: string
  name: string
  spent_usd: number
  cap_usd: number
  limit_pct?: number
}

interface DailyTotalsBundle {
  since: string
  total: { spent_usd: number }
  by_workflow: DailyTotalsRow[]
}

function RunsPage() {
  const navigate = useNavigate()
  const [workflows, setWorkflows] = useState<Workflow[]>([])
  const [runs, setRuns] = useState<WorkflowRun[]>([])
  const [cancellingID, setCancellingID] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)

  const [workflowFilter, setWorkflowFilter] = useState<string>('all')
  const [statusFilter, setStatusFilter] = useState<'all' | RunStatus>('all')
  const [limit, setLimit] = useState(50)
  const [skip, setSkip] = useState(0)
  const [dailyTotal, setDailyTotal] = useState<DailyTotal | null>(null)
  const [dailyTotals, setDailyTotals] = useState<DailyTotalsBundle | null>(null)

  const wfNameByID = useMemo(() => {
    const out: Record<string, string> = {}
    for (const w of workflows) out[w.id] = w.name
    return out
  }, [workflows])

  // Set of workflow IDs that contain at least one AI Agent node. Cost
  // chips only matter for these — workflows with no ai_agent can't accrue
  // LLM spend, so the cap chip is meaningless. Keeps the toolbar honest
  // (no metric without a generator behind it).
  const wfHasAgent = useMemo(() => {
    const out = new Set<string>()
    for (const w of workflows) {
      if (w.nodes?.some((n) => n.type === 'ai_agent')) out.add(w.id)
    }
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

  const loadRuns = useCallback(async () => {
    setLoading(true)
    try {
      const params = new URLSearchParams()
      if (workflowFilter !== 'all') params.set('workflow_id', workflowFilter)
      if (statusFilter !== 'all') params.set('status', statusFilter)
      params.set('limit', String(limit))
      params.set('skip', String(skip))
      const res = await fetch(`${API_BASE}/api/v1/workflow_runs?${params}`)
      if (!res.ok) {
        const data = await res.json()
        toast.error(`Failed to load runs: ${data.error}`)
        return
      }
      const data: WorkflowRun[] = await res.json()
      setRuns(data ?? [])
    } catch {
      toast.error('Network error loading runs')
    } finally {
      setLoading(false)
    }
  }, [workflowFilter, statusFilter, limit, skip])

  useEffect(() => {
    loadWorkflows()
  }, [loadWorkflows])

  useEffect(() => {
    loadRuns()
  }, [loadRuns])

  const cancelRun = useCallback(
    async (id: string) => {
      if (!confirm('Force-cancel this run? Marks it as cancelled.')) return
      setCancellingID(id)
      try {
        const res = await fetch(`${API_BASE}/api/v1/workflow_runs/${id}/cancel`, { method: 'POST' })
        if (!res.ok) {
          const body = await res.json().catch(() => ({}))
          toast.error(`Cancel failed: ${body.error ?? res.statusText}`)
          return
        }
        toast.success('Run cancelled')
        loadRuns()
      } catch {
        toast.error('Network error cancelling run')
      } finally {
        setCancellingID(null)
      }
    },
    [loadRuns],
  )

  // Fetch the per-workflow daily-cost rollup for the chip. Skipped when
  // the user is viewing "All workflows" (the chip is per-workflow scoped;
  // a cross-workflow daily total isn't meaningful when caps are set per
  // workflow). Re-runs when the filter changes OR after a refresh, so the
  // chip stays in sync with whatever the runs table is showing.
  const loadDailyTotal = useCallback(async () => {
    if (workflowFilter === 'all') {
      setDailyTotal(null)
      return
    }
    try {
      const res = await fetch(`${API_BASE}/api/v1/workflow_runs/daily_total?workflow_id=${encodeURIComponent(workflowFilter)}`)
      if (!res.ok) return
      const data: DailyTotal = await res.json()
      setDailyTotal(data)
    } catch {
      // silent — chip is purely informational, not a hard requirement
    }
  }, [workflowFilter])

  // Cross-workflow rollup. Loaded only in the "All workflows" view; the
  // per-workflow `loadDailyTotal` covers the focused-filter case.
  const loadDailyTotals = useCallback(async () => {
    if (workflowFilter !== 'all') {
      setDailyTotals(null)
      return
    }
    try {
      const res = await fetch(`${API_BASE}/api/v1/workflow_runs/daily_totals`)
      if (!res.ok) return
      const data: DailyTotalsBundle = await res.json()
      setDailyTotals(data)
    } catch {
      // silent — informational rollup, not blocking
    }
  }, [workflowFilter])

  useEffect(() => {
    loadDailyTotal()
    loadDailyTotals()
  }, [loadDailyTotal, loadDailyTotals, runs])

  // Reset pagination when filters change so the user doesn't end up on
  // an empty page after narrowing the result set.
  useEffect(() => {
    setSkip(0)
  }, [workflowFilter, statusFilter, limit])

  const hasNext = runs.length === limit
  const hasPrev = skip > 0

  return (
      <main className="flex-1 min-h-0 overflow-y-auto p-6">
        <div className="mb-4 flex items-center gap-3 flex-wrap">
          <Select value={workflowFilter} onValueChange={setWorkflowFilter}>
            <SelectTrigger className="w-[260px]">
              <SelectValue placeholder="All workflows" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">All workflows</SelectItem>
              {workflows.map((w) => (
                <SelectItem key={w.id} value={w.id}>
                  {w.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>

          <Select value={statusFilter} onValueChange={(v) => setStatusFilter(v as 'all' | RunStatus)}>
            <SelectTrigger className="w-[160px]">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {STATUS_OPTIONS.map((o) => (
                <SelectItem key={o.value} value={o.value}>
                  {o.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>

          <Select value={String(limit)} onValueChange={(v) => setLimit(Number(v))}>
            <SelectTrigger className="w-[120px]">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {LIMIT_OPTIONS.map((n) => (
                <SelectItem key={n} value={String(n)}>
                  {n} per page
                </SelectItem>
              ))}
            </SelectContent>
          </Select>

          <Button variant="outline" size="sm" onClick={loadRuns} disabled={loading}>
            <RefreshCw className={`h-4 w-4 mr-2 ${loading ? 'animate-spin' : ''}`} />
            Refresh
          </Button>

          {/* Daily-spend chip — single-workflow filter view. Hidden when
              the focused workflow has no AI Agent (nothing can spend, so
              showing $0/$cap is misleading clutter). Color reflects
              fraction of MaxDailyUSD spent today (UTC): green<50%, amber
              50-80%, red >=80%. */}
          {dailyTotal && wfHasAgent.has(dailyTotal.workflow_id) && (
            <div
              className={`flex items-center gap-2 rounded-md px-3 py-1 text-xs font-medium ${dailyChipClass(dailyTotal.limit_pct, dailyTotal.cap_usd > 0)}`}
              title={`Today's spend on this workflow (UTC since ${new Date(dailyTotal.since).toLocaleTimeString()}). ${dailyTotal.cap_usd > 0 ? `Daily cap $${dailyTotal.cap_usd.toFixed(4)}.` : 'No daily cap configured.'}`}
            >
              <span className="opacity-80">Today:</span>
              <span>{formatCost(dailyTotal.spent_usd)}</span>
              {dailyTotal.cap_usd > 0 && (
                <>
                  <span className="opacity-60">/</span>
                  <span>${dailyTotal.cap_usd.toFixed(4)}</span>
                  <span className="opacity-80">({dailyTotal.limit_pct?.toFixed(1) ?? '0.0'}%)</span>
                </>
              )}
            </div>
          )}

          <div className="ml-auto flex items-center gap-2 text-sm text-muted-foreground">
            <Button
              variant="ghost"
              size="sm"
              onClick={() => setSkip(Math.max(0, skip - limit))}
              disabled={!hasPrev || loading}
            >
              <ChevronLeft className="h-4 w-4" />
            </Button>
            <span className="tabular-nums">
              {runs.length === 0 ? 0 : skip + 1}–{skip + runs.length}
            </span>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => setSkip(skip + limit)}
              disabled={!hasNext || loading}
            >
              <ChevronRight className="h-4 w-4" />
            </Button>
          </div>
        </div>

        {/* Cross-workflow daily-spend rollup. Rendered only in the "All
            workflows" view. Shows one chip per agent-bearing workflow
            (color reflects fraction of its MaxDailyUSD spent today) plus
            an aggregate Total chip that sums only those agent workflows.
            Click a workflow chip to focus the filter on it. */}
        {dailyTotals && (() => {
          const agentRows = dailyTotals.by_workflow.filter((r) => wfHasAgent.has(r.workflow_id))
          const agentTotal = agentRows.reduce((acc, r) => acc + r.spent_usd, 0)
          if (agentRows.length === 0) return null
          return (
          <div className="mb-4 flex items-center gap-2 flex-wrap">
            <div
              className="flex items-center gap-2 rounded-md px-3 py-1 text-xs font-medium bg-muted text-muted-foreground border border-border"
              title={`Sum of every agent-bearing workflow's daily spend (UTC since ${new Date(dailyTotals.since).toLocaleTimeString()}). No global cap.`}
            >
              <span className="opacity-80">Total today:</span>
              <span className="text-foreground">{formatCost(agentTotal)}</span>
            </div>

            {agentRows
              // Only chip workflows with non-zero spend or non-zero cap;
              // a freshly-saved agent workflow with no runs yet would
              // otherwise spam a $0 chip per workflow.
              .filter((row) => row.spent_usd > 0 || row.cap_usd > 0)
              .map((row) => (
                <button
                  key={row.workflow_id}
                  type="button"
                  onClick={() => setWorkflowFilter(row.workflow_id)}
                  className={`flex items-center gap-2 rounded-md px-3 py-1 text-xs font-medium ${dailyChipClass(row.limit_pct, row.cap_usd > 0)} hover:opacity-90 transition-opacity cursor-pointer`}
                  title={row.cap_usd > 0
                    ? `${row.name}: $${row.spent_usd.toFixed(4)} of $${row.cap_usd.toFixed(4)} cap (${row.limit_pct?.toFixed(1)}%). Click to filter.`
                    : `${row.name}: $${row.spent_usd.toFixed(4)} spent today, no cap. Click to filter.`}
                >
                  <span className="truncate max-w-[160px]">{row.name}</span>
                  <span className="opacity-60">·</span>
                  <span>{formatCost(row.spent_usd)}</span>
                  {row.cap_usd > 0 && (
                    <span className="opacity-80">({row.limit_pct?.toFixed(1) ?? '0.0'}%)</span>
                  )}
                </button>
              ))}
          </div>
          )
        })()}

        <div className="border rounded-md bg-card">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Started</TableHead>
                <TableHead>Workflow</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="text-right">Duration</TableHead>
                <TableHead className="text-right">Tokens</TableHead>
                <TableHead className="text-right">Cost</TableHead>
                <TableHead></TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {runs.length === 0 && !loading && (
                <TableRow>
                  <TableCell colSpan={7} className="text-center text-muted-foreground py-8">
                    No runs match these filters.
                  </TableCell>
                </TableRow>
              )}
              {runs.map((r) => (
                <TableRow
                  key={r.id}
                  className="cursor-pointer hover:bg-muted/50"
                  onClick={() => navigate({ to: '/runs/$runId', params: { runId: r.id } })}
                >
                  <TableCell className="font-mono text-xs">{formatStarted(r.started_at)}</TableCell>
                  <TableCell>{wfNameByID[r.workflow_id] ?? r.workflow_id}</TableCell>
                  <TableCell>
                    <div className="flex items-center gap-1.5">
                      <Badge className={statusBadgeClass(r.status)}>{r.status}</Badge>
                      {r.paused_agent && (
                        <Badge variant="outline" className="text-amber-600 dark:text-amber-400">
                          breakpoint
                        </Badge>
                      )}
                      {isCostExceeded(r.error) && (
                        <Badge variant="outline" className="text-amber-700 dark:text-amber-300 border-amber-700 dark:border-amber-300">
                          cost cap
                        </Badge>
                      )}
                    </div>
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {formatDuration(r.started_at, r.finished_at)}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {r.usage?.total_tokens?.toLocaleString() ?? '—'}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {formatCost(r.usage?.cost_usd)}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex items-center justify-end gap-2">
                      {(r.status === 'running' || r.status === 'pending_approval' || r.status === 'paused') && (
                        <button
                          type="button"
                          disabled={cancellingID === r.id}
                          onClick={(e) => {
                            e.stopPropagation()
                            cancelRun(r.id)
                          }}
                          className="text-xs text-destructive hover:underline disabled:opacity-50"
                          title="Force-cancel this run"
                        >
                          {cancellingID === r.id ? 'Cancelling…' : 'Cancel'}
                        </button>
                      )}
                      <Link
                        to="/runs/$runId"
                        params={{ runId: r.id }}
                        className="text-primary hover:underline text-sm"
                        onClick={(e) => e.stopPropagation()}
                      >
                        View
                      </Link>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      </main>
  )
}

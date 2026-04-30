// /admin — owner/admin ops dashboard.
//
// Renders three sections:
//   1. Run metrics (counts, cost, by-status, top workflows) — sourced
//      from /api/v1/runs/metrics
//   2. Worker health — sourced from /api/v1/workers/health
//   3. Window selector (24h / 7d / 30d) re-fetches metrics
//
// AuthGate redirects unauth → /login. Page itself fails open: if a
// non-admin lands here the metrics endpoint 403s + section shows a
// "no permission" notice rather than a blank page.

import { createFileRoute } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { toast } from 'sonner'
import { Activity, DollarSign, AlertTriangle, RefreshCw } from 'lucide-react'
import { Button } from '~/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '~/components/ui/card'
import { Badge } from '~/components/ui/badge'
import { api, ApiError } from '~/lib/api'
import { useAuthStore } from '~/lib/auth-store'

export const Route = createFileRoute('/admin')({
  component: AdminPage,
})

interface RunMetrics {
  since: string
  until: string
  total_runs: number
  total_cost_usd: number
  by_status: Array<{ status: string; count: number }>
  top_workflows: Array<{ workflow_id: string; name?: string; count: number; cost_usd: number }>
}

interface WorkerHealth {
  name: string
  status: string
  started_at: string
  last_heartbeat: string
  tick_count: number
  error_count?: number
  last_error?: string
  stopped_at?: string
}

const WINDOWS = [
  { value: '24h', label: 'Last 24h', hours: 24 },
  { value: '7d', label: 'Last 7 days', hours: 24 * 7 },
  { value: '30d', label: 'Last 30 days', hours: 24 * 30 },
]

function AdminPage() {
  const me = useAuthStore((s) => s.me)
  const [metrics, setMetrics] = useState<RunMetrics | null>(null)
  const [workers, setWorkers] = useState<WorkerHealth[]>([])
  const [forbidden, setForbidden] = useState(false)
  const [loading, setLoading] = useState(true)
  const [windowKey, setWindowKey] = useState('7d')

  if (!me) return null

  async function load() {
    setLoading(true)
    try {
      const w = WINDOWS.find((x) => x.value === windowKey) ?? WINDOWS[1]!
      const since = new Date(Date.now() - w.hours * 3600 * 1000).toISOString()
      const [m, h] = await Promise.all([
        api.get<RunMetrics>(`/api/v1/runs/metrics?since=${since}`),
        api.get<{ workers: WorkerHealth[] }>('/api/v1/workers/health'),
      ])
      setMetrics(m)
      setWorkers(h.workers ?? [])
      setForbidden(false)
    } catch (err) {
      if (err instanceof ApiError && err.status === 403) {
        setForbidden(true)
      } else {
        toast.error(err instanceof ApiError ? err.message : 'Load failed')
      }
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
  }, [windowKey])

  return (
    <main className="flex-1 min-h-0 overflow-y-auto max-w-5xl w-full mx-auto p-6 space-y-6">
        <div className="flex items-center justify-between">
          <h2 className="text-2xl font-semibold">Tenant ops</h2>
          <div className="flex items-center gap-2">
            <select
              className="border rounded px-2 py-1.5 text-sm bg-background"
              value={windowKey}
              onChange={(e) => setWindowKey(e.target.value)}
            >
              {WINDOWS.map((w) => (
                <option key={w.value} value={w.value}>{w.label}</option>
              ))}
            </select>
            <Button size="sm" variant="ghost" onClick={() => void load()} disabled={loading}>
              <RefreshCw className={`size-4 ${loading ? 'animate-spin' : ''}`} />
            </Button>
          </div>
        </div>

        {forbidden && (
          <Card>
            <CardContent className="pt-6">
              <div className="flex items-center gap-2 text-sm text-muted-foreground">
                <AlertTriangle className="size-4" />
                Owner or admin role required to view this page.
              </div>
            </CardContent>
          </Card>
        )}

        {!forbidden && (
          <>
            <RunMetricsSection metrics={metrics} loading={loading} />
            <WorkerHealthSection workers={workers} loading={loading} />
          </>
        )}
      </main>
  )
}

function RunMetricsSection({ metrics, loading }: { metrics: RunMetrics | null; loading: boolean }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Activity className="size-5" />
          Workflow runs
        </CardTitle>
        <CardDescription>Activity + cost rollups across this tenant.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {loading && !metrics && <div className="text-sm text-muted-foreground">Loading…</div>}
        {metrics && (
          <>
            <div className="grid grid-cols-2 md:grid-cols-3 gap-4">
              <Stat
                label="Total runs"
                value={metrics.total_runs.toLocaleString()}
                icon={<Activity className="size-4" />}
              />
              <Stat
                label="Total cost"
                value={`$${metrics.total_cost_usd.toFixed(2)}`}
                icon={<DollarSign className="size-4" />}
              />
              <Stat
                label="Window"
                value={`${new Date(metrics.since).toLocaleDateString()} — ${new Date(metrics.until).toLocaleDateString()}`}
                icon={null}
              />
            </div>

            {metrics.by_status.length > 0 && (
              <div>
                <div className="text-xs uppercase text-muted-foreground mb-2">By status</div>
                <div className="flex flex-wrap gap-2">
                  {metrics.by_status.map((s) => (
                    <div key={s.status} className="border rounded px-3 py-1.5 text-sm flex items-center gap-2">
                      <Badge variant="outline" className={statusColor(s.status)}>{s.status}</Badge>
                      <span className="font-mono">{s.count}</span>
                    </div>
                  ))}
                </div>
              </div>
            )}

            {metrics.top_workflows.length > 0 && (
              <div>
                <div className="text-xs uppercase text-muted-foreground mb-2">Top workflows by run count</div>
                <div className="border rounded divide-y">
                  {metrics.top_workflows.map((w) => (
                    <div key={w.workflow_id} className="flex items-center gap-3 p-3 text-sm">
                      <div className="flex-1">
                        <div className="font-medium">{w.name || '(deleted)'}</div>
                        <div className="text-xs text-muted-foreground font-mono">{w.workflow_id.slice(0, 16)}…</div>
                      </div>
                      <div className="text-sm font-mono">{w.count.toLocaleString()} runs</div>
                      <div className="text-sm font-mono text-muted-foreground">${w.cost_usd.toFixed(2)}</div>
                    </div>
                  ))}
                </div>
              </div>
            )}

            {metrics.total_runs === 0 && (
              <div className="text-sm text-muted-foreground">No runs in this window.</div>
            )}
          </>
        )}
      </CardContent>
    </Card>
  )
}

function WorkerHealthSection({ workers, loading }: { workers: WorkerHealth[]; loading: boolean }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Workers</CardTitle>
        <CardDescription>Live worker heartbeats. tick_count increments every 30s; stale rows = stuck process.</CardDescription>
      </CardHeader>
      <CardContent>
        {loading && workers.length === 0 && <div className="text-sm text-muted-foreground">Loading…</div>}
        {!loading && workers.length === 0 && (
          <div className="text-sm text-muted-foreground">No worker heartbeats recorded yet.</div>
        )}
        {workers.length > 0 && (
          <div className="border rounded divide-y">
            {workers.map((w) => (
              <div key={w.name} className="flex items-center gap-3 p-3 text-sm">
                <div className="flex-1">
                  <div className="font-medium">{w.name}</div>
                  <div className="text-xs text-muted-foreground">
                    started {new Date(w.started_at).toLocaleString()}
                    {' · '}last beat {new Date(w.last_heartbeat).toLocaleTimeString()}
                    {' · '}ticks {w.tick_count}
                  </div>
                  {w.last_error && (
                    <div className="text-xs text-destructive mt-1">last error: {w.last_error}</div>
                  )}
                </div>
                <Badge variant="outline" className={workerStatusColor(w.status)}>{w.status}</Badge>
              </div>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function Stat({ label, value, icon }: { label: string; value: string; icon: React.ReactNode }) {
  return (
    <div className="border rounded p-3">
      <div className="text-xs uppercase text-muted-foreground flex items-center gap-1">
        {icon}
        {label}
      </div>
      <div className="text-xl font-semibold mt-1">{value}</div>
    </div>
  )
}

function statusColor(s: string): string {
  switch (s) {
    case 'success': return 'border-emerald-700/50 text-emerald-300'
    case 'error': return 'border-red-700/50 text-red-300'
    case 'cancelled': return 'border-gray-700/50 text-gray-300'
    case 'running': return 'border-blue-700/50 text-blue-300'
    case 'paused': return 'border-amber-700/50 text-amber-300'
    case 'pending_approval': return 'border-purple-700/50 text-purple-300'
    default: return ''
  }
}

function workerStatusColor(s: string): string {
  switch (s) {
    case 'running': return 'border-emerald-700/50 text-emerald-300'
    case 'stopped': return 'border-gray-700/50 text-gray-300'
    case 'errored': return 'border-red-700/50 text-red-300'
    default: return ''
  }
}

// /skills — installed skill library.
//
// Backed by GET /api/v1/skills (registry contents) + POST
// /api/v1/skills/refresh (rescan source bundles, upsert any new
// versions). Read-only on installed skills for now; install/uninstall
// land with multi-tenancy gating in P1.12.
//
// Why a separate route: every demo viewer asks "what skills are
// installed?" — currently we have to dig through Mongo. This is the
// discoverability surface for the skills system, distinct from the
// agent-node skill picker (which is for *using* skills, not
// inventorying them).

import { createFileRoute } from '@tanstack/react-router'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { toast } from 'sonner'
import { ChevronDown, ChevronRight, Package, RefreshCw } from 'lucide-react'
import { Badge } from '~/components/ui/badge'
import { Button } from '~/components/ui/button'
import { Separator } from '~/components/ui/separator'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '~/components/ui/table'
import { api, ApiError } from '~/lib/api'

export const Route = createFileRoute('/skills')({
  component: SkillsPage,
})


interface ManifestTool {
  id?: string
  description?: string
}

interface ManifestSecret {
  name?: string
  description?: string
}

interface ManifestConfig {
  name?: string
  type?: string
  description?: string
  default?: unknown
  required?: boolean
  enum?: string[]
}

interface Manifest {
  id?: string
  name?: string
  version?: string
  description?: string
  api_version?: number
  tools?: ManifestTool[]
  secrets?: ManifestSecret[]
  config?: ManifestConfig[]
}

interface SkillRecord {
  id: string
  slug_id: string
  version: string
  manifest: Manifest
  source_id: string
  installed_at: string
  checksum: string
}

interface RefreshResult {
  imported: number
  errors: string[]
}

function shortSum(s: string): string {
  if (!s) return '—'
  return s.length > 12 ? s.slice(0, 12) + '…' : s
}

function formatDate(s: string): string {
  if (!s) return '—'
  const d = new Date(s)
  return d.toLocaleString(undefined, {
    year: 'numeric',
    month: 'short',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  })
}

function SkillsPage() {
  const [records, setRecords] = useState<SkillRecord[]>([])
  const [loading, setLoading] = useState(true)
  const [refreshing, setRefreshing] = useState(false)
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const data = await api.get<SkillRecord[]>('/api/v1/skills')
      setRecords(data)
    } catch (err) {
      toast.error(`Failed to load skills: ${err instanceof ApiError ? err.message : 'unknown'}`)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
  }, [load])

  const refresh = useCallback(async () => {
    setRefreshing(true)
    try {
      const result = await api.post<RefreshResult>('/api/v1/skills/refresh')
      if (result.errors && result.errors.length > 0) {
        toast.warning(`Imported ${result.imported}, ${result.errors.length} error${result.errors.length === 1 ? '' : 's'}`, {
          description: result.errors.join('\n'),
        })
      } else {
        toast.success(`Imported ${result.imported} skill version${result.imported === 1 ? '' : 's'}`)
      }
      await load()
    } catch (err) {
      toast.error(`Refresh failed: ${err instanceof Error ? err.message : 'unknown'}`)
    } finally {
      setRefreshing(false)
    }
  }, [load])

  // Group by slug_id so the user sees one row per skill with a stack of
  // versions underneath. Sorted by slug ascending; versions desc by
  // installed_at so the freshest install is at the top of each stack.
  const grouped = useMemo(() => {
    const m = new Map<string, SkillRecord[]>()
    for (const rec of records) {
      const arr = m.get(rec.slug_id) ?? []
      arr.push(rec)
      m.set(rec.slug_id, arr)
    }
    const out: { slugID: string; versions: SkillRecord[] }[] = []
    for (const [slugID, versions] of m) {
      versions.sort((a, b) => (b.installed_at.localeCompare(a.installed_at)))
      out.push({ slugID, versions })
    }
    out.sort((a, b) => a.slugID.localeCompare(b.slugID))
    return out
  }, [records])

  return (
      <div className="flex-1 min-h-0 overflow-y-auto max-w-6xl w-full mx-auto p-6">
        <div className="flex items-center justify-between mb-4">
          <div>
            <h1 className="text-2xl font-semibold flex items-center gap-2">
              <Package className="h-6 w-6" />
              Skill Library
            </h1>
            <p className="text-sm text-muted-foreground mt-1">
              Installed skill versions in the platform registry. Each version exposes a tool catalog + system-prompt fragment that AI agents can opt into.
            </p>
          </div>
          <Button onClick={refresh} disabled={refreshing} variant="secondary">
            <RefreshCw className={`h-4 w-4 mr-2 ${refreshing ? 'animate-spin' : ''}`} />
            {refreshing ? 'Scanning sources…' : 'Refresh from sources'}
          </Button>
        </div>

        <Separator className="mb-4" />

        {loading ? (
          <p className="text-sm text-muted-foreground">Loading…</p>
        ) : grouped.length === 0 ? (
          <div className="border rounded p-8 text-center">
            <Package className="h-10 w-10 mx-auto mb-3 text-muted-foreground/50" />
            <p className="text-sm text-muted-foreground">
              No skills installed. The API auto-imports every bundle in <code>SKILLS_DIR</code> on boot — if this is empty, either the directory is wrong or the bundles aren't on disk yet. Drop a bundle in and click <strong>Refresh from sources</strong> (no API restart needed).
            </p>
            <p className="text-xs text-muted-foreground/70 mt-2">
              Default <code>SKILLS_DIR</code>: <code>./skills/bundled</code> (relative to the API's working directory). Override with the <code>SKILLS_DIR</code> env var.
            </p>
          </div>
        ) : (
          <div className="space-y-2">
            {grouped.map((g) => {
              const latest = g.versions[0]!
              const open = expanded[g.slugID] ?? false
              return (
                <div key={g.slugID} className="border rounded">
                  <button
                    type="button"
                    className="w-full flex items-center gap-3 p-3 text-left hover:bg-muted/40 transition-colors"
                    onClick={() => setExpanded((p) => ({ ...p, [g.slugID]: !open }))}
                  >
                    {open ? <ChevronDown className="h-4 w-4 shrink-0" /> : <ChevronRight className="h-4 w-4 shrink-0" />}
                    <div className="flex-1 min-w-0">
                      <div className="flex items-center gap-2">
                        <span className="font-medium">{latest.manifest.name || g.slugID}</span>
                        <Badge variant="outline" className="font-mono text-[10px]">{g.slugID}</Badge>
                        <Badge variant="default" className="text-[10px]">{latest.version}</Badge>
                        {g.versions.length > 1 && (
                          <span className="text-xs text-muted-foreground">+ {g.versions.length - 1} prior</span>
                        )}
                      </div>
                      {latest.manifest.description && (
                        <p className="text-xs text-muted-foreground mt-1 line-clamp-1">
                          {latest.manifest.description}
                        </p>
                      )}
                    </div>
                    <span className="text-xs text-muted-foreground shrink-0">{latest.source_id}</span>
                  </button>

                  {open && (
                    <div className="border-t bg-muted/10 p-4 space-y-4">
                      {latest.manifest.description && (
                        <div>
                          <p className="text-[11px] font-semibold uppercase text-muted-foreground/80 mb-1">Description</p>
                          <p className="text-sm">{latest.manifest.description}</p>
                        </div>
                      )}

                      {latest.manifest.tools && latest.manifest.tools.length > 0 && (
                        <div>
                          <p className="text-[11px] font-semibold uppercase text-muted-foreground/80 mb-1">
                            Tools ({latest.manifest.tools.length})
                          </p>
                          <ul className="text-sm space-y-1">
                            {latest.manifest.tools.map((t, i) => (
                              <li key={i} className="flex items-baseline gap-2">
                                <code className="text-xs bg-muted px-1 py-0.5 rounded shrink-0">
                                  {g.slugID}__{t.id ?? '?'}
                                </code>
                                {t.description && <span className="text-xs text-muted-foreground">{t.description}</span>}
                              </li>
                            ))}
                          </ul>
                        </div>
                      )}

                      {latest.manifest.secrets && latest.manifest.secrets.length > 0 && (
                        <div>
                          <p className="text-[11px] font-semibold uppercase text-muted-foreground/80 mb-1">
                            Required Secrets ({latest.manifest.secrets.length})
                          </p>
                          <ul className="text-sm space-y-1">
                            {latest.manifest.secrets.map((s, i) => (
                              <li key={i} className="flex items-baseline gap-2">
                                <code className="text-xs bg-muted px-1 py-0.5 rounded shrink-0">{s.name}</code>
                                {s.description && <span className="text-xs text-muted-foreground">{s.description}</span>}
                              </li>
                            ))}
                          </ul>
                        </div>
                      )}

                      {latest.manifest.config && latest.manifest.config.length > 0 && (
                        <div>
                          <p className="text-[11px] font-semibold uppercase text-muted-foreground/80 mb-1">
                            Config ({latest.manifest.config.length})
                          </p>
                          <ul className="text-sm space-y-1">
                            {latest.manifest.config.map((c, i) => (
                              <li key={i} className="flex items-baseline gap-2">
                                <code className="text-xs bg-muted px-1 py-0.5 rounded shrink-0">{c.name}</code>
                                <span className="text-[10px] text-muted-foreground">({c.type}{c.required ? ', required' : ''})</span>
                                {c.description && <span className="text-xs text-muted-foreground">{c.description}</span>}
                              </li>
                            ))}
                          </ul>
                        </div>
                      )}

                      <div>
                        <p className="text-[11px] font-semibold uppercase text-muted-foreground/80 mb-1">
                          Installed Versions ({g.versions.length})
                        </p>
                        <Table>
                          <TableHeader>
                            <TableRow>
                              <TableHead>Version</TableHead>
                              <TableHead>Source</TableHead>
                              <TableHead>Installed</TableHead>
                              <TableHead>Checksum</TableHead>
                            </TableRow>
                          </TableHeader>
                          <TableBody>
                            {g.versions.map((v) => (
                              <TableRow key={`${v.slug_id}@${v.version}`}>
                                <TableCell className="font-mono text-xs">{v.version}</TableCell>
                                <TableCell className="text-xs">{v.source_id}</TableCell>
                                <TableCell className="text-xs">{formatDate(v.installed_at)}</TableCell>
                                <TableCell className="font-mono text-xs" title={v.checksum}>
                                  {shortSum(v.checksum)}
                                </TableCell>
                              </TableRow>
                            ))}
                          </TableBody>
                        </Table>
                      </div>
                    </div>
                  )}
                </div>
              )
            })}
          </div>
        )}
      </div>
  )
}

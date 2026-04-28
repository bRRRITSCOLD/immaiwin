import { useEffect, useState } from 'react'
import { useReactFlow } from '@xyflow/react'
import { Sparkles, ChevronDown, ChevronRight, RefreshCcw } from 'lucide-react'
import { Textarea } from '~/components/ui/textarea'
import { Button } from '~/components/ui/button'
import { toast } from 'sonner'

const API_BASE = import.meta.env['VITE_API_URL'] ?? 'http://localhost:8080'

interface Props {
  nodeId: string
  data: Record<string, unknown>
}

interface SkillReq {
  slug_id: string
  range: string
}

type SecretBindings = Record<string, string>
type SkillConfigBindings = Record<string, Record<string, unknown>>

interface ConfigDef {
  name: string
  type: string
  description?: string
  required?: boolean
  default?: unknown
  enum?: string[]
}

interface ManifestPreview {
  name?: string
  description?: string
  author?: { name?: string; url?: string }
  license?: string
  api_version?: number
  tools?: { id: string; description: string; language: string; file: string }[]
  prompt?: { fragment?: string }
  capabilities?: {
    network?: { egress?: string[] }
    storage?: { read?: boolean; write?: boolean }
    secrets?: { name: string; type: string; description?: string }[]
  }
  config?: ConfigDef[]
}

interface RegistryRecord {
  slug_id: string
  version: string
  manifest: ManifestPreview
  source_id: string
}

/**
 * Collapsible "Skills" panel mounted on the AI Agent node.
 *
 * Lets a workflow author declare which skill bundles the agent should load
 * at run time. The shape on disk is `data.skills: SkillReq[]` — the same
 * shape the executor's `appendSkillTools` reads. Authors can either edit
 * the JSON directly (for power users + multi-skill setups) or click an
 * installed skill from the registry list to append it with a `^x.y.z` range.
 *
 * The list is fetched lazily on first expand so closed agent nodes don't
 * pay the network cost.
 */
export function SkillsPanel({ nodeId, data }: Props) {
  const { updateNodeData } = useReactFlow()
  const skills = (data?.skills as SkillReq[] | undefined) ?? []
  const secrets = (data?.skill_secrets as SecretBindings | undefined) ?? {}
  const skillConfig = (data?.skill_config as SkillConfigBindings | undefined) ?? {}

  const [open, setOpen] = useState(skills.length > 0)
  const [text, setText] = useState(() => JSON.stringify(skills, null, 2))
  const [secretsText, setSecretsText] = useState(() => JSON.stringify(secrets, null, 2))
  const [configText, setConfigText] = useState(() => JSON.stringify(skillConfig, null, 2))
  const [available, setAvailable] = useState<RegistryRecord[]>([])
  const [loaded, setLoaded] = useState(false)
  const [expandedKey, setExpandedKey] = useState<string | null>(null)

  // Sync textarea on external data change (e.g. file import).
  useEffect(() => {
    setText(JSON.stringify(skills, null, 2))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [JSON.stringify(skills)])

  async function loadAvailable() {
    try {
      const res = await fetch(`${API_BASE}/api/v1/skills`)
      if (!res.ok) {
        toast.error('Failed to load installed skills')
        return
      }
      const recs: RegistryRecord[] = await res.json()
      setAvailable(recs)
      setLoaded(true)
    } catch {
      toast.error('Network error loading skills')
    }
  }

  async function handleRefresh() {
    try {
      const res = await fetch(`${API_BASE}/api/v1/skills/refresh`, { method: 'POST' })
      if (!res.ok) {
        const d = await res.json().catch(() => ({}))
        toast.error(d.error ?? 'Refresh failed')
        return
      }
      const body = await res.json()
      toast.success(`Imported ${body.imported ?? 0} skill version(s) from sources`)
      await loadAvailable()
    } catch {
      toast.error('Network error refreshing skills')
    }
  }

  function commitText(raw: string) {
    setText(raw)
    try {
      const parsed = JSON.parse(raw)
      if (Array.isArray(parsed)) {
        updateNodeData(nodeId, { skills: parsed })
      }
    } catch {
      // Keep editing; only persist on valid JSON.
    }
  }

  function commitSecrets(raw: string) {
    setSecretsText(raw)
    try {
      const parsed = JSON.parse(raw)
      if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
        updateNodeData(nodeId, { skill_secrets: parsed })
      }
    } catch {
      // Keep editing; only persist on valid JSON.
    }
  }

  function commitConfig(raw: string) {
    setConfigText(raw)
    try {
      const parsed = JSON.parse(raw)
      if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
        updateNodeData(nodeId, { skill_config: parsed })
      }
    } catch {
      // Keep editing; only persist on valid JSON.
    }
  }

  function appendFromRegistry(rec: RegistryRecord) {
    // Build a SemVer range that always matches the picked version. For
    // 0.x.y skills npm treats minor bumps as breaking, so `^0.0.x` would
    // *not* match 0.1.0. Anchor on the lowest stable segment instead:
    //   1.x.x  → ^1.0.0  (any 1.y.z)
    //   0.x.0+ → ^0.x.0  (any 0.x.z)
    //   0.0.x  → 0.0.x   (exact pin — pre-release territory)
    const [majStr, minStr] = rec.version.split('.')
    const major = Number(majStr)
    const minor = Number(minStr)
    let range: string
    if (major > 0) {
      range = `^${major}.0.0`
    } else if (minor > 0) {
      range = `^0.${minor}.0`
    } else {
      range = rec.version
    }
    const next: SkillReq[] = [...skills, { slug_id: rec.slug_id, range }]
    setText(JSON.stringify(next, null, 2))
    updateNodeData(nodeId, { skills: next })
  }

  return (
    <div className="border-t border-border/50">
      <button
        type="button"
        onClick={() => {
          const nextOpen = !open
          setOpen(nextOpen)
          if (nextOpen && !loaded) loadAvailable()
        }}
        className="nodrag flex w-full items-center justify-between gap-2 px-3 py-1.5 text-[10px] font-medium text-muted-foreground hover:text-foreground"
      >
        <span className="flex items-center gap-1.5">
          <Sparkles className={`h-3 w-3 ${skills.length > 0 ? 'text-purple-400' : ''}`} />
          Skills {skills.length > 0 && <span className="text-purple-400">●&nbsp;{skills.length}</span>}
        </span>
        {open ? <ChevronDown className="h-3 w-3" /> : <ChevronRight className="h-3 w-3" />}
      </button>

      {open && (
        <div className="space-y-2 px-3 pb-2">
          <div className="flex items-center justify-between">
            <p className="text-[10px] text-muted-foreground">
              Declare bundles the agent should load. Tools from each skill register as <code className="text-[9px]">slug__tool</code>.
            </p>
            <Button
              size="sm"
              variant="ghost"
              className="nodrag h-6 px-2 text-[10px]"
              onClick={handleRefresh}
              type="button"
            >
              <RefreshCcw className="h-3 w-3 mr-1" /> Refresh
            </Button>
          </div>

          {available.length > 0 && (
            <div className="rounded border border-border/50 max-h-72 overflow-y-auto">
              {available.map((rec) => {
                const key = `${rec.slug_id}-${rec.version}`
                const isOpen = expandedKey === key
                const m = rec.manifest ?? {}
                return (
                  <div key={key} className="border-b border-border/30 last:border-b-0">
                    <div className="flex items-center gap-1 px-2 py-1 text-[10px] hover:bg-muted/50">
                      <button
                        type="button"
                        onClick={() => setExpandedKey(isOpen ? null : key)}
                        className="nodrag shrink-0 text-muted-foreground hover:text-foreground"
                        aria-label="Toggle details"
                      >
                        {isOpen ? <ChevronDown className="h-3 w-3" /> : <ChevronRight className="h-3 w-3" />}
                      </button>
                      <button
                        type="button"
                        onClick={() => appendFromRegistry(rec)}
                        className="nodrag flex flex-1 items-center justify-between gap-2 truncate text-left"
                        title="Add to agent skills"
                      >
                        <span className="truncate">
                          <span className="text-foreground font-medium">{m.name || rec.slug_id}</span>{' '}
                          <span className="text-muted-foreground">@ {rec.version}</span>
                        </span>
                        <span className="text-[9px] text-muted-foreground shrink-0">{rec.source_id}</span>
                      </button>
                    </div>
                    {isOpen && (
                      <div className="px-2 pb-2 pl-6 space-y-2 text-[10px] text-muted-foreground">
                        {m.description && <p className="text-foreground">{m.description}</p>}

                        <div className="grid grid-cols-2 gap-x-2 gap-y-0.5">
                          <span>slug</span><span className="text-foreground font-mono">{rec.slug_id}</span>
                          <span>version</span><span className="text-foreground font-mono">{rec.version}</span>
                          {m.author?.name && (<><span>author</span><span className="text-foreground">{m.author.name}</span></>)}
                          {m.license && (<><span>license</span><span className="text-foreground">{m.license}</span></>)}
                          {m.api_version != null && (<><span>api_version</span><span className="text-foreground">{m.api_version}</span></>)}
                        </div>

                        {(m.tools?.length ?? 0) > 0 && (
                          <div>
                            <p className="font-medium text-foreground mb-1">Tools ({m.tools!.length})</p>
                            <ul className="space-y-1">
                              {m.tools!.map((t) => (
                                <li key={t.id} className="rounded border border-border/30 px-1.5 py-1">
                                  <p className="text-foreground font-mono">
                                    {rec.slug_id.replaceAll('/', '_')}__{t.id}
                                    <span className="ml-1 text-muted-foreground">({t.language})</span>
                                  </p>
                                  {t.description && <p className="text-muted-foreground">{t.description}</p>}
                                  <p className="text-[9px] text-muted-foreground/70">file: {t.file}</p>
                                </li>
                              ))}
                            </ul>
                          </div>
                        )}

                        {m.prompt?.fragment && (
                          <div>
                            <p className="font-medium text-foreground">Prompt fragment</p>
                            <p className="text-muted-foreground">Appended to agent system prompt: <code className="text-[9px]">{m.prompt.fragment}</code></p>
                          </div>
                        )}

                        {(m.capabilities?.network?.egress?.length ?? 0) > 0 && (
                          <div>
                            <p className="font-medium text-foreground">Network egress</p>
                            <p className="text-muted-foreground font-mono">{m.capabilities!.network!.egress!.join(', ')}</p>
                          </div>
                        )}

                        {(m.capabilities?.secrets?.length ?? 0) > 0 && (
                          <div>
                            <p className="font-medium text-foreground">Secrets required</p>
                            <ul className="space-y-0.5">
                              {m.capabilities!.secrets!.map((s) => (
                                <li key={s.name} className="text-muted-foreground">
                                  <code className="text-[9px] text-foreground">{s.name}</code>
                                  <span className="ml-1">({s.type})</span>
                                  {s.description && <span> — {s.description}</span>}
                                </li>
                              ))}
                            </ul>
                          </div>
                        )}

                        {(m.config?.length ?? 0) > 0 && (
                          <div>
                            <p className="font-medium text-foreground">Config (author-bound)</p>
                            <ul className="space-y-0.5">
                              {m.config!.map((c) => (
                                <li key={c.name} className="text-muted-foreground">
                                  <code className="text-[9px] text-foreground">{c.name}</code>
                                  <span className="ml-1">({c.type}{c.required ? ', required' : ''})</span>
                                  {c.enum && c.enum.length > 0 && <span className="ml-1 text-[9px]">enum: {c.enum.join(', ')}</span>}
                                  {c.default !== undefined && <span className="ml-1 text-[9px]">default: <code>{JSON.stringify(c.default)}</code></span>}
                                  {c.description && <p>{c.description}</p>}
                                </li>
                              ))}
                            </ul>
                          </div>
                        )}
                      </div>
                    )}
                  </div>
                )
              })}
            </div>
          )}

          {loaded && available.length === 0 && (
            <p className="text-[10px] text-muted-foreground italic">
              No skills installed. Click <span className="font-medium">Refresh</span> after adding bundles to <code>$SKILLS_DIR</code>.
            </p>
          )}

          <div>
            <p className="text-[10px] text-muted-foreground mb-1">
              Skills JSON: <code className="text-[9px]">[{`{"slug_id":"…","range":"^1.0.0"}`}]</code>
            </p>
            <Textarea
              className="nodrag font-mono text-[10px] min-h-[80px] resize-y"
              value={text}
              onChange={(e) => commitText(e.target.value)}
              placeholder='[{"slug_id": "hello-world", "range": "^0.1.0"}]'
            />
          </div>

          <div>
            <p className="text-[10px] text-muted-foreground mb-1">
              Skill secrets — map each declared secret to a Connection ID:
              <br />
              <code className="text-[9px]">{`{"weather_api_key": "<connection-id>"}`}</code>
            </p>
            <Textarea
              className="nodrag font-mono text-[10px] min-h-[60px] resize-y"
              value={secretsText}
              onChange={(e) => commitSecrets(e.target.value)}
              placeholder='{"weather_api_key": "<connection-id>"}'
            />
          </div>

          <div>
            <p className="text-[10px] text-muted-foreground mb-1">
              Skill config — author-bound knobs per skill. String values support <code className="text-[9px]">{'{{params.x}}'}</code>:
              <br />
              <code className="text-[9px]">{`{"weather-formatter": {"default_style": "friendly"}}`}</code>
            </p>
            <Textarea
              className="nodrag font-mono text-[10px] min-h-[60px] resize-y"
              value={configText}
              onChange={(e) => commitConfig(e.target.value)}
              placeholder='{"weather-formatter": {"default_style": "friendly"}}'
            />
          </div>
        </div>
      )}
    </div>
  )
}

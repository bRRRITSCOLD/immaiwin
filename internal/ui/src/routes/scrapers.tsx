import { createFileRoute, Link } from '@tanstack/react-router'
import { useCallback, useEffect, useRef, useState } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import { toast } from 'sonner'
import { ChevronDown, ChevronRight, Trash2 } from 'lucide-react'
import { Badge } from '~/components/ui/badge'
import { Button } from '~/components/ui/button'
import { Checkbox } from '~/components/ui/checkbox'
import { Input } from '~/components/ui/input'
import { Label } from '~/components/ui/label'
import { Separator } from '~/components/ui/separator'
import { ScriptEditor } from '~/components/ScriptEditor'
import { AddScraperDialog } from '~/components/AddScraperDialog'

export const Route = createFileRoute('/scrapers')({
  component: ScrapersPage,
})

const API_BASE = import.meta.env['VITE_API_URL'] ?? 'http://localhost:8080'

interface ScraperConfig {
  source: string
  feed_url: string
  script?: string
  updated_at: string
}

const FALLBACK_SCRIPT = `// parse(raw) receives raw feed content (RSS/XML or HTML).
// TypeScript is supported — types are stripped before execution in goja.
function parse(raw: string): Article[] {
  const items = parseRSS(raw)
  return items.map((item) => ({
    url: item.link || item.guid || '',
    title: item.title || '',
    body: item.description || '',
    scraped_at: item.pubDate ? parseDate(item.pubDate) : now(),
  }))
}`

interface CardState {
  feedURL: string
  script: string
}

interface ScraperCardProps {
  config: ScraperConfig
  isExpanded: boolean
  isSelected: boolean
  onToggle: () => void
  onSelectChange: (checked: boolean) => void
  initialState: CardState
  onStateChange: (state: CardState) => void
}

function ScraperCard({ config, isExpanded, isSelected, onToggle, onSelectChange, initialState, onStateChange }: ScraperCardProps) {
  const [feedURL, setFeedURL] = useState(initialState.feedURL)
  const [script, setScript] = useState(initialState.script)
  const [hasScript, setHasScript] = useState(!!config.script)
  const [validating, setValidating] = useState(false)
  const [saving, setSaving] = useState(false)

  function handleFeedURLChange(v: string) {
    setFeedURL(v)
    onStateChange({ feedURL: v, script })
  }

  function handleScriptChange(v: string) {
    setScript(v)
    onStateChange({ feedURL, script: v })
  }

  async function validate() {
    setValidating(true)
    try {
      const res = await fetch(`${API_BASE}/api/v1/news/scrapers/validate`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ script }),
      })
      const data = await res.json()
      if (!res.ok) toast.error(`${config.source}: ${data.error}`)
      else toast.success(`${config.source}: script valid`)
    } catch {
      toast.error(`${config.source}: network error`)
    } finally {
      setValidating(false)
    }
  }

  async function save() {
    setSaving(true)
    try {
      const res = await fetch(`${API_BASE}/api/v1/news/scrapers/${config.source}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ feed_url: feedURL, script }),
      })
      const data = await res.json()
      if (!res.ok) toast.error(`${config.source}: ${data.error}`)
      else {
        setHasScript(true)
        toast.success(`${config.source}: saved`)
      }
    } catch {
      toast.error(`${config.source}: network error`)
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className={`rounded-lg border bg-card text-card-foreground ${isSelected ? 'border-primary' : ''}`}>
      <div
        className="flex items-center justify-between px-4 py-3 cursor-pointer select-none"
        onClick={onToggle}
      >
        <div className="flex items-center gap-3">
          {/* Checkbox — stop propagation so it doesn't trigger collapse */}
          <div onClick={(e) => e.stopPropagation()}>
            <Checkbox
              checked={isSelected}
              onCheckedChange={onSelectChange}
              aria-label={`Select ${config.source}`}
            />
          </div>
          {isExpanded
            ? <ChevronDown className="h-4 w-4 shrink-0 text-muted-foreground" />
            : <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground" />}
          <span className="font-medium">{config.source}</span>
          <Badge variant={hasScript ? 'default' : 'secondary'} className="text-xs">
            {hasScript ? 'custom script' : 'default parser'}
          </Badge>
        </div>
        <div className="flex items-center gap-2" onClick={(e) => e.stopPropagation()}>
          <Button size="sm" variant="outline" onClick={validate} disabled={validating}>
            {validating ? 'Validating…' : 'Validate'}
          </Button>
          <Button size="sm" onClick={save} disabled={saving}>
            {saving ? 'Saving…' : 'Save'}
          </Button>
        </div>
      </div>

      {isExpanded && (
        <div className="border-t p-4 space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor={`feed-${config.source}`} className="text-xs text-muted-foreground">Feed URL</Label>
            <Input
              id={`feed-${config.source}`}
              value={feedURL}
              onChange={(e) => handleFeedURLChange(e.target.value)}
              className="font-mono text-xs h-8"
            />
          </div>

          <div className="space-y-1.5">
            <Label className="text-xs text-muted-foreground">
              Parser Script
              <span className="ml-1.5 text-muted-foreground/60">
                — define <code className="text-xs bg-muted px-1 rounded">function parse(raw)</code>
              </span>
            </Label>
            <div className="rounded-md overflow-hidden border">
              <ScriptEditor value={script} onChange={handleScriptChange} height={320} />
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

function ScrapersPage() {
  const [configs, setConfigs] = useState<ScraperConfig[]>([])
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [deleting, setDeleting] = useState(false)
  const stateCache = useRef(new Map<string, CardState>())
  const parentRef = useRef<HTMLDivElement>(null)

  const load = useCallback(() => {
    fetch(`${API_BASE}/api/v1/news/scrapers`)
      .then((r) => r.json())
      .then((data: ScraperConfig[]) => setConfigs(data))
      .catch(() => {})
  }, [])

  useEffect(() => { load() }, [load])

  function toggleExpanded(source: string) {
    setExpanded((prev) => {
      const next = new Set(prev)
      if (next.has(source)) next.delete(source)
      else next.add(source)
      return next
    })
  }

  function toggleSelected(source: string, checked: boolean) {
    setSelected((prev) => {
      const next = new Set(prev)
      if (checked) next.add(source)
      else next.delete(source)
      return next
    })
  }

  async function deleteSelected() {
    setDeleting(true)
    try {
      await Promise.all(
        [...selected].map((source) =>
          fetch(`${API_BASE}/api/v1/news/scrapers/${encodeURIComponent(source)}`, { method: 'DELETE' })
        )
      )
      toast.success(`Deleted ${selected.size} scraper${selected.size !== 1 ? 's' : ''}`)
      setSelected(new Set())
      load()
    } catch {
      toast.error('Delete failed')
    } finally {
      setDeleting(false)
    }
  }

  function getInitialState(config: ScraperConfig): CardState {
    return stateCache.current.get(config.source) ?? {
      feedURL: config.feed_url,
      script: config.script || FALLBACK_SCRIPT,
    }
  }

  const virtualizer = useVirtualizer({
    count: configs.length,
    getScrollElement: () => parentRef.current,
    estimateSize: (i) => (expanded.has(configs[i]?.source ?? '') ? 492 : 57),
    overscan: 3,
  })

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
          <Link to="/scrapers" className="text-foreground font-medium">Scrapers</Link>
        </nav>
      </header>

      <div className="flex-1 overflow-hidden flex flex-col max-w-4xl mx-auto w-full px-4">
        <div className="py-6 flex items-start justify-between shrink-0">
          <div>
            <h2 className="text-base font-semibold">News Scraper Config</h2>
            <p className="text-sm text-muted-foreground mt-0.5">
              Override feed URL and parser script per source. Leave script at default to use built-in parser.
            </p>
          </div>
          <div className="flex items-center gap-2">
            {selected.size > 0 && (
              <Button
                size="sm"
                variant="destructive"
                onClick={deleteSelected}
                disabled={deleting}
                className="gap-1.5"
              >
                <Trash2 className="h-3.5 w-3.5" />
                {deleting ? 'Deleting…' : `Delete ${selected.size}`}
              </Button>
            )}
            <AddScraperDialog apiBase={API_BASE} onCreated={load} />
          </div>
        </div>

        <div ref={parentRef} className="flex-1 overflow-y-auto pb-6">
          <div style={{ height: virtualizer.getTotalSize(), position: 'relative' }}>
            {virtualizer.getVirtualItems().map((vitem) => {
              const config = configs[vitem.index]
              return (
                <div
                  key={vitem.key}
                  data-index={vitem.index}
                  ref={virtualizer.measureElement}
                  style={{
                    position: 'absolute',
                    top: 0,
                    left: 0,
                    right: 0,
                    transform: `translateY(${vitem.start}px)`,
                    paddingBottom: '1rem',
                  }}
                >
                  <ScraperCard
                    config={config}
                    isExpanded={expanded.has(config.source)}
                    isSelected={selected.has(config.source)}
                    onToggle={() => toggleExpanded(config.source)}
                    onSelectChange={(checked) => toggleSelected(config.source, !!checked)}
                    initialState={getInitialState(config)}
                    onStateChange={(s) => stateCache.current.set(config.source, s)}
                  />
                </div>
              )
            })}
          </div>
        </div>
      </div>
    </div>
  )
}

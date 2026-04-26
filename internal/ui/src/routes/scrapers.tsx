import { createFileRoute, Link } from '@tanstack/react-router'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { toast } from 'sonner'
import { Badge } from '~/components/ui/badge'
import { Button } from '~/components/ui/button'
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

const RSS_TEMPLATE = `// parse(raw) receives raw RSS/XML string.
// Helpers: parseRSS(xmlStr), now(), parseDate(str), $(html)
function parse(raw) {
  var items = parseRSS(raw)
  return items.map(function(item) {
    return {
      url: item.link || item.guid || '',
      title: item.title || '',
      body: item.description || '',
      scraped_at: item.pubDate ? parseDate(item.pubDate) : now()
    }
  })
}`

const HTML_TEMPLATE = `// parse(raw) receives raw HTML string (homepage).
// Helpers: $(html), now(), parseRSS(xmlStr), parseDate(str)
function parse(raw) {
  var articles = []
  $(raw).find('h3').each(function(i, el) {
    var title = el.text().trim()
    if (!title) return
    var a = el.find('a')
    var link = a.length ? a.attr('href') : ''
    if (!link) return
    if (link.indexOf('/') === 0) link = 'https://www.aljazeera.com' + link
    if (link.indexOf('http') !== 0) return
    articles.push({ url: link, title: title, scraped_at: now() })
  })
  return articles
}`

interface SourceDef {
  id: string
  label: string
  defaultFeedURL: string
  template: string
}

const KNOWN_SOURCES: SourceDef[] = [
  { id: 'bloomberg', label: 'Bloomberg', defaultFeedURL: 'https://feeds.bloomberg.com/markets/news.rss', template: RSS_TEMPLATE },
  { id: 'investing', label: 'Investing.com', defaultFeedURL: 'https://www.investing.com/rss/news_301.rss', template: RSS_TEMPLATE },
  { id: 'aljazeera', label: 'Al Jazeera', defaultFeedURL: 'https://www.aljazeera.com/', template: HTML_TEMPLATE },
]

const KNOWN_IDS = new Set(KNOWN_SOURCES.map((s) => s.id))

function ScraperCard({ source, saved }: { source: SourceDef; saved?: ScraperConfig }) {
  const [feedURL, setFeedURL] = useState(saved?.feed_url || source.defaultFeedURL)
  const [script, setScript] = useState(saved?.script || source.template)
  const [hasScript, setHasScript] = useState(!!saved?.script)
  const [validating, setValidating] = useState(false)
  const [saving, setSaving] = useState(false)
  const [clearing, setClearing] = useState(false)

  async function validate() {
    setValidating(true)
    try {
      const res = await fetch(`${API_BASE}/api/v1/news/scrapers/validate`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ script }),
      })
      const data = await res.json()
      if (!res.ok) toast.error(`${source.label}: ${data.error}`)
      else toast.success(`${source.label}: script valid`)
    } catch {
      toast.error(`${source.label}: network error`)
    } finally {
      setValidating(false)
    }
  }

  async function save() {
    setSaving(true)
    try {
      const body: Record<string, string> = { feed_url: feedURL }
      if (hasScript || script !== source.template) {
        body.script = script
      }
      const res = await fetch(`${API_BASE}/api/v1/news/scrapers/${source.id}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      const data = await res.json()
      if (!res.ok) toast.error(`${source.label}: ${data.error}`)
      else {
        setHasScript(!!body.script)
        toast.success(`${source.label}: saved`)
      }
    } catch {
      toast.error(`${source.label}: network error`)
    } finally {
      setSaving(false)
    }
  }

  async function clearScript() {
    setClearing(true)
    try {
      const res = await fetch(`${API_BASE}/api/v1/news/scrapers/${source.id}/script`, {
        method: 'DELETE',
      })
      if (!res.ok) {
        const data = await res.json()
        toast.error(`${source.label}: ${data.error}`)
      } else {
        setHasScript(false)
        setScript(source.template)
        toast.success(`${source.label}: script cleared — using default parser`)
      }
    } catch {
      toast.error(`${source.label}: network error`)
    } finally {
      setClearing(false)
    }
  }

  return (
    <div className="rounded-lg border bg-card text-card-foreground">
      <div className="flex items-center justify-between px-4 py-3 border-b">
        <div className="flex items-center gap-2">
          <span className="font-medium">{source.label}</span>
          <Badge variant={hasScript ? 'default' : 'secondary'} className="text-xs">
            {hasScript ? 'custom script' : 'default parser'}
          </Badge>
        </div>
        <div className="flex items-center gap-2">
          <Button size="sm" variant="outline" onClick={validate} disabled={validating}>
            {validating ? 'Validating…' : 'Validate'}
          </Button>
          {hasScript && (
            <Button size="sm" variant="ghost" onClick={clearScript} disabled={clearing} className="text-destructive hover:text-destructive">
              {clearing ? 'Clearing…' : 'Clear Script'}
            </Button>
          )}
          <Button size="sm" onClick={save} disabled={saving}>
            {saving ? 'Saving…' : 'Save'}
          </Button>
        </div>
      </div>

      <div className="p-4 space-y-4">
        <div className="space-y-1.5">
          <Label htmlFor={`feed-${source.id}`} className="text-xs text-muted-foreground">Feed URL</Label>
          <Input
            id={`feed-${source.id}`}
            value={feedURL}
            onChange={(e) => setFeedURL(e.target.value)}
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
            <ScriptEditor value={script} onChange={setScript} height={320} />
          </div>
        </div>
      </div>
    </div>
  )
}

function ScrapersPage() {
  const [configs, setConfigs] = useState<ScraperConfig[]>([])

  const load = useCallback(() => {
    fetch(`${API_BASE}/api/v1/news/scrapers`)
      .then((r) => r.json())
      .then((data: ScraperConfig[]) => setConfigs(data))
      .catch(() => {})
  }, [])

  useEffect(() => { load() }, [load])

  // Merge: known sources first, then any extra sources from DB not in KNOWN_SOURCES
  const allSources = useMemo<SourceDef[]>(() => {
    const extras = configs
      .filter((c) => !KNOWN_IDS.has(c.source))
      .map((c) => ({
        id: c.source,
        label: c.source,
        defaultFeedURL: c.feed_url,
        template: RSS_TEMPLATE,
      }))
    return [...KNOWN_SOURCES, ...extras]
  }, [configs])

  function savedFor(id: string) {
    return configs.find((c) => c.source === id)
  }

  return (
    <div className="h-screen overflow-x-hidden overflow-y-auto bg-background text-foreground flex flex-col">
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

      <main className="max-w-4xl mx-auto w-full px-4 py-6 space-y-6">
        <div className="flex items-start justify-between">
          <div>
            <h2 className="text-base font-semibold">News Scraper Config</h2>
            <p className="text-sm text-muted-foreground mt-0.5">
              Override feed URL and parser script per source. Leave script at default to use built-in parser.
            </p>
          </div>
          <AddScraperDialog apiBase={API_BASE} onCreated={load} />
        </div>

        {allSources.map((source) => (
          <ScraperCard key={source.id} source={source} saved={savedFor(source.id)} />
        ))}
      </main>
    </div>
  )
}

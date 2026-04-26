import { createFileRoute, Link } from '@tanstack/react-router'
import { useCallback, useEffect, useState } from 'react'
import { toast } from 'sonner'
import { Separator } from '~/components/ui/separator'
import { WorkflowSidebar } from '~/components/workflow/WorkflowSidebar'
import { WorkflowCanvas } from '~/components/workflow/WorkflowCanvas'
import { useWorkflowStore, type Workflow } from '~/components/workflow/useWorkflowStore'
import type { RunResults } from '~/components/workflow/RunResultsContext'
import type { Node, Edge } from '@xyflow/react'

export const Route = createFileRoute('/workflows')({
  component: WorkflowsPage,
})

const API_BASE = import.meta.env['VITE_API_URL'] ?? 'http://localhost:8080'

interface ScraperConfig {
  source: string
  feed_url: string
  script?: string
}

function buildSeedWorkflow(scraper: ScraperConfig): Workflow {
  const id = crypto.randomUUID()
  const { nodes, edges, params } =
    scraper.source === 'aljazeera'
      ? buildAljazeeraGraph(scraper)
      : buildRSSGraph(scraper)
  return {
    id,
    name: scraper.source,
    params,
    nodes,
    edges,
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  }
}

/** RSS sources: trigger → http_fetch → js_transform(parseRSS) → for_each → mongo_upsert → redis_publish */
function buildRSSGraph(scraper: ScraperConfig): { nodes: Node[]; edges: Edge[]; params: Record<string, string> } {
  const params = {
    feedURL:    scraper.feed_url,
    platform:   scraper.source,
    collection: 'news_articles',
    channel:    'immaiwin:news:articles',
  }

  const script = `// context.fetchFeed.output = { ok, status, body } from HTTP Fetch
// Return array — ForEach iterates it item by item into Mongo Upsert
var items = parseRSS(context.fetchFeed.output.body);
return items.map(function(item) {
  return {
    url:        item.link || item.guid,
    title:      item.title,
    body:       item.description,
    scraped_at: item.pubDate ? parseDate(item.pubDate) : now(),
    platform:   params.platform,
    metadata:   { categories: item.categories }
  };
});`

  const nodes: Node[] = [
    { id: 'trigger-1',      type: 'trigger',      position: { x: 0,    y: 60  }, data: {} },
    { id: 'http_fetch-1',   type: 'http_fetch',   position: { x: 220,  y: 40  }, data: { url: '{{params.feedURL}}', name: 'fetchFeed' } },
    { id: 'js_transform-1', type: 'js_transform', position: { x: 500,  y: 20  }, data: { script } },
    { id: 'for_each-1',     type: 'for_each',     position: { x: 780,  y: 30  }, data: {} },
    { id: 'mongo_upsert-1', type: 'mongo_upsert', position: { x: 1020, y: 10  }, data: { collection: '{{params.collection}}', filter_field: 'url' } },
    { id: 'redis_publish-1',type: 'redis_publish',position: { x: 1280, y: 10  }, data: { channel: '{{params.channel}}' } },
    { id: 'notify-1',       type: 'notify',       position: { x: 780,  y: 240 }, data: { message: '{{params.platform}}: pipeline error' } },
  ]
  const edges: Edge[] = [
    { id: 'e1', source: 'trigger-1',      target: 'http_fetch-1' },
    { id: 'e2', source: 'http_fetch-1',   target: 'js_transform-1', sourceHandle: 'success' },
    { id: 'e3', source: 'http_fetch-1',   target: 'notify-1',       sourceHandle: 'error', targetHandle: 'in-top' },
    { id: 'e4', source: 'js_transform-1', target: 'for_each-1',     sourceHandle: 'success' },
    { id: 'e5', source: 'js_transform-1', target: 'notify-1',       sourceHandle: 'error', targetHandle: 'in-left' },
    { id: 'e6', source: 'for_each-1',     target: 'mongo_upsert-1', sourceHandle: 'item' },
    { id: 'e7', source: 'for_each-1',     target: 'notify-1',       sourceHandle: 'error', targetHandle: 'in-bottom' },
    { id: 'e8', source: 'mongo_upsert-1', target: 'redis_publish-1',sourceHandle: 'success' },
  ]
  return { nodes, edges, params }
}

/**
 * AlJazeera: two-step fetch (no httpGet in JS).
 * trigger → http_fetch-1(homepage) → js_transform-1(extract links [{url,title}])
 * → for_each
 *    item → http_fetch-2({{input.url}})
 *              ← http_fetch-2 named "fetchArticle" → context.fetchArticle.input = {url, title}
 *           → js_transform-2(merge: context.fetchArticle.input=meta, $(input.body)=article HTML)
 *           → mongo_upsert → redis_publish
 *    error → notify
 */
function buildAljazeeraGraph(scraper: ScraperConfig): { nodes: Node[]; edges: Edge[]; params: Record<string, string> } {
  const params = {
    feedURL:    scraper.feed_url,
    platform:   'aljazeera',
    baseURL:    'https://www.aljazeera.com',
    collection: 'news_articles',
    channel:    'immaiwin:news:articles',
  }

  // http_fetch-1 named "fetchHomepage" → context.fetchHomepage.output.body = raw HTML
  const parseLinksScript = `// context.fetchHomepage.output = { ok, status, body } — raw homepage HTML
// Extract article links from h3 headings → return [{url, title}]
var articles = [];
$(context.fetchHomepage.output.body).find('h3').each(function(i, el) {
  var title = el.text().trim();
  if (!title) return;
  var a = el.find('a');
  var link = a.length ? (a.attr('href') || '') : '';
  if (!link) return;
  if (link.indexOf('/') === 0) link = params.baseURL + link;
  if (link.indexOf('http') !== 0) return;
  articles.push({ url: link, title: title });
});
return articles;`

  // http_fetch-2 named "fetchArticle":
  //   context.fetchArticle.input  = {url, title} — for_each item passed into http_fetch-2
  //   context.fetchArticle.output = {ok, status, body} — article page response
  const mergeBodyScript = `// context.fetchArticle.input  = { url, title } — for_each item passed into http_fetch-2
// context.fetchArticle.output = { ok, status, body } — article page response
var meta = context.fetchArticle.input;
var body = '';
var selectors = [
  '[data-testid="ArticleBodyParagraph"]',
  '.wysiwyg--all-content',
  '.article-p-wrapper',
  'article',
];
for (var s = 0; s < selectors.length; s++) {
  var container = $(context.fetchArticle.output.body).find(selectors[s]).first();
  if (container.length > 0) {
    body = container.text().trim();
    break;
  }
}
return {
  url:        meta.url,
  title:      meta.title,
  body:       body,
  scraped_at: now(),
  platform:   params.platform,
};`

  const nodes: Node[] = [
    { id: 'trigger-1',      type: 'trigger',      position: { x: 0,    y: 80  }, data: {} },
    { id: 'http_fetch-1',   type: 'http_fetch',   position: { x: 220,  y: 60  }, data: { url: '{{params.feedURL}}', name: 'fetchHomepage' } },
    { id: 'js_transform-1', type: 'js_transform', position: { x: 480,  y: 40  }, data: { script: parseLinksScript } },
    { id: 'for_each-1',     type: 'for_each',     position: { x: 760,  y: 50  }, data: { name: 'forEachArticle' } },
    { id: 'http_fetch-2',   type: 'http_fetch',   position: { x: 1000, y: 20  }, data: { url: '{{context.forEachArticle.item.url}}', name: 'fetchArticle' } },
    { id: 'js_transform-2', type: 'js_transform', position: { x: 1260, y: 20  }, data: { script: mergeBodyScript } },
    { id: 'mongo_upsert-1', type: 'mongo_upsert', position: { x: 1520, y: 20  }, data: { collection: '{{params.collection}}', filter_field: 'url' } },
    { id: 'redis_publish-1',type: 'redis_publish',position: { x: 1780, y: 20  }, data: { channel: '{{params.channel}}' } },
    { id: 'notify-1',       type: 'notify',       position: { x: 760,  y: 260 }, data: { message: '{{params.platform}}: pipeline error' } },
  ]
  const edges: Edge[] = [
    { id: 'e1',  source: 'trigger-1',      target: 'http_fetch-1' },
    { id: 'e2',  source: 'http_fetch-1',   target: 'js_transform-1', sourceHandle: 'success' },
    { id: 'e3',  source: 'http_fetch-1',   target: 'notify-1',       sourceHandle: 'error', targetHandle: 'in-top' },
    { id: 'e4',  source: 'js_transform-1', target: 'for_each-1',     sourceHandle: 'success' },
    { id: 'e5',  source: 'js_transform-1', target: 'notify-1',       sourceHandle: 'error', targetHandle: 'in-left' },
    // for_each body: fetch each article URL then transform + upsert + publish
    { id: 'e6',  source: 'for_each-1',     target: 'http_fetch-2',   sourceHandle: 'item' },
    { id: 'e7',  source: 'for_each-1',     target: 'notify-1',       sourceHandle: 'error', targetHandle: 'in-bottom' },
    { id: 'e8',  source: 'http_fetch-2',   target: 'js_transform-2', sourceHandle: 'success' },
    { id: 'e9',  source: 'js_transform-2', target: 'mongo_upsert-1', sourceHandle: 'success' },
    { id: 'e10', source: 'mongo_upsert-1', target: 'redis_publish-1',sourceHandle: 'success' },
  ]
  return { nodes, edges, params }
}

function WorkflowsPage() {
  const { workflows, activeId, setWorkflows, setActive, activeWorkflow } = useWorkflowStore()
  const [lastRun, setLastRun] = useState<RunResults | null>(null)

  const load = useCallback(async () => {
    try {
      const [wfRes, scRes] = await Promise.all([
        fetch(`${API_BASE}/api/v1/workflows`),
        fetch(`${API_BASE}/api/v1/news/scrapers`),
      ])
      const wfs: Workflow[] = await wfRes.json()
      const scrapers: ScraperConfig[] = await scRes.json()

      // seed missing workflows
      const existingNames = new Set(wfs.map((w) => w.name))
      const toSeed = scrapers.filter((s) => !existingNames.has(s.source))

      if (toSeed.length > 0) {
        const seeded = await Promise.all(
          toSeed.map(async (s) => {
            const wf = buildSeedWorkflow(s)
            const res = await fetch(`${API_BASE}/api/v1/workflows/${wf.id}`, {
              method: 'PUT',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify(wf),
            })
            return res.ok ? ((await res.json()) as Workflow) : null
          }),
        )
        const saved = seeded.filter(Boolean) as Workflow[]
        setWorkflows([...wfs, ...saved])
      } else {
        setWorkflows(wfs)
      }
    } catch {
      toast.error('Failed to load workflows')
    }
  }, [setWorkflows])

  useEffect(() => {
    load()
  }, [load])

  // auto-select first workflow
  useEffect(() => {
    if (workflows.length > 0 && !activeId) {
      setActive(workflows[0].id)
    }
  }, [workflows, activeId, setActive])

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

  async function handleRun(stopAt?: string) {
    const wf = activeWorkflow()
    if (!wf) return
    setLastRun(null) // clear stale results immediately before new run
    try {
      const res = await fetch(`${API_BASE}/api/v1/workflows/${wf.id}/run`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: stopAt ? JSON.stringify({ stop_at: stopAt }) : undefined,
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
        grouped[step.node_id].push(step)
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
        <WorkflowSidebar onSelect={setActive} />
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

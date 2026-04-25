import { useCallback, useEffect, useRef, useState } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import { Badge } from '~/components/ui/badge'
import { Card, CardContent, CardHeader } from '~/components/ui/card'
import { ScrollArea } from '~/components/ui/scroll-area'
import { Skeleton } from '~/components/ui/skeleton'
import { Newspaper } from 'lucide-react'

const API_BASE = import.meta.env['VITE_API_URL'] ?? 'http://localhost:8080'

interface Article {
  id: string
  platform: string
  url: string
  title: string
  body?: string
  scraped_at: string
}

function mergeArticle(prev: Article[], incoming: Article): Article[] {
  if (prev.some((a) => a.url === incoming.url)) return prev
  return [incoming, ...prev].slice(0, 500)
}

export function NewsFeed() {
  const [articles, setArticles] = useState<Article[]>([])
  const [connected, setConnected] = useState(false)
  const [loading, setLoading] = useState(true)
  const viewportRef = useRef<HTMLDivElement>(null)

  const virtualizer = useVirtualizer({
    count: articles.length,
    getScrollElement: () => viewportRef.current,
    estimateSize: () => 120,
    overscan: 5,
    getItemKey: (index) => articles[index]!.url,
  })

  const fetchHistory = useCallback(() => {
    fetch(`${API_BASE}/api/v1/news`)
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`)
        return r.json() as Promise<Article[]>
      })
      .then((data) => {
        setArticles(data ?? [])
        setLoading(false)
      })
      .catch(() => setLoading(false))
  }, [])

  useEffect(() => { fetchHistory() }, [fetchHistory])

  useEffect(() => {
    const es = new EventSource(`${API_BASE}/api/v1/news/stream`)
    es.addEventListener('article', (e: MessageEvent) => {
      const article = JSON.parse(e.data as string) as Article
      setArticles((prev) => mergeArticle(prev, article))
    })
    es.onopen = () => setConnected(true)
    es.onerror = () => setConnected(false)
    return () => { es.close() }
  }, [])

  return (
    <div className="flex flex-col h-full">
      <div className="flex items-center justify-between px-1 pb-2 shrink-0">
        <span className="text-sm text-muted-foreground">Latest articles</span>
        <Badge variant={connected ? 'default' : 'destructive'} className="gap-1.5">
          <span className={`h-1.5 w-1.5 rounded-full ${connected ? 'bg-green-400' : 'bg-red-400'}`} />
          {connected ? 'Live' : 'Disconnected'}
        </Badge>
      </div>

      <div className="flex-1 overflow-hidden min-h-0">
        <ScrollArea className="h-full" viewportRef={viewportRef}>
          {loading ? (
            <div className="pr-4"><LoadingState /></div>
          ) : articles.length === 0 ? (
            <EmptyState />
          ) : (
            <div style={{ height: `${virtualizer.getTotalSize()}px`, position: 'relative' }} className="pr-4">
              {virtualizer.getVirtualItems().map((virtualRow) => (
                <div
                  key={virtualRow.key}
                  data-index={virtualRow.index}
                  ref={virtualizer.measureElement}
                  style={{
                    position: 'absolute',
                    top: 0,
                    left: 0,
                    width: '100%',
                    transform: `translateY(${virtualRow.start}px)`,
                    paddingBottom: '12px',
                  }}
                  className="animate-in fade-in-0 slide-in-from-top-3 duration-300"
                >
                  <ArticleCard article={articles[virtualRow.index]!} />
                </div>
              ))}
            </div>
          )}
        </ScrollArea>
      </div>
    </div>
  )
}

function ArticleCard({ article }: { article: Article }) {
  const scrapedAt = new Date(article.scraped_at)
  return (
    <Card className="gap-3 py-4">
      <CardHeader className="px-4 pb-0">
        <div className="flex items-start justify-between gap-3">
          <div className="flex items-center gap-2 min-w-0">
            <Badge variant="secondary" className="shrink-0 text-xs capitalize">
              {article.platform}
            </Badge>
            <a
              href={article.url}
              target="_blank"
              rel="noopener noreferrer"
              className="text-sm font-medium leading-snug hover:underline"
            >
              {article.title}
            </a>
          </div>
          <time
            className="text-xs text-muted-foreground whitespace-nowrap shrink-0"
            dateTime={article.scraped_at}
            title={scrapedAt.toISOString()}
          >
            {scrapedAt.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })}
          </time>
        </div>
      </CardHeader>
      {article.body && (
        <CardContent className="px-4">
          <p className="text-xs text-muted-foreground leading-relaxed line-clamp-3">
            {article.body}
          </p>
        </CardContent>
      )}
    </Card>
  )
}

function LoadingState() {
  return (
    <>
      {[...Array(4)].map((_, i) => (
        <Card key={i} className="gap-3 py-4 mb-3">
          <CardHeader className="px-4 pb-0">
            <div className="flex items-center justify-between gap-3">
              <div className="flex items-center gap-2">
                <Skeleton className="h-5 w-16 rounded-md" />
                <Skeleton className="h-4 w-64" />
              </div>
              <Skeleton className="h-3 w-16" />
            </div>
          </CardHeader>
          <CardContent className="px-4">
            <Skeleton className="h-10 w-full" />
          </CardContent>
        </Card>
      ))}
    </>
  )
}

function EmptyState() {
  return (
    <Card className="py-16">
      <CardContent className="flex flex-col items-center text-center gap-2">
        <Newspaper className="h-8 w-8 text-muted-foreground/40" />
        <p className="font-medium text-muted-foreground">Waiting for news…</p>
        <p className="text-sm text-muted-foreground/60">
          Articles stream here as the scraper discovers them.
        </p>
      </CardContent>
    </Card>
  )
}

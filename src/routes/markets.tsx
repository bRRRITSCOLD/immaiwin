import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { useCallback, useEffect, useRef, useState } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import { ChevronDown, ChevronUp, Search, Star } from 'lucide-react'
import { Badge } from '~/components/ui/badge'
import { Button } from '~/components/ui/button'
import { Card, CardContent, CardHeader } from '~/components/ui/card'
import { Input } from '~/components/ui/input'
import {
  Pagination,
  PaginationContent,
  PaginationItem,
  PaginationLink,
  PaginationNext,
  PaginationPrevious,
} from '~/components/ui/pagination'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '~/components/ui/select'
import { ScrollArea } from '~/components/ui/scroll-area'
import { Separator } from '~/components/ui/separator'
import { Skeleton } from '~/components/ui/skeleton'

const API_BASE = import.meta.env['VITE_API_URL'] ?? 'http://localhost:8080'
const PAGE_SIZE = 20

interface Market {
  id: string
  question: string
  slug: string
  endDate: string
  liquidity: string
  volume: string
  volume24hr: string
  active: boolean
  closed: boolean
  lastTradePrice: string
  bestBid: string
  bestAsk: string
  image: string
  category: string
  outcomes: string
  outcomePrices: string
}

interface MarketEvent {
  id: string
  slug: string
  title: string
  endDate: string
  active: boolean
  closed: boolean
  liquidity: string
  volume: string
  volume24hr: string
  category: string
  markets: Market[]
}

type StatusFilter = 'all' | 'active' | 'closed'
type SortField = 'volume_num' | 'volume24hr_num' | 'liquidity_num' | 'end_date' | 'default'

interface MarketsSearch {
  q?: string
  status?: StatusFilter
  sort?: SortField
  page?: number
}

const VALID_SORTS: SortField[] = ['volume_num', 'volume24hr_num', 'liquidity_num', 'end_date', 'default']

// Maps UI sort values to Polymarket API order field names
const sortToOrder: Record<string, string> = {
  volume_num: 'volume',
  volume24hr_num: 'volume24hr',
  liquidity_num: 'liquidity',
  end_date: 'end_date',
}

export const Route = createFileRoute('/markets')({
  validateSearch: (search: Record<string, unknown>): MarketsSearch => ({
    q: typeof search['q'] === 'string' ? search['q'] : undefined,
    status:
      search['status'] === 'active' || search['status'] === 'closed'
        ? search['status']
        : 'all',
    sort: VALID_SORTS.includes(search['sort'] as SortField)
      ? (search['sort'] as SortField)
      : 'volume_num',
    page: typeof search['page'] === 'number' ? search['page'] : 1,
  }),
  component: MarketsPage,
})

function todayYMD(): string {
  return new Date().toISOString().slice(0, 10)
}

function buildUrl(q: string, status: StatusFilter, sort: SortField, page: number): string {
  if (q.trim()) {
    const params = new URLSearchParams()
    params.set('q', q.trim())
    params.set('page', String(page))
    if (status !== 'all') params.set('status', status)
    if (sort !== 'default') params.set('sort', sort)
    return `${API_BASE}/api/v1/events/search?${params}`
  }
  const params = new URLSearchParams()
  params.set('limit', String(PAGE_SIZE))
  params.set('offset', String((page - 1) * PAGE_SIZE))
  if (status === 'active') { params.set('active', 'true'); params.set('end_date_min', todayYMD()) }
  if (status === 'closed') params.set('closed', 'true')
  if (sort !== 'default') { params.set('order', sortToOrder[sort] ?? sort); params.set('ascending', 'false') }
  return `${API_BASE}/api/v1/events?${params}`
}

function MarketsPage() {
  const search = Route.useSearch()
  const navigate = useNavigate({ from: '/markets' })

  const q = search.q ?? ''
  const status = (search.status ?? 'all') as StatusFilter
  const sort = (search.sort ?? 'volume_num') as SortField
  const page = search.page ?? 1

  const [inputValue, setInputValue] = useState(q)
  const [events, setEvents] = useState<MarketEvent[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [hasMore, setHasMore] = useState(false)

  // watchlist: set of market IDs currently saved, plus pending local changes
  const [watchlistIds, setWatchlistIds] = useState<Set<string>>(new Set())
  const [watchlistData, setWatchlistData] = useState<Map<string, { question: string; slug: string }>>(new Map())
  const [pendingIds, setPendingIds] = useState<Set<string> | null>(null)
  const [saving, setSaving] = useState(false)

  const activeIds = pendingIds ?? watchlistIds

  // Flat list of all markets across all visible events (for watchlist save)
  const allMarkets = events.flatMap((e) => e.markets)

  const eventsViewportRef = useRef<HTMLDivElement>(null)
  const eventsVirtualizer = useVirtualizer({
    count: events.length,
    getScrollElement: () => eventsViewportRef.current,
    estimateSize: () => 220,
    overscan: 3,
  })

  const setParam = useCallback(
    (updates: Partial<MarketsSearch>) => {
      void navigate({
        search: (prev) => ({ ...prev, ...updates }),
        replace: true,
      })
    },
    [navigate],
  )

  useEffect(() => {
    setInputValue(q)
  }, [q])

  // Load watchlist once on mount
  useEffect(() => {
    fetch(`${API_BASE}/api/v1/watchlist`)
      .then((r) => r.json() as Promise<Array<{ market_id: string; question: string; slug: string }>>)
      .then((data) => {
        setWatchlistIds(new Set(data.map((d) => d.market_id)))
        setWatchlistData(new Map(data.map((d) => [d.market_id, { question: d.question, slug: d.slug }])))
      })
      .catch(() => {/* non-fatal */})
  }, [])

  const toggleWatchlist = useCallback((market: Market) => {
    setPendingIds((prev) => {
      const base = prev ?? watchlistIds
      const next = new Set(base)
      if (next.has(market.id)) {
        next.delete(market.id)
      } else {
        next.add(market.id)
      }
      return next
    })
  }, [watchlistIds])

  const saveWatchlist = useCallback(() => {
    if (pendingIds === null) return
    setSaving(true)
    const pageMarketsMap = new Map(allMarkets.map((m) => [m.id, { question: m.question, slug: m.slug }]))
    const body = [...pendingIds].flatMap((id) => {
      const fromPage = pageMarketsMap.get(id)
      if (fromPage) return [{ market_id: id, ...fromPage }]
      const fromWatchlist = watchlistData.get(id)
      if (fromWatchlist) return [{ market_id: id, ...fromWatchlist }]
      return []
    })

    fetch(`${API_BASE}/api/v1/watchlist`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`)
        setWatchlistIds(pendingIds)
        setPendingIds(null)
      })
      .catch(() => {/* keep pending so user can retry */})
      .finally(() => setSaving(false))
  }, [pendingIds, allMarkets, watchlistData])

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    setError(null)

    fetch(buildUrl(q, status, sort, page))
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`)
        return r.json() as Promise<MarketEvent[]>
      })
      .then((data) => {
        if (cancelled) return
        setEvents(data ?? [])
        setHasMore((data ?? []).length === PAGE_SIZE)
        setLoading(false)
      })
      .catch((err: unknown) => {
        if (cancelled) return
        setError(err instanceof Error ? err.message : 'Unknown error')
        setLoading(false)
      })

    return () => {
      cancelled = true
    }
  }, [q, status, sort, page])

  const handleSearch = useCallback(() => {
    setParam({ q: inputValue || undefined, page: 1 })
  }, [inputValue, setParam])

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLInputElement>) => {
      if (e.key === 'Enter') handleSearch()
    },
    [handleSearch],
  )

  return (
    <div className="min-h-screen bg-background text-foreground">
      <header className="sticky top-0 z-10 border-b bg-background/90 backdrop-blur-sm px-6 py-3 flex items-center justify-between">
        <div className="flex items-center gap-4">
          <Link to="/" className="text-lg font-semibold tracking-tight">
            immaiwin
          </Link>
          <Separator orientation="vertical" className="h-5" />
          <nav className="flex items-center gap-3 text-sm">
            <Link to="/" className="text-muted-foreground hover:text-foreground transition-colors">
              Trades
            </Link>
            <Link to="/markets" className="text-foreground font-medium">
              Markets
            </Link>
            <Link to="/watchlist" className="text-muted-foreground hover:text-foreground transition-colors">
              Watchlist
            </Link>
          </nav>
        </div>
      </header>

      <main className="max-w-4xl mx-auto px-4 py-6 space-y-5">
        {/* Search + filters */}
        <div className="flex gap-2">
          <div className="relative flex-1">
            <Search className="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground pointer-events-none" />
            <Input
              className="pl-9"
              placeholder="Search markets…"
              value={inputValue}
              onChange={(e) => setInputValue(e.target.value)}
              onKeyDown={handleKeyDown}
            />
          </div>
          <Select
            value={status}
            onValueChange={(v) => setParam({ status: v as StatusFilter, page: 1 })}
          >
            <SelectTrigger className="w-36">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">All</SelectItem>
              <SelectItem value="active">Active</SelectItem>
              <SelectItem value="closed">Closed</SelectItem>
            </SelectContent>
          </Select>
          <Select
            value={sort}
            onValueChange={(v) => setParam({ sort: v as SortField, page: 1 })}
          >
            <SelectTrigger className="w-44">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="volume_num">Volume (high→low)</SelectItem>
              <SelectItem value="volume24hr_num">Vol 24h (high→low)</SelectItem>
              <SelectItem value="liquidity_num">Liquidity (high→low)</SelectItem>
              <SelectItem value="end_date">End date</SelectItem>
              <SelectItem value="default">Default</SelectItem>
            </SelectContent>
          </Select>
          <Button onClick={handleSearch} variant="default">
            Search
          </Button>
          {pendingIds !== null && (
            <Button onClick={saveWatchlist} variant="secondary" disabled={saving}>
              {saving ? 'Saving…' : 'Save watchlist'}
            </Button>
          )}
        </div>

        {/* Results */}
        {loading ? (
          <LoadingState />
        ) : error ? (
          <ErrorState message={error} />
        ) : events.length === 0 ? (
          <EmptyState />
        ) : (
          <ScrollArea className="h-[calc(100vh-14rem)]" viewportRef={eventsViewportRef}>
            <div style={{ height: `${eventsVirtualizer.getTotalSize()}px`, position: 'relative' }}>
              {eventsVirtualizer.getVirtualItems().map((virtualRow) => (
                <div
                  key={virtualRow.key}
                  data-index={virtualRow.index}
                  ref={eventsVirtualizer.measureElement}
                  style={{
                    position: 'absolute',
                    top: 0,
                    left: 0,
                    width: '100%',
                    transform: `translateY(${virtualRow.start}px)`,
                    paddingBottom: '12px',
                  }}
                >
                  <EventCard
                    event={events[virtualRow.index]!}
                    watchedIds={activeIds}
                    onToggleWatch={toggleWatchlist}
                  />
                </div>
              ))}
            </div>
          </ScrollArea>
        )}

        {/* Pagination */}
        {!loading && !error && (events.length > 0 || page > 1) && (
          <EventsPagination
            page={page}
            hasMore={hasMore}
            onPage={(p) => setParam({ page: p })}
          />
        )}
      </main>
    </div>
  )
}

const MARKETS_PREVIEW = 3

function EventCard({
  event,
  watchedIds,
  onToggleWatch,
}: {
  event: MarketEvent
  watchedIds: Set<string>
  onToggleWatch: (m: Market) => void
}) {
  const [expanded, setExpanded] = useState(false)
  const markets = event.markets ?? []
  const visible = expanded ? markets : markets.slice(0, MARKETS_PREVIEW)
  const hasMore = markets.length > MARKETS_PREVIEW

  return (
    <Card className="py-4 gap-0">
      <CardHeader className="px-4 pb-2">
        <div className="flex items-start justify-between gap-3">
          <div className="flex-1 min-w-0">
            <p className="font-semibold leading-snug">{event.title}</p>
            {event.category && (
              <p className="text-xs text-muted-foreground mt-0.5">{event.category}</p>
            )}
          </div>
          <div className="flex items-center gap-1.5 shrink-0">
            {event.active && !event.closed && (
              <Badge variant="default" className="text-xs">Active</Badge>
            )}
            {event.closed && (
              <Badge variant="secondary" className="text-xs">Closed</Badge>
            )}
          </div>
        </div>

        <div className="grid grid-cols-3 gap-2 mt-2">
          <Stat label="Vol 24h" value={formatDecimal(event.volume24hr, 0)} />
          <Stat label="Liquidity" value={formatDecimal(event.liquidity, 0)} />
          {event.endDate && (
            <Stat label="Ends" value={formatDate(event.endDate)} />
          )}
        </div>
      </CardHeader>

      {markets.length > 0 && (
        <CardContent className="px-4 pt-0 space-y-0">
          <Separator className="mb-3" />
          {expanded ? (
            <div className="max-h-72 overflow-y-auto space-y-2 pb-1">
              {markets.map((m) => (
                <MarketRow
                  key={m.id}
                  market={m}
                  watched={watchedIds.has(m.id)}
                  onToggleWatch={() => onToggleWatch(m)}
                />
              ))}
            </div>
          ) : (
            <div className="space-y-2">
              {visible.map((m) => (
                <MarketRow
                  key={m.id}
                  market={m}
                  watched={watchedIds.has(m.id)}
                  onToggleWatch={() => onToggleWatch(m)}
                />
              ))}
            </div>
          )}
          {hasMore && (
            <button
              onClick={() => setExpanded((v) => !v)}
              className="mt-2 flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground transition-colors"
            >
              {expanded ? (
                <><ChevronUp className="h-3 w-3" /> Show less</>
              ) : (
                <><ChevronDown className="h-3 w-3" /> {markets.length - MARKETS_PREVIEW} more</>
              )}
            </button>
          )}
        </CardContent>
      )}
    </Card>
  )
}

function MarketRow({
  market,
  watched,
  onToggleWatch,
}: {
  market: Market
  watched: boolean
  onToggleWatch: () => void
}) {
  const outcomes = tryParseArray(market.outcomes)
  const prices = tryParseArray(market.outcomePrices)

  return (
    <div className="rounded-md border bg-muted/30 px-3 py-2 space-y-1.5">
      <div className="flex items-start justify-between gap-2">
        <p className="text-sm leading-snug flex-1 min-w-0">{market.question}</p>
        <button
          onClick={onToggleWatch}
          className="p-0.5 rounded hover:bg-muted transition-colors shrink-0 mt-0.5"
          aria-label={watched ? 'Remove from watchlist' : 'Add to watchlist'}
        >
          <Star
            className={`h-3.5 w-3.5 ${watched ? 'fill-yellow-400 text-yellow-400' : 'text-muted-foreground'}`}
          />
        </button>
      </div>

      <div className="flex items-center gap-4 flex-wrap">
        <MiniStat label="Price" value={formatDecimal(market.lastTradePrice, 4)} />
        <MiniStat label="Vol 24h" value={formatDecimal(market.volume24hr, 0)} />
        <MiniStat label="Bid/Ask" value={`${formatDecimal(market.bestBid, 3)} / ${formatDecimal(market.bestAsk, 3)}`} />
        {market.endDate && <MiniStat label="Ends" value={formatDate(market.endDate)} />}
        {outcomes.length > 0 && outcomes.map((o, i) => (
          <MiniStat key={i} label={o} value={prices[i] !== undefined ? formatDecimal(prices[i] ?? '', 3) : '—'} />
        ))}
      </div>
    </div>
  )
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <p className="text-xs text-muted-foreground uppercase tracking-wider mb-0.5">{label}</p>
      <p className="font-mono text-sm font-medium tabular-nums text-foreground">{value || '—'}</p>
    </div>
  )
}

function MiniStat({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center gap-1">
      <span className="text-xs text-muted-foreground">{label}:</span>
      <span className="font-mono text-xs font-medium tabular-nums">{value || '—'}</span>
    </div>
  )
}

function EventsPagination({
  page,
  hasMore,
  onPage,
}: {
  page: number
  hasMore: boolean
  onPage: (p: number) => void
}) {
  return (
    <Pagination>
      <PaginationContent>
        <PaginationItem>
          <PaginationPrevious
            onClick={() => onPage(page - 1)}
            aria-disabled={page <= 1}
            className={page <= 1 ? 'pointer-events-none opacity-50' : 'cursor-pointer'}
          />
        </PaginationItem>
        <PaginationItem>
          <PaginationLink isActive>{page}</PaginationLink>
        </PaginationItem>
        <PaginationItem>
          <PaginationNext
            onClick={() => onPage(page + 1)}
            aria-disabled={!hasMore}
            className={!hasMore ? 'pointer-events-none opacity-50' : 'cursor-pointer'}
          />
        </PaginationItem>
      </PaginationContent>
    </Pagination>
  )
}

function LoadingState() {
  return (
    <div className="space-y-3">
      {[...Array(5)].map((_, i) => (
        <Card key={i} className="py-4 gap-3">
          <CardHeader className="px-4 pb-0">
            <div className="flex items-start justify-between gap-3">
              <div className="flex-1 space-y-2">
                <Skeleton className="h-4 w-3/4" />
                <Skeleton className="h-3 w-1/4" />
              </div>
              <Skeleton className="h-5 w-14 rounded-md" />
            </div>
            <div className="grid grid-cols-3 gap-2 mt-2">
              <Skeleton className="h-9" />
              <Skeleton className="h-9" />
              <Skeleton className="h-9" />
            </div>
          </CardHeader>
          <CardContent className="px-4">
            <Skeleton className="h-16 w-full rounded-md" />
          </CardContent>
        </Card>
      ))}
    </div>
  )
}

function EmptyState() {
  return (
    <Card className="py-16">
      <CardContent className="flex flex-col items-center text-center gap-2">
        <p className="font-medium text-muted-foreground">No markets found</p>
        <p className="text-sm text-muted-foreground/60">Try adjusting your search or filters.</p>
      </CardContent>
    </Card>
  )
}

function ErrorState({ message }: { message: string }) {
  return (
    <Card className="py-12">
      <CardContent className="flex flex-col items-center text-center gap-2">
        <p className="font-medium text-destructive">Failed to load markets</p>
        <p className="text-sm text-muted-foreground font-mono">{message}</p>
      </CardContent>
    </Card>
  )
}

function tryParseArray(raw: string): string[] {
  if (!raw) return []
  try {
    const parsed: unknown = JSON.parse(raw)
    if (Array.isArray(parsed)) return parsed as string[]
    return []
  } catch {
    return []
  }
}

function formatDecimal(raw: string, decimals: number): string {
  if (!raw || raw === '0') return '—'
  const n = parseFloat(raw)
  if (isNaN(n) || n === 0) return '—'
  return n.toLocaleString(undefined, { maximumFractionDigits: decimals })
}

function formatDate(raw: string): string {
  if (!raw) return ''
  try {
    return new Date(raw).toLocaleDateString(undefined, {
      year: 'numeric',
      month: 'short',
      day: 'numeric',
    })
  } catch {
    return raw
  }
}

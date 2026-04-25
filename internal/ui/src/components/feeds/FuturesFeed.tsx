import { useCallback, useEffect, useRef, useState } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import { Plus, Trash2, Zap } from 'lucide-react'
import { Button } from '~/components/ui/button'
import { Card, CardContent, CardHeader } from '~/components/ui/card'
import { Input } from '~/components/ui/input'
import { ScrollArea } from '~/components/ui/scroll-area'
import { Separator } from '~/components/ui/separator'
import { Skeleton } from '~/components/ui/skeleton'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '~/components/ui/tabs'
import { useSchwabAuth, SchwabAuthBar } from '~/components/schwab-auth'

const API_BASE = import.meta.env['VITE_API_URL'] ?? 'http://localhost:8080'

interface FuturesWatchlistItem {
  symbol: string
  added_at: string
}

interface FuturesEvent {
  symbol: string
  root: string
  price: number
  size: number
  volume: number
  oi: number
  unusual: boolean
  reason?: string
  detected_at: string
}

export function FuturesFeed() {
  const { authorized, disconnecting, disconnect } = useSchwabAuth()

  const [watchlist, setWatchlist] = useState<FuturesWatchlistItem[]>([])
  const [wlLoading, setWlLoading] = useState(true)
  const [wlError, setWlError] = useState<string | null>(null)
  const [addInput, setAddInput] = useState('')
  const [saving, setSaving] = useState(false)

  const [events, setEvents] = useState<FuturesEvent[]>([])
  const [connected, setConnected] = useState(false)
  const [streamLoading, setStreamLoading] = useState(true)
  const viewportRef = useRef<HTMLDivElement>(null)

  const virtualizer = useVirtualizer({
    count: events.length,
    getScrollElement: () => viewportRef.current,
    estimateSize: () => 110,
    overscan: 5,
    getItemKey: (i) => `${events[i]!.symbol}-${events[i]!.detected_at}`,
  })

  const fetchWatchlist = useCallback(() => {
    setWlLoading(true)
    setWlError(null)
    fetch(`${API_BASE}/api/v1/futures/watchlist`)
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`)
        return r.json() as Promise<FuturesWatchlistItem[]>
      })
      .then((data) => {
        setWatchlist(data ?? [])
        setWlLoading(false)
      })
      .catch((err: unknown) => {
        setWlError(err instanceof Error ? err.message : 'Unknown error')
        setWlLoading(false)
      })
  }, [])

  useEffect(() => { fetchWatchlist() }, [fetchWatchlist])

  useEffect(() => {
    const es = new EventSource(`${API_BASE}/api/v1/futures/stream`)
    es.addEventListener('future', (e: MessageEvent) => {
      const ev = JSON.parse(e.data as string) as FuturesEvent
      setStreamLoading(false)
      setEvents((prev) => [ev, ...prev].slice(0, 500))
    })
    es.onopen = () => {
      setConnected(true)
      setTimeout(() => setStreamLoading(false), 3000)
    }
    es.onerror = () => setConnected(false)
    return () => { es.close() }
  }, [])

  const syncWatchlist = useCallback((symbols: string[]) => {
    setSaving(true)
    return fetch(`${API_BASE}/api/v1/futures/watchlist`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ symbols }),
    })
      .then((r) => { if (!r.ok) throw new Error(`HTTP ${r.status}`) })
      .finally(() => setSaving(false))
  }, [])

  const addSymbol = useCallback(() => {
    const sym = addInput.trim().toUpperCase()
    if (!sym || watchlist.some((w) => w.symbol === sym)) { setAddInput(''); return }
    const next = [...watchlist.map((w) => w.symbol), sym]
    syncWatchlist(next).then(() => {
      setWatchlist((prev) => [...prev, { symbol: sym, added_at: new Date().toISOString() }])
      setAddInput('')
    }).catch(() => {})
  }, [addInput, watchlist, syncWatchlist])

  const removeSymbol = useCallback((sym: string) => {
    const next = watchlist.filter((w) => w.symbol !== sym).map((w) => w.symbol)
    syncWatchlist(next).then(() => {
      setWatchlist((prev) => prev.filter((w) => w.symbol !== sym))
    }).catch(() => {})
  }, [watchlist, syncWatchlist])

  return (
    <div className="flex flex-col h-full">
      <SchwabAuthBar
        authorized={authorized}
        disconnecting={disconnecting}
        disconnect={disconnect}
        connected={connected}
      />

      <Tabs defaultValue="feed" className="flex flex-col flex-1 overflow-hidden">
        <TabsList className="mb-2 shrink-0">
          <TabsTrigger value="feed">Live Feed</TabsTrigger>
          <TabsTrigger value="watchlist">Watchlist</TabsTrigger>
        </TabsList>

        <TabsContent value="feed" className="flex-1 overflow-hidden mt-0 data-[state=inactive]:hidden">
          <ScrollArea className="h-full" viewportRef={viewportRef}>
            {streamLoading ? (
              <div className="pr-4"><FeedLoadingState /></div>
            ) : events.length === 0 ? (
              <FeedEmptyState authorized={authorized ?? false} />
            ) : (
              <div style={{ height: `${virtualizer.getTotalSize()}px`, position: 'relative' }} className="pr-4">
                {virtualizer.getVirtualItems().map((vr) => (
                  <div
                    key={vr.key}
                    data-index={vr.index}
                    ref={virtualizer.measureElement}
                    style={{
                      position: 'absolute',
                      top: 0,
                      left: 0,
                      width: '100%',
                      transform: `translateY(${vr.start}px)`,
                      paddingBottom: '12px',
                    }}
                    className="animate-in fade-in-0 slide-in-from-top-3 duration-300"
                  >
                    <FuturesEventCard event={events[vr.index]!} />
                  </div>
                ))}
              </div>
            )}
          </ScrollArea>
        </TabsContent>

        <TabsContent value="watchlist" className="flex-1 overflow-hidden mt-0 data-[state=inactive]:hidden">
          <ScrollArea className="h-full">
            <div className="space-y-4 pr-4">
              <div className="flex items-center gap-2">
                <Input
                  className="max-w-xs font-mono uppercase"
                  placeholder="/CL, /ES, /NQ…"
                  value={addInput}
                  onChange={(e) => setAddInput(e.target.value.toUpperCase())}
                  onKeyDown={(e) => { if (e.key === 'Enter') addSymbol() }}
                  disabled={saving}
                />
                <Button size="sm" onClick={addSymbol} disabled={saving || !addInput.trim()} className="gap-1.5">
                  <Plus className="h-3.5 w-3.5" />
                  Add
                </Button>
              </div>
              {wlLoading ? (
                <WatchlistLoadingState />
              ) : wlError ? (
                <ErrorState message={wlError} />
              ) : watchlist.length === 0 ? (
                <WatchlistEmptyState />
              ) : (
                <div className="space-y-2">
                  {watchlist.map((item) => (
                    <Card key={item.symbol} className="py-3">
                      <CardContent className="px-4 flex items-center justify-between gap-3">
                        <div>
                          <p className="font-mono font-semibold text-sm">{item.symbol}</p>
                          <p className="text-xs text-muted-foreground">Added {formatDate(item.added_at)}</p>
                        </div>
                        <Button
                          variant="ghost"
                          size="icon"
                          className="h-7 w-7 text-muted-foreground hover:text-destructive"
                          onClick={() => removeSymbol(item.symbol)}
                          disabled={saving}
                        >
                          <Trash2 className="h-3.5 w-3.5" />
                        </Button>
                      </CardContent>
                    </Card>
                  ))}
                </div>
              )}
            </div>
          </ScrollArea>
        </TabsContent>
      </Tabs>
    </div>
  )
}

function FuturesEventCard({ event }: { event: FuturesEvent }) {
  return (
    <Card className="gap-3 py-4">
      <CardHeader className="px-4 pb-0">
        <div className="flex items-start justify-between gap-3">
          <div className="flex items-center gap-2 flex-wrap min-w-0">
            <span className="font-mono font-semibold text-sm">{event.symbol}</span>
            <span className="text-xs text-muted-foreground">{event.root}</span>
          </div>
          <time className="text-xs text-muted-foreground whitespace-nowrap shrink-0">
            {new Date(event.detected_at).toLocaleTimeString()}
          </time>
        </div>
      </CardHeader>
      <CardContent className="px-4">
        <div className="grid grid-cols-4 gap-2">
          <Stat label="Price" value={event.price > 0 ? event.price.toFixed(2) : '—'} />
          <Stat label="Size" value={event.size > 0 ? event.size.toLocaleString() : '—'} />
          <Stat label="Volume" value={event.volume > 0 ? event.volume.toLocaleString() : '—'} />
          <Stat label="OI" value={event.oi > 0 ? event.oi.toLocaleString() : '—'} />
        </div>
        {event.unusual && event.reason && (
          <>
            <Separator className="mt-2" />
            <p className="text-xs text-amber-400/90 bg-amber-950/30 border border-amber-900/30 rounded-md px-3 py-2 mt-2 leading-relaxed">
              {event.reason}
            </p>
          </>
        )}
      </CardContent>
    </Card>
  )
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <p className="text-xs text-muted-foreground uppercase tracking-wider mb-0.5">{label}</p>
      <p className="font-mono text-sm font-medium tabular-nums text-muted-foreground">{value}</p>
    </div>
  )
}

function FeedLoadingState() {
  return (
    <>
      {[...Array(3)].map((_, i) => (
        <Card key={i} className="gap-3 py-4 mb-3">
          <CardHeader className="px-4 pb-0">
            <div className="flex items-center gap-2">
              <Skeleton className="h-4 w-16" />
              <Skeleton className="h-4 w-10" />
            </div>
          </CardHeader>
          <CardContent className="px-4">
            <div className="grid grid-cols-4 gap-2">
              {[...Array(4)].map((_, j) => <Skeleton key={j} className="h-8" />)}
            </div>
          </CardContent>
        </Card>
      ))}
    </>
  )
}

function FeedEmptyState({ authorized }: { authorized: boolean }) {
  return (
    <Card className="py-16">
      <CardContent className="flex flex-col items-center text-center gap-3">
        <p className="font-medium text-muted-foreground">
          {authorized ? 'Waiting for futures prints…' : 'Schwab not connected'}
        </p>
        <p className="text-sm text-muted-foreground/60">
          {authorized
            ? 'Add root symbols to the watchlist (e.g. /CL, /ES) to stream live futures data.'
            : 'Connect your Schwab account to start streaming live futures data.'}
        </p>
        {!authorized && (
          <a href={`${API_BASE}/auth/schwab`}>
            <Button variant="outline" size="sm" className="gap-1.5 mt-1">
              <Zap className="h-3.5 w-3.5" />
              Connect Schwab
            </Button>
          </a>
        )}
      </CardContent>
    </Card>
  )
}

function WatchlistLoadingState() {
  return (
    <div className="space-y-2">
      {[...Array(3)].map((_, i) => (
        <Card key={i} className="py-3">
          <CardContent className="px-4 flex items-center gap-3">
            <Skeleton className="h-4 w-16" />
            <Skeleton className="h-3 w-24" />
          </CardContent>
        </Card>
      ))}
    </div>
  )
}

function WatchlistEmptyState() {
  return (
    <Card className="py-12">
      <CardContent className="flex flex-col items-center text-center gap-2">
        <p className="font-medium text-muted-foreground">No futures roots watched</p>
        <p className="text-sm text-muted-foreground/60">
          Add root symbols above (e.g. /CL, /ES, /NQ) to start streaming futures trades.
        </p>
      </CardContent>
    </Card>
  )
}

function ErrorState({ message }: { message: string }) {
  return (
    <Card className="py-12">
      <CardContent className="flex flex-col items-center text-center gap-2">
        <p className="font-medium text-destructive">Failed to load watchlist</p>
        <p className="text-sm text-muted-foreground font-mono">{message}</p>
      </CardContent>
    </Card>
  )
}

function formatDate(raw: string): string {
  if (!raw) return ''
  try {
    return new Date(raw).toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' })
  } catch { return raw }
}

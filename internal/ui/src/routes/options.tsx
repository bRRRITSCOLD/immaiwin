import { createFileRoute, Link } from '@tanstack/react-router'
import { useCallback, useEffect, useRef, useState } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import { AlertTriangle, LogOut, Plus, Trash2, Zap } from 'lucide-react'
import { Badge } from '~/components/ui/badge'
import { Button } from '~/components/ui/button'
import { Card, CardContent, CardHeader } from '~/components/ui/card'
import { Input } from '~/components/ui/input'
import { ScrollArea } from '~/components/ui/scroll-area'
import { Separator } from '~/components/ui/separator'
import { Skeleton } from '~/components/ui/skeleton'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '~/components/ui/tabs'

const API_BASE = import.meta.env['VITE_API_URL'] ?? 'http://localhost:8080'

interface OptionsWatchlistItem {
  symbol: string
  added_at: string
}

interface OptionsEvent {
  symbol: string
  underlying: string
  strike: number
  expiration: string
  type: 'call' | 'put'
  price: number
  size: number
  unusual: boolean
  reason?: string
  volume_oi_ratio?: number
  detected_at: string
}

export const Route = createFileRoute('/options')({
  component: OptionsPage,
})

function OptionsPage() {
  const [authorized, setAuthorized] = useState<boolean | null>(null)
  const [disconnecting, setDisconnecting] = useState(false)
  const [watchlist, setWatchlist] = useState<OptionsWatchlistItem[]>([])
  const [wlLoading, setWlLoading] = useState(true)
  const [wlError, setWlError] = useState<string | null>(null)
  const [addInput, setAddInput] = useState('')
  const [saving, setSaving] = useState(false)

  const [events, setEvents] = useState<OptionsEvent[]>([])
  const [connected, setConnected] = useState(false)
  const [streamLoading, setStreamLoading] = useState(true)
  const viewportRef = useRef<HTMLDivElement>(null)
  const esRef = useRef<EventSource | null>(null)

  const virtualizer = useVirtualizer({
    count: events.length,
    getScrollElement: () => viewportRef.current,
    estimateSize: () => 130,
    overscan: 5,
    getItemKey: (i) => `${events[i]!.symbol}-${events[i]!.detected_at}`,
  })

  // Poll Schwab auth status
  useEffect(() => {
    const check = () => {
      fetch(`${API_BASE}/api/v1/auth/schwab/status`)
        .then((r) => r.json() as Promise<{ authorized: boolean }>)
        .then((d) => setAuthorized(d.authorized))
        .catch(() => setAuthorized(false))
    }
    check()
    const id = setInterval(check, 30_000)
    return () => clearInterval(id)
  }, [])

  const disconnect = useCallback(() => {
    setDisconnecting(true)
    fetch(`${API_BASE}/api/v1/auth/schwab`, { method: 'DELETE' })
      .then(() => setAuthorized(false))
      .catch(() => {})
      .finally(() => setDisconnecting(false))
  }, [])

  // Fetch options watchlist
  const fetchWatchlist = useCallback(() => {
    setWlLoading(true)
    setWlError(null)
    fetch(`${API_BASE}/api/v1/options/watchlist`)
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`)
        return r.json() as Promise<OptionsWatchlistItem[]>
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

  // SSE unusual options stream
  useEffect(() => {
    const es = new EventSource(`${API_BASE}/api/v1/options/stream`)
    esRef.current = es
    es.addEventListener('option', (e: MessageEvent) => {
      const ev = JSON.parse(e.data as string) as OptionsEvent
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
    return fetch(`${API_BASE}/api/v1/options/watchlist`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ symbols }),
    })
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`)
      })
      .finally(() => setSaving(false))
  }, [])

  const addSymbol = useCallback(() => {
    const sym = addInput.trim().toUpperCase()
    if (!sym || watchlist.some((w) => w.symbol === sym)) {
      setAddInput('')
      return
    }
    const next = [...watchlist.map((w) => w.symbol), sym]
    syncWatchlist(next).then(() => {
      setWatchlist((prev) => [
        ...prev,
        { symbol: sym, added_at: new Date().toISOString() },
      ])
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
    <div className="min-h-screen bg-background text-foreground">
      <header className="sticky top-0 z-10 border-b bg-background/90 backdrop-blur-sm px-6 py-3 flex items-center justify-between">
        <div className="flex items-center gap-4">
          <Link to="/" className="text-lg font-semibold tracking-tight">immaiwin</Link>
          <Separator orientation="vertical" className="h-5" />
          <nav className="flex items-center gap-3 text-sm">
            <Link to="/" className="text-muted-foreground hover:text-foreground transition-colors">Trades</Link>
            <Link to="/markets" className="text-muted-foreground hover:text-foreground transition-colors">Markets</Link>
            <Link to="/watchlist" className="text-muted-foreground hover:text-foreground transition-colors">Watchlist</Link>
            <Link to="/news" className="text-muted-foreground hover:text-foreground transition-colors">News</Link>
            <Link to="/options" className="text-foreground font-medium">Options</Link>
            <Link to="/futures" className="text-muted-foreground hover:text-foreground transition-colors">Futures</Link>
          </nav>
        </div>
        <div className="flex items-center gap-2">
          {authorized === null ? (
            <Skeleton className="h-6 w-28" />
          ) : authorized ? (
            <Button
              size="sm"
              variant="outline"
              className="gap-1.5 border-green-700 text-green-400 hover:bg-red-950 hover:text-red-400 hover:border-red-700"
              onClick={disconnect}
              disabled={disconnecting}
            >
              <LogOut className="h-3.5 w-3.5" />
              {disconnecting ? 'Disconnecting…' : 'Schwab Connected'}
            </Button>
          ) : (
            <a href={`${API_BASE}/auth/schwab`}>
              <Button size="sm" variant="outline" className="gap-1.5">
                <Zap className="h-3.5 w-3.5" />
                Connect Schwab
              </Button>
            </a>
          )}
          <Badge variant={connected ? 'default' : 'destructive'} className="gap-1.5">
            <span className={`h-1.5 w-1.5 rounded-full ${connected ? 'bg-green-400' : 'bg-red-400'}`} />
            {connected ? 'Live' : 'Disconnected'}
          </Badge>
        </div>
      </header>

      <main className="max-w-3xl mx-auto px-4 py-6">
        <Tabs defaultValue="feed">
          <TabsList className="mb-4">
            <TabsTrigger value="feed">Live Feed</TabsTrigger>
            <TabsTrigger value="watchlist">Watchlist</TabsTrigger>
          </TabsList>

          {/* ── Live feed ── */}
          <TabsContent value="feed">
            <ScrollArea className="h-[calc(100vh-9rem)]" viewportRef={viewportRef}>
              {streamLoading ? (
                <div className="pr-4"><FeedLoadingState /></div>
              ) : events.length === 0 ? (
                <FeedEmptyState authorized={authorized ?? false} />
              ) : (
                <div
                  style={{ height: `${virtualizer.getTotalSize()}px`, position: 'relative' }}
                  className="pr-4"
                >
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
                      <OptionsEventCard event={events[vr.index]!} />
                    </div>
                  ))}
                </div>
              )}
            </ScrollArea>
          </TabsContent>

          {/* ── Watchlist manager ── */}
          <TabsContent value="watchlist">
            <div className="space-y-4">
              {/* Add symbol */}
              <div className="flex items-center gap-2">
                <Input
                  className="max-w-xs font-mono uppercase"
                  placeholder="SPY, AAPL, QQQ…"
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
          </TabsContent>
        </Tabs>
      </main>
    </div>
  )
}

function OptionsEventCard({ event }: { event: OptionsEvent }) {
  const isCall = event.type === 'call'
  const exp = new Date(event.expiration).toLocaleDateString(undefined, {
    month: 'short', day: 'numeric', year: '2-digit',
  })
  const notional = (event.size * 100 * event.price).toLocaleString(undefined, { maximumFractionDigits: 0 })

  return (
    <Card className={`gap-3 py-4 ${event.unusual ? 'border-amber-700/50' : ''}`}>
      <CardHeader className="px-4 pb-0">
        <div className="flex items-start justify-between gap-3">
          <div className="flex items-center gap-2 flex-wrap min-w-0">
            <Badge className={`shrink-0 font-bold text-xs ${isCall ? 'bg-green-700 text-green-100' : 'bg-red-800 text-red-100'}`}>
              {event.type.toUpperCase()}
            </Badge>
            <span className="font-mono font-semibold text-sm">{event.underlying}</span>
            <span className="text-sm text-muted-foreground">
              ${event.strike} · {exp}
            </span>
            {event.unusual && (
              <Badge variant="outline" className="shrink-0 text-xs bg-amber-800 text-amber-100 border-amber-700">
                <AlertTriangle className="h-3 w-3 mr-1" />UNUSUAL
              </Badge>
            )}
          </div>
          <time className="text-xs text-muted-foreground whitespace-nowrap shrink-0">
            {new Date(event.detected_at).toLocaleTimeString()}
          </time>
        </div>
      </CardHeader>
      <CardContent className="px-4 space-y-2">
        <div className="grid grid-cols-4 gap-2">
          <Stat label="Size" value={`${event.size.toLocaleString()} ct`} highlight={event.unusual} />
          <Stat label="Price" value={`$${event.price.toFixed(2)}`} />
          <Stat label="Notional" value={`$${notional}`} highlight={event.unusual} />
          <Stat label="Vol/OI" value={event.volume_oi_ratio ? `${event.volume_oi_ratio.toFixed(1)}x` : '—'} />
        </div>
        {event.unusual && event.reason && (
          <>
            <Separator />
            <p className="text-xs text-amber-400/90 bg-amber-950/30 border border-amber-900/30 rounded-md px-3 py-2 leading-relaxed">
              {event.reason}
            </p>
          </>
        )}
      </CardContent>
    </Card>
  )
}

function Stat({ label, value, highlight }: { label: string; value: string; highlight?: boolean }) {
  return (
    <div>
      <p className="text-xs text-muted-foreground uppercase tracking-wider mb-0.5">{label}</p>
      <p className={`font-mono text-sm font-medium tabular-nums ${highlight ? 'text-foreground' : 'text-muted-foreground'}`}>
        {value}
      </p>
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
              <Skeleton className="h-5 w-14 rounded-md" />
              <Skeleton className="h-4 w-12" />
              <Skeleton className="h-4 w-32" />
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
          {authorized ? 'Waiting for options prints…' : 'Schwab not connected'}
        </p>
        <p className="text-sm text-muted-foreground/60">
          {authorized
            ? 'All options prints stream here in real time. Unusual blocks are highlighted.'
            : 'Connect your Schwab account to start streaming live options data.'}
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
        <p className="font-medium text-muted-foreground">No underlyings watched</p>
        <p className="text-sm text-muted-foreground/60">
          Add ticker symbols above (e.g. SPY, AAPL) to start tracking unusual options activity.
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

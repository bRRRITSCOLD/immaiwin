import { createFileRoute, Link } from '@tanstack/react-router'
import { useCallback, useEffect, useRef, useState } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import { ChevronDown, ChevronUp } from 'lucide-react'
import { Badge } from '~/components/ui/badge'
import { Card, CardContent, CardHeader } from '~/components/ui/card'
import { ScrollArea } from '~/components/ui/scroll-area'
import { Separator } from '~/components/ui/separator'
import { Skeleton } from '~/components/ui/skeleton'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '~/components/ui/tabs'
import { Tooltip, TooltipContent, TooltipTrigger } from '~/components/ui/tooltip'
import { AlertTriangle } from 'lucide-react'

const API_BASE = import.meta.env['VITE_API_URL'] ?? 'http://localhost:8080'

interface Trade {
  asset_id: string
  market: string
  market_question: string
  price: string
  size: string
  side: string
  fee_rate_bps: string
  timestamp: string
  unusual: boolean
  rolling_avg_size: number
  reason: string
  detected_at: string
  token_outcome: string
}

export const Route = createFileRoute('/')({
  component: TradesPage,
})

function TradesPage() {
  const [liveTrades, setLiveTrades] = useState<Trade[]>([])
  const [connected, setConnected] = useState(false)
  const [liveLoading, setLiveLoading] = useState(true)
  const esRef = useRef<EventSource | null>(null)
  const liveViewportRef = useRef<HTMLDivElement>(null)

  const [histTrades, setHistTrades] = useState<Trade[]>([])
  const [histLoading, setHistLoading] = useState(true)
  const [histError, setHistError] = useState<string | null>(null)

  const liveVirtualizer = useVirtualizer({
    count: liveTrades.length,
    getScrollElement: () => liveViewportRef.current,
    estimateSize: () => 160,
    overscan: 5,
    getItemKey: (index) => `${liveTrades[index]!.asset_id}-${liveTrades[index]!.detected_at}`,
  })

  // SSE for live feed
  useEffect(() => {
    const es = new EventSource(`${API_BASE}/api/v1/trades/stream`)
    esRef.current = es

    es.addEventListener('trade', (e: MessageEvent) => {
      const trade = JSON.parse(e.data as string) as Trade
      setLiveLoading(false)
      setLiveTrades((prev) => [trade, ...prev].slice(0, 200))
    })

    es.onopen = () => {
      setConnected(true)
      setTimeout(() => setLiveLoading(false), 3000)
    }
    es.onerror = () => setConnected(false)

    return () => { es.close() }
  }, [])

  // Historical trades from MongoDB (unusual only)
  const fetchHistTrades = useCallback(() => {
    setHistLoading(true)
    setHistError(null)
    fetch(`${API_BASE}/api/v1/trades?limit=200`)
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`)
        return r.json() as Promise<Trade[]>
      })
      .then((data) => {
        setHistTrades(data ?? [])
        setHistLoading(false)
      })
      .catch((err: unknown) => {
        setHistError(err instanceof Error ? err.message : 'Unknown error')
        setHistLoading(false)
      })
  }, [])

  useEffect(() => { fetchHistTrades() }, [fetchHistTrades])

  return (
    <div className="min-h-screen bg-background text-foreground">
      <header className="sticky top-0 z-10 border-b bg-background/90 backdrop-blur-sm px-6 py-3 flex items-center justify-between">
        <div className="flex items-center gap-4">
          <h1 className="text-lg font-semibold tracking-tight">immaiwin</h1>
          <Separator orientation="vertical" className="h-5" />
          <nav className="flex items-center gap-3 text-sm">
            <Link to="/" className="text-foreground font-medium">Trades</Link>
            <Link to="/markets" className="text-muted-foreground hover:text-foreground transition-colors">Markets</Link>
            <Link to="/watchlist" className="text-muted-foreground hover:text-foreground transition-colors">Watchlist</Link>
          </nav>
        </div>
        <Badge variant={connected ? 'default' : 'destructive'} className="gap-1.5">
          <span className={`h-1.5 w-1.5 rounded-full ${connected ? 'bg-green-400' : 'bg-red-400'}`} />
          {connected ? 'Live' : 'Disconnected'}
        </Badge>
      </header>

      <main className="max-w-3xl mx-auto px-4 py-6">
        <Tabs defaultValue="live" onValueChange={(v) => { if (v === 'history') fetchHistTrades() }}>
          <TabsList className="mb-4">
            <TabsTrigger value="live">Live Feed</TabsTrigger>
            <TabsTrigger value="history">History by Market</TabsTrigger>
          </TabsList>

          <TabsContent value="live">
            <ScrollArea className="h-[calc(100vh-9rem)]" viewportRef={liveViewportRef}>
              {liveLoading ? (
                <div className="pr-4"><LoadingState /></div>
              ) : liveTrades.length === 0 ? (
                <LiveEmptyState />
              ) : (
                <div style={{ height: `${liveVirtualizer.getTotalSize()}px`, position: 'relative' }} className="pr-4">
                  {liveVirtualizer.getVirtualItems().map((virtualRow) => (
                    <div
                      key={virtualRow.key}
                      data-index={virtualRow.index}
                      ref={liveVirtualizer.measureElement}
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
                      <TradeCard trade={liveTrades[virtualRow.index]!} />
                    </div>
                  ))}
                </div>
              )}
            </ScrollArea>
          </TabsContent>

          <TabsContent value="history">
            <ScrollArea className="h-[calc(100vh-9rem)]">
              <div className="space-y-3 pr-4">
                {histLoading ? (
                  <LoadingState />
                ) : histError ? (
                  <ErrorState message={histError} />
                ) : histTrades.length === 0 ? (
                  <HistEmptyState />
                ) : (
                  <MarketGroups trades={histTrades} />
                )}
              </div>
            </ScrollArea>
          </TabsContent>
        </Tabs>
      </main>
    </div>
  )
}

function marketLabel(t: Trade): string {
  if (t.market_question) return t.market_question
  const id = t.market || t.asset_id
  return id.startsWith('0x') ? `Unknown market (${id.slice(0, 10)}…)` : id
}

// Group trades by market_question, render one collapsible card per market
function MarketGroups({ trades }: { trades: Trade[] }) {
  // Preserve insertion order (newest first from server) — group maintains first-seen order
  const groupMap = new Map<string, Trade[]>()
  for (const t of trades) {
    const key = t.market_question || t.market || t.asset_id
    if (!groupMap.has(key)) groupMap.set(key, [])
    groupMap.get(key)!.push(t)
  }
  const groups = Array.from(groupMap.entries())

  return (
    <div className="space-y-3">
      {groups.map(([question, groupTrades]) => (
        <MarketGroup key={question} question={marketLabel(groupTrades[0]!)} trades={groupTrades} />
      ))}
    </div>
  )
}

function MarketGroup({ question, trades }: { question: string; trades: Trade[] }) {
  const [open, setOpen] = useState(false)
  const latest = trades[0]
  const viewportRef = useRef<HTMLDivElement>(null)

  const virtualizer = useVirtualizer({
    count: trades.length,
    getScrollElement: () => viewportRef.current,
    estimateSize: () => 110,
    overscan: 3,
  })

  return (
    <Card className="gap-0 py-0">
      <button
        className="w-full px-4 py-3 flex items-center justify-between gap-3 hover:bg-muted/40 transition-colors rounded-t-lg"
        onClick={() => setOpen((v) => !v)}
      >
        <div className="flex items-center gap-2 min-w-0">
          <Badge variant="secondary" className="shrink-0 text-xs tabular-nums">
            {trades.length}
          </Badge>
          <span className="font-medium text-sm leading-snug text-left truncate">{question}</span>
        </div>
        <div className="flex items-center gap-2 shrink-0">
          {latest && (
            <span className="text-xs text-muted-foreground">
              {new Date(latest.detected_at).toLocaleString()}
            </span>
          )}
          {open ? <ChevronUp className="h-4 w-4 text-muted-foreground" /> : <ChevronDown className="h-4 w-4 text-muted-foreground" />}
        </div>
      </button>

      {open && (
        <>
          <Separator />
          <ScrollArea className="h-72" viewportRef={viewportRef}>
            <div
              style={{ height: `${virtualizer.getTotalSize()}px`, position: 'relative' }}
              className="p-3"
            >
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
                    paddingBottom: '8px',
                  }}
                >
                  <HistTradeRow trade={trades[virtualRow.index]!} />
                </div>
              ))}
            </div>
          </ScrollArea>
        </>
      )}
    </Card>
  )
}

function SideBadge({ side }: { side: string }) {
  const isBuy = side.toUpperCase() === 'BUY'
  return (
    // <Badge variant={isBuy ? 'default' : 'destructive'} className="shrink-0 font-bold text-xs">
    <Badge className={`shrink-0 font-bold text-xs ${isBuy ? 'bg-green-400' : 'bg-red-400'}`}>
      {side || '—'}
    </Badge>
  )
}

function OutcomeBadge({ outcome }: { outcome: string }) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Badge variant="outline" className="shrink-0 text-xs cursor-default bg-zinc-500 text-white border-zinc-400">
          {outcome?.toUpperCase() || ""}
        </Badge>
      </TooltipTrigger>
      <TooltipContent>
        Traded on the {outcome} outcome token — a position that profits if the market resolves {outcome}.
      </TooltipContent>
    </Tooltip>
  )
}

function UnusualBadge() {
  return (
    <Badge variant="outline" className="shrink-0 text-xs bg-amber-800 text-amber-100 border-amber-700">
      <AlertTriangle className="h-4 w-4" /> UNUSUAL
    </Badge>
  )
}

function HistTradeRow({ trade }: { trade: Trade }) {
  const size = formatNumber(trade.size, 0)
  const price = formatNumber(trade.price, 4)
  const detectedAt = new Date(trade.detected_at)

  return (
    <div className="rounded-md border bg-muted/30 px-3 py-2 space-y-1.5">
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2 min-w-0">
          <SideBadge side={trade.side} />
          {trade.token_outcome && <OutcomeBadge outcome={trade.token_outcome} />}
          <UnusualBadge />
        </div>
        <time className="text-xs text-muted-foreground whitespace-nowrap shrink-0" dateTime={trade.detected_at}>
          {detectedAt.toLocaleString()}
        </time>
      </div>
      <div className="flex items-center gap-4 flex-wrap">
        <MiniStat label="Size" value={`${size} USDC`} />
        <MiniStat label="Price" value={price} />
        {trade.rolling_avg_size > 0 && <MiniStat label="Avg" value={formatNumber(String(trade.rolling_avg_size), 0)} />}
      </div>
      {trade.reason && (
        <p className="text-xs text-amber-400/90 bg-amber-950/30 border border-amber-900/30 rounded px-2 py-1 leading-relaxed">
          {trade.reason}
        </p>
      )}
    </div>
  )
}

function TradeCard({ trade }: { trade: Trade }) {
  const detectedAt = new Date(trade.detected_at)
  const size = formatNumber(trade.size, 0)
  const price = formatNumber(trade.price, 4)

  return (
    <Card className="gap-3 py-4">
      <CardHeader className="px-4 pb-0">
        <div className="flex items-start justify-between gap-3">
          <div className="flex items-center gap-2 min-w-0">
            <SideBadge side={trade.side} />
            {trade.token_outcome && <OutcomeBadge outcome={trade.token_outcome} />}
            {trade.unusual && <UnusualBadge />}
            <div className="min-w-0">
              {trade.market_question ? (
                <p className="text-sm font-medium leading-snug">{trade.market_question}</p>
              ) : (
                <p className="font-mono text-sm truncate">{truncate(trade.asset_id, 26)}</p>
              )}
              <p className="font-mono text-xs text-muted-foreground truncate mt-0.5">
                {truncate(trade.asset_id, 20)}
              </p>
            </div>
          </div>
          <time
            className="text-xs text-muted-foreground whitespace-nowrap shrink-0"
            dateTime={trade.detected_at}
            title={detectedAt.toISOString()}
          >
            {detectedAt.toLocaleTimeString()}
          </time>
        </div>
      </CardHeader>

      <CardContent className="px-4 space-y-3">
        <div className="grid grid-cols-3 gap-2">
          <Stat label="Size (USDC)" value={size} highlight={trade.unusual} />
          <Stat label="Price" value={price} />
          <Stat label="Rolling Avg" value={trade.unusual ? formatNumber(String(trade.rolling_avg_size), 0) : '—'} />
        </div>

        {trade.unusual && trade.reason && (
          <>
            <Separator />
            <p className="text-xs text-amber-400/90 bg-amber-950/30 border border-amber-900/30 rounded-md px-3 py-2 leading-relaxed">
              {trade.reason}
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

function MiniStat({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center gap-1">
      <span className="text-xs text-muted-foreground">{label}:</span>
      <span className="font-mono text-xs font-medium tabular-nums">{value}</span>
    </div>
  )
}

function LoadingState() {
  return (
    <>
      {[...Array(4)].map((_, i) => (
        <Card key={i} className="gap-3 py-4">
          <CardHeader className="px-4 pb-0">
            <div className="flex items-center justify-between gap-3">
              <div className="flex items-center gap-2">
                <Skeleton className="h-5 w-10 rounded-md" />
                <Skeleton className="h-4 w-40" />
              </div>
              <Skeleton className="h-3 w-16" />
            </div>
          </CardHeader>
          <CardContent className="px-4 space-y-3">
            <div className="grid grid-cols-3 gap-2">
              <Skeleton className="h-8" />
              <Skeleton className="h-8" />
              <Skeleton className="h-8" />
            </div>
          </CardContent>
        </Card>
      ))}
    </>
  )
}

function LiveEmptyState() {
  return (
    <Card className="py-16">
      <CardContent className="flex flex-col items-center text-center gap-2">
        <p className="font-medium text-muted-foreground">Waiting for trades…</p>
        <p className="text-sm text-muted-foreground/60">
          All trades stream here in real time. Unusual trades are highlighted.
        </p>
      </CardContent>
    </Card>
  )
}

function HistEmptyState() {
  return (
    <Card className="py-16">
      <CardContent className="flex flex-col items-center text-center gap-2">
        <p className="font-medium text-muted-foreground">No historical trades yet</p>
        <p className="text-sm text-muted-foreground/60">
          Unusual trades are saved as they're detected. Check back after the watcher runs.
        </p>
      </CardContent>
    </Card>
  )
}

function ErrorState({ message }: { message: string }) {
  return (
    <Card className="py-12">
      <CardContent className="flex flex-col items-center text-center gap-2">
        <p className="font-medium text-destructive">Failed to load trades</p>
        <p className="text-sm text-muted-foreground font-mono">{message}</p>
      </CardContent>
    </Card>
  )
}

function formatNumber(raw: string, decimals: number): string {
  const n = parseFloat(raw)
  if (isNaN(n)) return raw
  return n.toLocaleString(undefined, { maximumFractionDigits: decimals })
}

function truncate(s: string, n: number): string {
  return s.length > n ? `${s.slice(0, n)}…` : s
}

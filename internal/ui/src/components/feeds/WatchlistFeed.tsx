import { useCallback, useEffect, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { ChevronDown, ChevronUp, Star, Trash2 } from 'lucide-react'
import { Button } from '~/components/ui/button'
import { Card, CardContent, CardHeader } from '~/components/ui/card'
import { Checkbox } from '~/components/ui/checkbox'
import { Input } from '~/components/ui/input'
import { ScrollArea } from '~/components/ui/scroll-area'
import { Separator } from '~/components/ui/separator'
import { Skeleton } from '~/components/ui/skeleton'
import { Textarea } from '~/components/ui/textarea'

const API_BASE = import.meta.env['VITE_API_URL'] ?? 'http://localhost:8080'

const DEFAULT_WINDOW_SIZE = 20
const DEFAULT_EXPR = `Size >= 10000 || (WindowFull && Avg > 0 && Size >= Avg * 3.0)`

interface WatchlistItem {
  id: string
  market_id: string
  question: string
  slug: string
  unusual_expr?: string
  window_size?: number
  added_at: string
}

export function WatchlistFeed() {
  const [items, setItems] = useState<WatchlistItem[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [selected, setSelected] = useState<Set<string>>(new Set())

  useEffect(() => {
    fetch(`${API_BASE}/api/v1/watchlist`)
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`)
        return r.json() as Promise<WatchlistItem[]>
      })
      .then((data) => {
        setItems(data ?? [])
        setLoading(false)
      })
      .catch((err: unknown) => {
        setError(err instanceof Error ? err.message : 'Unknown error')
        setLoading(false)
      })
  }, [])

  const toggleSelect = useCallback((marketId: string) => {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(marketId)) next.delete(marketId)
      else next.add(marketId)
      return next
    })
  }, [])

  const allSelected = items.length > 0 && selected.size === items.length
  const someSelected = selected.size > 0 && !allSelected

  const toggleAll = useCallback(() => {
    if (allSelected || someSelected) {
      setSelected(new Set())
    } else {
      setSelected(new Set(items.map((i) => i.market_id)))
    }
  }, [allSelected, someSelected, items])

  const removeSelected = useCallback(() => {
    const next = items.filter((i) => !selected.has(i.market_id))
    setSaving(true)
    fetch(`${API_BASE}/api/v1/watchlist`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(
        next.map((i) => ({ market_id: i.market_id, question: i.question, slug: i.slug })),
      ),
    })
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`)
        setItems(next)
        setSelected(new Set())
      })
      .catch(() => {})
      .finally(() => setSaving(false))
  }, [items, selected])

  const handleExprSaved = useCallback((marketId: string, expr: string) => {
    setItems((prev) =>
      prev.map((item) =>
        item.market_id === marketId ? { ...item, unusual_expr: expr } : item,
      ),
    )
  }, [])

  return (
    <div className="flex flex-col flex-1 min-h-0">
      {/* Toolbar */}
      <div className="flex items-center justify-between gap-3 px-1 pb-3 shrink-0">
        <div className="flex items-center gap-3">
          {!loading && items.length > 0 && (
            <Checkbox
              checked={allSelected ? true : someSelected ? 'indeterminate' : false}
              onCheckedChange={toggleAll}
              aria-label="Select all"
            />
          )}
          <span className="text-sm text-muted-foreground">
            {loading ? '' : selected.size > 0
              ? `${selected.size} of ${items.length} selected`
              : `${items.length} market${items.length !== 1 ? 's' : ''}`}
          </span>
        </div>
        <div className="flex items-center gap-2">
          {selected.size > 0 && (
            <Button
              variant="destructive"
              size="sm"
              onClick={removeSelected}
              disabled={saving}
              className="gap-1.5"
            >
              <Trash2 className="h-3.5 w-3.5" />
              Remove {selected.size}
            </Button>
          )}
          <Link to="/markets">
            <Button variant="outline" size="sm">+ Add markets</Button>
          </Link>
        </div>
      </div>

      {/* List */}
      <div className="flex-1 overflow-hidden min-h-0">
        <ScrollArea className="h-full">
          <div className="space-y-3 pr-4">
            {loading ? (
              <LoadingState />
            ) : error ? (
              <ErrorState message={error} />
            ) : items.length === 0 ? (
              <EmptyState />
            ) : (
              items.map((item) => (
                <WatchlistCard
                  key={item.market_id}
                  item={item}
                  selected={selected.has(item.market_id)}
                  onToggle={() => toggleSelect(item.market_id)}
                  onExprSaved={handleExprSaved}
                  disabled={saving}
                />
              ))
            )}
          </div>
        </ScrollArea>
      </div>
    </div>
  )
}

function WatchlistCard({
  item,
  selected,
  onToggle,
  onExprSaved,
  disabled,
}: {
  item: WatchlistItem
  selected: boolean
  onToggle: () => void
  onExprSaved: (marketId: string, expr: string) => void
  disabled: boolean
}) {
  const [exprOpen, setExprOpen] = useState(false)
  const [exprValue, setExprValue] = useState(item.unusual_expr || DEFAULT_EXPR)
  const [windowValue, setWindowValue] = useState(String(item.window_size || DEFAULT_WINDOW_SIZE))
  const [exprSaving, setExprSaving] = useState(false)
  const [exprError, setExprError] = useState<string | null>(null)
  const [exprSaved, setExprSaved] = useState(false)
  const savedTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  const hasCustomExpr = !!item.unusual_expr && item.unusual_expr !== DEFAULT_EXPR
  const hasCustomWindow = !!item.window_size && item.window_size !== DEFAULT_WINDOW_SIZE

  const savedExpr = item.unusual_expr || DEFAULT_EXPR
  const savedWindow = String(item.window_size || DEFAULT_WINDOW_SIZE)
  const isDirty = exprValue !== savedExpr || windowValue !== savedWindow

  const resetToSaved = useCallback(() => {
    setExprValue(savedExpr)
    setWindowValue(savedWindow)
    setExprError(null)
    setExprSaved(false)
  }, [savedExpr, savedWindow])

  const saveExpr = useCallback(() => {
    const windowSize = windowValue.trim() === '' ? 0 : parseInt(windowValue, 10)
    if (windowValue.trim() !== '' && (isNaN(windowSize) || windowSize < 1)) {
      setExprError('Window size must be a positive integer (or empty for default)')
      return
    }
    setExprSaving(true)
    setExprError(null)
    fetch(`${API_BASE}/api/v1/watchlist/${encodeURIComponent(item.market_id)}/config`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ expr: exprValue, window_size: windowSize }),
    })
      .then(async (r) => {
        if (!r.ok) {
          const body = await r.json().catch(() => ({ error: `HTTP ${r.status}` })) as { error?: string }
          throw new Error(body.error ?? `HTTP ${r.status}`)
        }
        onExprSaved(item.market_id, exprValue)
        setExprSaved(true)
        if (savedTimerRef.current) clearTimeout(savedTimerRef.current)
        savedTimerRef.current = setTimeout(() => setExprSaved(false), 2000)
      })
      .catch((err: unknown) => {
        setExprError(err instanceof Error ? err.message : 'Save failed')
      })
      .finally(() => setExprSaving(false))
  }, [item.market_id, exprValue, windowValue, onExprSaved])

  return (
    <Card className={`py-4 gap-2 transition-colors ${selected ? 'border-primary/50 bg-primary/5' : ''}`}>
      <CardHeader className="px-4 pb-0">
        <div className="flex items-start justify-between gap-3">
          <div className="flex items-start gap-3 min-w-0 cursor-pointer" onClick={onToggle}>
            <Checkbox
              checked={selected}
              disabled={disabled}
              onCheckedChange={onToggle}
              onClick={(e) => e.stopPropagation()}
              className="mt-0.5 shrink-0"
              aria-label="Select market"
            />
            <div className="flex items-start gap-2 min-w-0">
              <Star className="h-4 w-4 mt-0.5 fill-yellow-400 text-yellow-400 shrink-0" />
              <p className="font-medium leading-snug">{item.question}</p>
            </div>
          </div>
          <button
            onClick={() => setExprOpen((v) => !v)}
            className="flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground transition-colors shrink-0 mt-0.5"
            aria-label="Toggle expression editor"
          >
            {(hasCustomExpr || hasCustomWindow) && !exprOpen && (
              <span className="h-1.5 w-1.5 rounded-full bg-zinc-500 inline-block mr-0.5" />
            )}
            <span>Expr</span>
            {exprOpen ? <ChevronUp className="h-3 w-3" /> : <ChevronDown className="h-3 w-3" />}
          </button>
        </div>
      </CardHeader>
      <CardContent className="px-4 pt-0 pl-14 space-y-2">
        <p className="text-xs text-muted-foreground">Added {formatDate(item.added_at)}</p>
        {exprOpen && (
          <div className="space-y-2 pt-1" onClick={(e) => e.stopPropagation()}>
            <Separator />
            <p className="text-xs text-muted-foreground mb-1">
              Custom unusual-trade expression. Leave empty to use default detector.
            </p>
            <div className="grid grid-cols-2 gap-x-4 gap-y-0.5 text-xs mb-1">
              {[
                ['Size', 'USD notional size of trade (e.g. 5000.00)'],
                ['Price', 'Trade price 0–1 (e.g. 0.72 = 72¢)'],
                ['Side', '"BUY" or "SELL"'],
                ['Avg', `Rolling avg size of last ${item.window_size || DEFAULT_WINDOW_SIZE} trades (partial until WindowFull)`],
                ['WindowFull', `True once ${item.window_size || DEFAULT_WINDOW_SIZE} trades seen — gate Avg-based checks`],
                ['AssetID', 'CLOB token ID — unique per Yes/No token'],
                ['Market', 'Market condition ID — shared by Yes+No tokens'],
              ].map(([name, desc]) => (
                <div key={name} className="flex items-baseline gap-1.5">
                  <code className="text-zinc-400 shrink-0">{name}</code>
                  <span className="text-muted-foreground/70 truncate">{desc}</span>
                </div>
              ))}
            </div>
            <div className="flex items-center gap-2">
              <label className="text-xs text-muted-foreground whitespace-nowrap">Avg window (N)</label>
              <Input
                type="number"
                min={1}
                className="h-7 w-24 text-xs font-mono"
                placeholder="20"
                value={windowValue}
                onChange={(e) => {
                  setWindowValue(e.target.value)
                  setExprError(null)
                  setExprSaved(false)
                }}
              />
              <span className="text-xs text-muted-foreground/60">trades (empty = global default)</span>
            </div>
            <Textarea
              className="font-mono text-xs min-h-[60px] resize-y"
              value={exprValue}
              onChange={(e) => {
                setExprValue(e.target.value)
                setExprError(null)
                setExprSaved(false)
              }}
            />
            {exprError && (
              <p className="text-xs text-destructive font-mono">{exprError}</p>
            )}
            <div className="flex items-center gap-2">
              <Button size="sm" variant="secondary" onClick={saveExpr} disabled={exprSaving}>
                {exprSaving ? 'Saving…' : exprSaved ? 'Saved' : 'Save'}
              </Button>
              {isDirty && (
                <Button size="sm" variant="ghost" onClick={resetToSaved} className="text-muted-foreground">
                  Reset
                </Button>
              )}
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function LoadingState() {
  return (
    <div className="space-y-3">
      {[...Array(4)].map((_, i) => (
        <Card key={i} className="py-4">
          <CardHeader className="px-4 pb-0">
            <div className="flex items-center gap-3">
              <Skeleton className="h-4 w-4 rounded shrink-0" />
              <Skeleton className="h-4 w-4 rounded-full shrink-0" />
              <Skeleton className="h-4 w-3/4" />
            </div>
          </CardHeader>
          <CardContent className="px-4 pt-2 pl-14">
            <Skeleton className="h-3 w-24" />
          </CardContent>
        </Card>
      ))}
    </div>
  )
}

function EmptyState() {
  return (
    <Card className="py-16">
      <CardContent className="flex flex-col items-center text-center gap-3">
        <Star className="h-8 w-8 text-muted-foreground/40" />
        <p className="font-medium text-muted-foreground">No markets watchlisted</p>
        <p className="text-sm text-muted-foreground/60">
          Go to Markets and star markets to add them here.
        </p>
        <Link to="/markets">
          <Button variant="outline" size="sm">Browse markets</Button>
        </Link>
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
    return new Date(raw).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
  } catch { return raw }
}

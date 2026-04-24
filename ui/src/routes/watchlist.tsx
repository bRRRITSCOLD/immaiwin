import { createFileRoute, Link } from '@tanstack/react-router'
import { useCallback, useEffect, useState } from 'react'
import { Star, Trash2 } from 'lucide-react'
import { Button } from '~/components/ui/button'
import { Card, CardContent, CardHeader } from '~/components/ui/card'
import { Checkbox } from '~/components/ui/checkbox'
import { Separator } from '~/components/ui/separator'
import { Skeleton } from '~/components/ui/skeleton'

const API_BASE = import.meta.env['VITE_API_URL'] ?? 'http://localhost:8080'

interface WatchlistItem {
  id: string
  market_id: string
  question: string
  slug: string
  added_at: string
}

export const Route = createFileRoute('/watchlist')({
  component: WatchlistPage,
})

function WatchlistPage() {
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
            <Link to="/markets" className="text-muted-foreground hover:text-foreground transition-colors">
              Markets
            </Link>
            <Link to="/watchlist" className="text-foreground font-medium">
              Watchlist
            </Link>
          </nav>
        </div>
      </header>

      <main className="max-w-4xl mx-auto px-4 py-6 space-y-4">
        <div className="flex items-center justify-between gap-3">
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

        {loading ? (
          <LoadingState />
        ) : error ? (
          <ErrorState message={error} />
        ) : items.length === 0 ? (
          <EmptyState />
        ) : (
          <div className="space-y-3">
            {items.map((item) => (
              <WatchlistCard
                key={item.market_id}
                item={item}
                selected={selected.has(item.market_id)}
                onToggle={() => toggleSelect(item.market_id)}
                disabled={saving}
              />
            ))}
          </div>
        )}
      </main>
    </div>
  )
}

function WatchlistCard({
  item,
  selected,
  onToggle,
  disabled,
}: {
  item: WatchlistItem
  selected: boolean
  onToggle: () => void
  disabled: boolean
}) {
  return (
    <Card
      className={`py-4 gap-2 cursor-pointer transition-colors ${selected ? 'border-primary/50 bg-primary/5' : ''}`}
      onClick={onToggle}
    >
      <CardHeader className="px-4 pb-0">
        <div className="flex items-start justify-between gap-3">
          <div className="flex items-start gap-3 min-w-0">
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
        </div>
      </CardHeader>
      <CardContent className="px-4 pt-0 pl-14">
        <p className="text-xs text-muted-foreground">
          Added {formatDate(item.added_at)}
        </p>
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
    return new Date(raw).toLocaleDateString(undefined, {
      year: 'numeric',
      month: 'short',
      day: 'numeric',
    })
  } catch {
    return raw
  }
}

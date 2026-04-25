import { createFileRoute, Link } from '@tanstack/react-router'
import { Separator } from '~/components/ui/separator'
import { PolymarketMarketsFeed } from '~/components/feeds/PolymarketMarketsFeed'

export const Route = createFileRoute('/markets')({
  component: MarketsPage,
})

function MarketsPage() {
  return (
    <div className="h-screen overflow-hidden bg-background text-foreground flex flex-col">
      <header className="sticky top-0 z-10 border-b bg-background/90 backdrop-blur-sm px-6 py-3 flex items-center gap-4 shrink-0">
        <Link to="/" className="text-lg font-semibold tracking-tight">immaiwin</Link>
        <Separator orientation="vertical" className="h-5" />
        <nav className="flex items-center gap-3 text-sm">
          <Link to="/" className="text-muted-foreground hover:text-foreground transition-colors">Polymarket</Link>
          <Link to="/markets" className="text-foreground font-medium">Markets</Link>
          <Link to="/news" className="text-muted-foreground hover:text-foreground transition-colors">News</Link>
          <Link to="/options" className="text-muted-foreground hover:text-foreground transition-colors">Options</Link>
          <Link to="/futures" className="text-muted-foreground hover:text-foreground transition-colors">Futures</Link>
          <Link to="/dashboard" className="text-muted-foreground hover:text-foreground transition-colors">Dashboard</Link>
          <Link to="/scrapers" className="text-muted-foreground hover:text-foreground transition-colors">Scrapers</Link>
        </nav>
      </header>
      <main className="max-w-4xl mx-auto w-full px-4 py-6 flex-1 min-h-0">
        <PolymarketMarketsFeed />
      </main>
    </div>
  )
}

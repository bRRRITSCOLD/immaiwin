import { createFileRoute, Link } from '@tanstack/react-router'
import { Separator } from '~/components/ui/separator'
import { NewsFeed } from '~/components/feeds/NewsFeed'

export const Route = createFileRoute('/news')({
  component: NewsPage,
})

function NewsPage() {
  return (
    <div className="h-screen overflow-hidden bg-background text-foreground flex flex-col">
      <header className="sticky top-0 z-10 border-b bg-background/90 backdrop-blur-sm px-6 py-3 flex items-center gap-4 shrink-0">
        <h1 className="text-lg font-semibold tracking-tight">immaiwin</h1>
        <Separator orientation="vertical" className="h-5" />
        <nav className="flex items-center gap-3 text-sm">
          <Link to="/" className="text-muted-foreground hover:text-foreground transition-colors">Polymarket</Link>
          <Link to="/news" className="text-foreground font-medium">News</Link>
          <Link to="/options" className="text-muted-foreground hover:text-foreground transition-colors">Options</Link>
          <Link to="/futures" className="text-muted-foreground hover:text-foreground transition-colors">Futures</Link>
          <Link to="/dashboard" className="text-muted-foreground hover:text-foreground transition-colors">Dashboard</Link>
          <Link to="/scrapers" className="text-muted-foreground hover:text-foreground transition-colors">Scrapers</Link>
          <Link to="/workflows" className="text-muted-foreground hover:text-foreground transition-colors">Workflows</Link>
        </nav>
      </header>
      <main className="max-w-3xl mx-auto w-full px-4 py-6 flex-1 min-h-0">
        <NewsFeed />
      </main>
    </div>
  )
}

import { createFileRoute, Link } from '@tanstack/react-router'
import {
  DndContext,
  closestCenter,
  PointerSensor,
  useSensor,
  useSensors,
} from '@dnd-kit/core'
import type { DragEndEvent } from '@dnd-kit/core'
import { SortableContext, rectSortingStrategy } from '@dnd-kit/sortable'
import { Eye, EyeOff } from 'lucide-react'
import { Button } from '~/components/ui/button'
import { Separator } from '~/components/ui/separator'
import { DashboardCard } from '~/components/dashboard/DashboardCard'
import { useDashboardLayout, type CardId } from '~/components/dashboard/useDashboardLayout'

export const Route = createFileRoute('/dashboard')({
  component: DashboardPage,
})

function DashboardPage() {
  const { cards, reorder, toggleVisible, resize } = useDashboardLayout()

  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 8 } }),
  )

  function handleDragEnd(event: DragEndEvent) {
    const { active, over } = event
    if (over && active.id !== over.id) {
      reorder(active.id as CardId, over.id as CardId)
    }
  }

  return (
    <div className="h-screen overflow-x-hidden overflow-y-auto bg-background text-foreground">
      <header className="sticky top-0 z-10 border-b bg-background/90 backdrop-blur-sm px-6 py-3 flex items-center justify-between shrink-0">
        <div className="flex items-center gap-4">
          <h1 className="text-lg font-semibold tracking-tight">immaiwin</h1>
          <Separator orientation="vertical" className="h-5" />
          <nav className="flex items-center gap-3 text-sm">
            <Link to="/" className="text-muted-foreground hover:text-foreground transition-colors">Polymarket</Link>
            <Link to="/news" className="text-muted-foreground hover:text-foreground transition-colors">News</Link>
            <Link to="/options" className="text-muted-foreground hover:text-foreground transition-colors">Options</Link>
            <Link to="/futures" className="text-muted-foreground hover:text-foreground transition-colors">Futures</Link>
            <Link to="/dashboard" className="text-foreground font-medium">Dashboard</Link>
            <Link to="/scrapers" className="text-muted-foreground hover:text-foreground transition-colors">Scrapers</Link>
          </nav>
        </div>
        {/* Feed visibility toggles */}
        <div className="flex items-center gap-1.5">
          {cards.map((card) => (
            <Button
              key={card.id}
              variant={card.visible ? 'secondary' : 'ghost'}
              size="sm"
              className="gap-1 h-7 px-2 text-xs"
              onClick={() => toggleVisible(card.id)}
            >
              {card.visible
                ? <Eye className="h-3 w-3" />
                : <EyeOff className="h-3 w-3" />}
              {card.label}
            </Button>
          ))}
        </div>
      </header>

      <DndContext
        sensors={sensors}
        collisionDetection={closestCenter}
        onDragEnd={handleDragEnd}
      >
        <SortableContext items={cards.map((c) => c.id)} strategy={rectSortingStrategy}>
          <div className="flex flex-wrap items-start gap-4 p-4">
            {cards.map((card) => (
              <DashboardCard
                key={card.id}
                card={card}
                onToggleVisible={toggleVisible}
                onResize={resize}
              />
            ))}
          </div>
        </SortableContext>
      </DndContext>
    </div>
  )
}

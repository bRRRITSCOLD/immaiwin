import { useCallback } from 'react'
import { useSortable } from '@dnd-kit/sortable'
import { CSS } from '@dnd-kit/utilities'
import { Eye, EyeOff, GripVertical } from 'lucide-react'
import { Card } from '~/components/ui/card'
import { type CardId, type CardState } from './useDashboardLayout'
import { useResizeHandle } from './useResizeHandle'
import { TradesFeed } from '~/components/feeds/TradesFeed'
import { OptionsFeed } from '~/components/feeds/OptionsFeed'
import { FuturesFeed } from '~/components/feeds/FuturesFeed'
import { NewsFeed } from '~/components/feeds/NewsFeed'

const FEED_MAP: Record<CardId, React.ComponentType> = {
  trades: TradesFeed,
  options: OptionsFeed,
  futures: FuturesFeed,
  news: NewsFeed,
}

interface DashboardCardProps {
  card: CardState
  onToggleVisible: (id: CardId) => void
  onResize: (id: CardId, width: number, height: number) => void
}

export function DashboardCard({ card, onToggleVisible, onResize }: DashboardCardProps) {
  const {
    attributes,
    listeners,
    setNodeRef,
    transform,
    transition,
    isDragging,
  } = useSortable({ id: card.id })

  const style: React.CSSProperties = {
    transform: CSS.Transform.toString(transform),
    transition,
    flex: `0 0 ${card.width}px`,
    height: card.visible ? `${card.height}px` : 'auto',
    opacity: isDragging ? 0.5 : 1,
    position: 'relative',
  }

  const handleResize = useCallback(
    (w: number, h: number) => onResize(card.id, w, h),
    [card.id, onResize],
  )

  const { handlePointerDown, handlePointerMove, handlePointerUp } = useResizeHandle(handleResize)

  const FeedComponent = FEED_MAP[card.id]

  return (
    <div ref={setNodeRef} style={style} className="min-w-0">
      <Card className={`flex flex-col overflow-hidden py-0 gap-0 ${card.visible ? 'h-full' : ''}`}>
        {/* Drag handle header */}
        <div
          className="flex items-center justify-between px-3 py-2 border-b bg-muted/30 select-none cursor-grab active:cursor-grabbing shrink-0"
          {...attributes}
          {...listeners}
        >
          <div className="flex items-center gap-2">
            <GripVertical className="h-4 w-4 text-muted-foreground/60" />
            <span className="text-sm font-medium">{card.label}</span>
          </div>
          <button
            className="text-muted-foreground hover:text-foreground transition-colors p-0.5 rounded"
            onClick={(e) => {
              e.stopPropagation()
              onToggleVisible(card.id)
            }}
            onPointerDown={(e) => e.stopPropagation()}
          >
            {card.visible
              ? <Eye className="h-3.5 w-3.5" />
              : <EyeOff className="h-3.5 w-3.5" />}
          </button>
        </div>

        {/* Feed body */}
        {card.visible && (
          <div className="flex-1 overflow-hidden min-h-0 p-3">
            <FeedComponent />
          </div>
        )}
      </Card>

      {/* Resize handle — bottom-right corner */}
      {card.visible && (
        <div
          className="absolute bottom-0 right-0 w-4 h-4 cursor-se-resize z-10 flex items-end justify-end p-0.5"
          style={{ touchAction: 'none' }}
          onPointerDown={(e) => handlePointerDown(e, card.width, card.height)}
          onPointerMove={handlePointerMove}
          onPointerUp={handlePointerUp}
        >
          <svg width="10" height="10" viewBox="0 0 10 10" className="text-muted-foreground/40">
            <circle cx="2" cy="8" r="1.2" fill="currentColor" />
            <circle cx="5" cy="8" r="1.2" fill="currentColor" />
            <circle cx="8" cy="8" r="1.2" fill="currentColor" />
            <circle cx="5" cy="5" r="1.2" fill="currentColor" />
            <circle cx="8" cy="5" r="1.2" fill="currentColor" />
            <circle cx="8" cy="2" r="1.2" fill="currentColor" />
          </svg>
        </div>
      )}
    </div>
  )
}

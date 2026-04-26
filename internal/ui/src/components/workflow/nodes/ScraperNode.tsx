import { Handle, Position, type NodeProps } from '@xyflow/react'
import { Rss } from 'lucide-react'

export function ScraperNode({ data }: NodeProps) {
  const source = (data?.source as string) ?? 'unknown'
  const feedUrl = (data?.feed_url as string) ?? ''

  return (
    <div className="w-[220px] rounded-lg border bg-card text-card-foreground shadow-sm">
      <div className="flex items-center gap-2 px-4 py-3 border-b">
        <Rss className="h-4 w-4 text-muted-foreground shrink-0" />
        <span className="text-sm font-medium truncate">{source}</span>
      </div>
      {feedUrl && (
        <div className="px-4 py-2">
          <p className="text-xs text-muted-foreground truncate">{feedUrl}</p>
        </div>
      )}
      <Handle type="target" position={Position.Left} />
      <Handle type="source" position={Position.Right} />
    </div>
  )
}

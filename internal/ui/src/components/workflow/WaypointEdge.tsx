import { useCallback } from 'react'
import {
  getBezierPath,
  getSmoothStepPath,
  getStraightPath,
  EdgeLabelRenderer,
  useReactFlow,
  type EdgeProps,
} from '@xyflow/react'

type PathFn = (params: {
  sourceX: number
  sourceY: number
  targetX: number
  targetY: number
  sourcePosition: any
  targetPosition: any
}) => [string, number, number]

const pathFns: Record<string, PathFn> = {
  default: getBezierPath,
  smoothstep: getSmoothStepPath as PathFn,
  step: ((p: any) => getSmoothStepPath({ ...p, borderRadius: 0 })) as PathFn,
  straight: getStraightPath as PathFn,
}

export interface Waypoint {
  x: number
  y: number
}

export function WaypointEdge(props: EdgeProps) {
  const {
    id,
    sourceX,
    sourceY,
    targetX,
    targetY,
    sourcePosition,
    targetPosition,
    style = {},
    label,
    labelStyle,
    selected,
    data,
  } = props

  const { setEdges, screenToFlowPosition } = useReactFlow()
  const waypoints: Waypoint[] = (data?.waypoints as Waypoint[]) ?? []
  const routing = (data?.routing as string) || 'default'

  let edgePath: string
  let labelX: number
  let labelY: number

  if (waypoints.length > 0) {
    const points = [
      { x: sourceX, y: sourceY },
      ...waypoints,
      { x: targetX, y: targetY },
    ]
    edgePath =
      `M ${points[0].x},${points[0].y}` +
      points.slice(1).map((p) => ` L ${p.x},${p.y}`).join('')

    // Label at midpoint of full path
    const mid = Math.floor(points.length / 2)
    if (points.length % 2 === 0) {
      labelX = (points[mid - 1].x + points[mid].x) / 2
      labelY = (points[mid - 1].y + points[mid].y) / 2
    } else {
      labelX = points[mid].x
      labelY = points[mid].y
    }
  } else {
    const getPath = pathFns[routing] || getBezierPath
    ;[edgePath, labelX, labelY] = getPath({
      sourceX,
      sourceY,
      targetX,
      targetY,
      sourcePosition,
      targetPosition,
    })
  }

  const strokeColor = (style as Record<string, any>).stroke ?? '#22c55e'

  const onWaypointPointerDown = useCallback(
    (e: React.PointerEvent, wpIndex: number) => {
      e.stopPropagation()
      e.preventDefault()
      const el = e.currentTarget as HTMLElement
      el.setPointerCapture(e.pointerId)

      const onMove = (ev: PointerEvent) => {
        const flow = screenToFlowPosition({ x: ev.clientX, y: ev.clientY })
        setEdges((eds) =>
          eds.map((ed) => {
            if (ed.id !== id) return ed
            const wps = [...((ed.data?.waypoints as Waypoint[]) ?? [])]
            wps[wpIndex] = { x: flow.x, y: flow.y }
            return { ...ed, data: { ...ed.data, waypoints: wps } }
          }),
        )
      }

      const onUp = () => {
        el.removeEventListener('pointermove', onMove)
        el.removeEventListener('pointerup', onUp)
      }

      el.addEventListener('pointermove', onMove)
      el.addEventListener('pointerup', onUp)
    },
    [id, setEdges, screenToFlowPosition],
  )

  const removeWaypoint = useCallback(
    (e: React.MouseEvent, wpIndex: number) => {
      e.preventDefault()
      e.stopPropagation()
      setEdges((eds) =>
        eds.map((ed) => {
          if (ed.id !== id) return ed
          const wps = [...((ed.data?.waypoints as Waypoint[]) ?? [])]
          wps.splice(wpIndex, 1)
          return { ...ed, data: { ...ed.data, waypoints: wps } }
        }),
      )
    },
    [id, setEdges],
  )

  const onLabelPointerDown = useCallback(
    (e: React.PointerEvent) => {
      e.stopPropagation()
      e.preventDefault()
      const el = e.currentTarget as HTMLElement
      el.setPointerCapture(e.pointerId)
      el.style.cursor = 'grabbing'

      const onMove = (ev: PointerEvent) => {
        const flow = screenToFlowPosition({ x: ev.clientX, y: ev.clientY })
        setEdges((eds) =>
          eds.map((ed) => {
            if (ed.id !== id) return ed
            return { ...ed, data: { ...ed.data, labelPosition: { x: flow.x, y: flow.y } } }
          }),
        )
      }

      const onUp = () => {
        el.style.cursor = 'grab'
        el.removeEventListener('pointermove', onMove)
        el.removeEventListener('pointerup', onUp)
      }

      el.addEventListener('pointermove', onMove)
      el.addEventListener('pointerup', onUp)
    },
    [id, setEdges, screenToFlowPosition],
  )

  return (
    <>
      {/* invisible fat path for click/hover interaction */}
      <path
        d={edgePath}
        stroke="transparent"
        strokeWidth={20}
        fill="none"
        className="react-flow__edge-interaction"
      />
      {/* visible path */}
      <path
        d={edgePath}
        fill="none"
        className="react-flow__edge-path"
        style={style}
      />
      {/* draggable label */}
      {label && (() => {
        const lp = data?.labelPosition as { x: number; y: number } | undefined
        const lx = lp?.x ?? labelX
        const ly = lp?.y ?? labelY
        return (
          <EdgeLabelRenderer>
            <div
              className="nodrag nopan"
              onPointerDown={onLabelPointerDown}
              style={{
                position: 'absolute',
                transform: `translate(-50%, -50%) translate(${lx}px, ${ly}px)`,
                fontSize: 10,
                color: '#ffffff',
                background: 'hsl(var(--background))',
                padding: '2px 8px',
                borderRadius: 4,
                border: '1px solid hsl(var(--border))',
                pointerEvents: 'all',
                cursor: 'grab',
                zIndex: 1,
                ...(labelStyle as React.CSSProperties),
              }}
            >
              {label}
            </div>
          </EdgeLabelRenderer>
        )
      })()}
      {/* waypoint circles — only visible when edge selected */}
      {selected && waypoints.length > 0 && (
        <EdgeLabelRenderer>
          {waypoints.map((wp, i) => (
            <div
              key={i}
              className="nodrag nopan"
              onPointerDown={(e) => onWaypointPointerDown(e, i)}
              onContextMenu={(e) => removeWaypoint(e, i)}
              style={{
                position: 'absolute',
                transform: `translate(-50%, -50%) translate(${wp.x}px, ${wp.y}px)`,
                width: 8,
                height: 8,
                borderRadius: '50%',
                background: strokeColor,
                border: '2px solid hsl(var(--background))',
                cursor: 'grab',
                pointerEvents: 'all',
              }}
              onMouseEnter={(e) => {
                ;(e.currentTarget as HTMLElement).style.transform =
                  `translate(-50%, -50%) translate(${wp.x}px, ${wp.y}px) scale(1.5)`
              }}
              onMouseLeave={(e) => {
                ;(e.currentTarget as HTMLElement).style.transform =
                  `translate(-50%, -50%) translate(${wp.x}px, ${wp.y}px)`
              }}
            />
          ))}
        </EdgeLabelRenderer>
      )}
    </>
  )
}

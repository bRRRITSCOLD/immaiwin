import {
  getBezierPath,
  getSmoothStepPath,
  getStraightPath,
  EdgeLabelRenderer,
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

/**
 * Two-layer custom edge: solid color underneath + dashed purple on top.
 * Creates alternating purple / underColor dash pattern.
 * Supports routing via data.routing (bezier/smoothstep/step/straight).
 */
function BodyEdge({
  underColor,
  underColorSel,
  ...props
}: EdgeProps & { underColor: string; underColorSel: string }) {
  const {
    sourceX,
    sourceY,
    targetX,
    targetY,
    sourcePosition,
    targetPosition,
    label,
    selected,
    data,
  } = props

  const routing = (data?.routing as string) || 'default'
  const getPath = pathFns[routing] || getBezierPath

  const [edgePath, labelX, labelY] = getPath({
    sourceX,
    sourceY,
    targetX,
    targetY,
    sourcePosition,
    targetPosition,
  })

  const purple = selected ? '#ddd6fe' : '#a78bfa'
  const under = selected ? underColorSel : underColor

  return (
    <>
      {/* invisible fat path for click/selection interaction */}
      <path d={edgePath} stroke="transparent" strokeWidth={12} fill="none" />
      {/* color dashes — offset so they alternate: purple, gap, underColor, gap */}
      <path
        d={edgePath}
        stroke={under}
        strokeWidth={2.5}
        fill="none"
        strokeDasharray="8 12"
        strokeDashoffset="-10"
      />
      <path
        d={edgePath}
        stroke={purple}
        strokeWidth={2.5}
        fill="none"
        strokeDasharray="8 12"
      />
      <EdgeLabelRenderer>
        <div
          className="nodrag nopan"
          style={{
            position: 'absolute',
            transform: `translate(-50%, -50%) translate(${labelX}px, ${labelY}px)`,
            fontSize: 10,
            color: '#ffffff',
            background: 'hsl(var(--background))',
            padding: '2px 8px',
            borderRadius: 4,
            border: '1px solid hsl(var(--border))',
            pointerEvents: 'all',
          }}
        >
          {label}
        </div>
      </EdgeLabelRenderer>
    </>
  )
}

export function BodySuccessEdge(props: EdgeProps) {
  return (
    <BodyEdge
      {...props}
      underColor="#22c55e"
      underColorSel="#86efac"
    />
  )
}

export function BodyErrorEdge(props: EdgeProps) {
  return (
    <BodyEdge
      {...props}
      underColor="#ef4444"
      underColorSel="#fca5a5"
    />
  )
}

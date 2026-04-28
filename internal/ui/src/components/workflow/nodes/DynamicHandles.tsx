import { useCallback, useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Handle, Position, useReactFlow, useEdges, useUpdateNodeInternals } from '@xyflow/react'
import { ChevronRight } from 'lucide-react'
import { useWorkflowStore, type EdgePaletteType } from '../useWorkflowStore'

export interface DynamicHandle {
  id: string
  side: 'top' | 'right' | 'bottom' | 'left'
  offset: number // 0-100% along that side
  paletteType: string // 'start' | 'success' | 'error' | 'item' | 'receive'
}

const sideToPosition: Record<string, Position> = {
  top: Position.Top,
  right: Position.Right,
  bottom: Position.Bottom,
  left: Position.Left,
}

const paletteColors: Record<string, string> = {
  start: '#3b82f6',
  success: '#22c55e',
  error: '#ef4444',
  item: '#a78bfa',
  tool: '#c084fc',
  receive: '#888888',
}

const allPaletteTypes: { type: EdgePaletteType; label: string; color: string }[] = [
  { type: 'start', label: 'Start', color: '#3b82f6' },
  { type: 'success', label: 'Success', color: '#22c55e' },
  { type: 'error', label: 'Error', color: '#ef4444' },
  { type: 'item', label: 'Item', color: '#a78bfa' },
  { type: 'tool', label: 'Tool', color: '#c084fc' },
  { type: 'receive', label: 'Receive', color: '#888888' },
]

/**
 * Returns a glow style for a source handle when its palette type is active.
 */
export function useHandleHighlight(handlePaletteType: string): React.CSSProperties {
  const active = useWorkflowStore((s) => s.selectedEdgeType)
  if (active !== handlePaletteType) return {}
  return {
    boxShadow: '0 0 8px 3px rgba(255,255,255,0.7)',
    outline: '2px solid rgba(255,255,255,0.9)',
    outlineOffset: '1px',
  }
}

/** Default handles auto-seeded when data.handles is undefined */
function defaultHandlesForType(nodeType: string): DynamicHandle[] {
  const receiveHandles: DynamicHandle[] = [
    { id: 'in-top', side: 'top', offset: 50, paletteType: 'receive' },
    { id: 'in-left', side: 'left', offset: 50, paletteType: 'receive' },
    { id: 'in-bottom', side: 'bottom', offset: 50, paletteType: 'receive' },
  ]

  switch (nodeType) {
    case 'trigger':
      return [
        { id: 'start-out', side: 'right', offset: 50, paletteType: 'start' },
        ...receiveHandles,
      ]
    case 'for_each':
      return [
        { id: 'item', side: 'right', offset: 30, paletteType: 'item' },
        { id: 'success', side: 'right', offset: 60, paletteType: 'success' },
        { id: 'error', side: 'right', offset: 85, paletteType: 'error' },
        ...receiveHandles,
      ]
    default:
      return [
        { id: 'success', side: 'right', offset: 35, paletteType: 'success' },
        { id: 'error', side: 'right', offset: 65, paletteType: 'error' },
        ...receiveHandles,
      ]
  }
}

/**
 * Validate palette edge type against source node type.
 */
function isPaletteValidForNode(nodeType: string, paletteType: string): boolean {
  switch (paletteType) {
    case 'start': return nodeType === 'trigger'
    case 'item': return nodeType === 'for_each'
    case 'success':
    case 'error': return nodeType !== 'trigger'
    case 'receive': return true
    default: return true
  }
}

interface HandleMenuState {
  dhId: string
  screenX: number
  screenY: number
}

interface BorderMenuState {
  side: string
  offset: number
  screenX: number
  screenY: number
}

interface Props {
  nodeId: string
  nodeType: string
  data: Record<string, unknown>
}

/**
 * Renders all handles (source + target) for a node. Auto-seeds defaults.
 * Border zones always active: hover → crosshair cursor + ghost dot,
 * right-click → ctx menu with "Add Handle" submenu, left-click adds when palette active.
 * Handles have their own ctx menu (delete/move/attach).
 */
export function DynamicHandles({ nodeId, nodeType, data }: Props) {
  const { updateNodeData, setEdges } = useReactFlow()
  const edges = useEdges()
  const updateNodeInternals = useUpdateNodeInternals()
  const paletteType = useWorkflowStore((s) => s.selectedEdgeType)
  const setAttachingFrom = useWorkflowStore((s) => s.setAttachingFrom)

  // Auto-seed default handles if undefined
  const handles = (data.handles as DynamicHandle[] | undefined) ?? (() => {
    const defaults = defaultHandlesForType(nodeType)
    queueMicrotask(() => updateNodeData(nodeId, { handles: defaults }))
    return defaults
  })()

  // Update node internals when handles change so RF registers them
  const handleIds = handles.map((h) => h.id).join(',')
  useEffect(() => {
    if (handleIds) {
      updateNodeInternals(nodeId)
    }
  }, [handleIds, nodeId, updateNodeInternals])

  const [ghost, setGhost] = useState<{ side: string; offset: number } | null>(null)
  const [handleMenu, setHandleMenu] = useState<HandleMenuState | null>(null)
  const [borderMenu, setBorderMenu] = useState<BorderMenuState | null>(null)
  const [borderSubmenuOpen, setBorderSubmenuOpen] = useState(false)
  const [movingHandle, setMovingHandle] = useState<DynamicHandle | null>(null)
  const zoneRefs = useRef<Record<string, HTMLDivElement | null>>({})

  const showPaletteZones = (paletteType != null && isPaletteValidForNode(nodeType, paletteType)) || movingHandle != null

  // Close menus on Escape
  useEffect(() => {
    if (!handleMenu && !movingHandle && !borderMenu) return
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        setHandleMenu(null)
        setBorderMenu(null)
        setBorderSubmenuOpen(false)
        setMovingHandle(null)
        setGhost(null)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [handleMenu, movingHandle, borderMenu])

  const calcOffset = useCallback(
    (side: string, e: React.MouseEvent) => {
      const el = zoneRefs.current[side]
      if (!el) return 50
      const rect = el.getBoundingClientRect()
      let offset: number
      if (side === 'top' || side === 'bottom') {
        offset = ((e.clientX - rect.left) / rect.width) * 100
      } else {
        offset = ((e.clientY - rect.top) / rect.height) * 100
      }
      return Math.max(0, Math.min(100, offset))
    },
    [],
  )

  const onZoneMove = useCallback(
    (side: string, e: React.MouseEvent) => {
      setGhost({ side, offset: calcOffset(side, e) })
    },
    [calcOffset],
  )

  const onZoneLeave = useCallback(() => {
    if (!movingHandle) setGhost(null)
  }, [movingHandle])

  const onZoneClick = useCallback(
    (side: string, e: React.MouseEvent) => {
      // Move mode: reposition existing handle
      if (movingHandle) {
        const offset = Math.max(2, Math.min(98, calcOffset(side, e)))
        const prev = (data.handles as DynamicHandle[] | undefined) ?? []
        updateNodeData(nodeId, {
          handles: prev.map((h) =>
            h.id === movingHandle.id
              ? { ...h, side: side as DynamicHandle['side'], offset }
              : h,
          ),
        })
        setMovingHandle(null)
        setGhost(null)
        return
      }

      // Palette active: create handle from selected palette type
      if (paletteType && isPaletteValidForNode(nodeType, paletteType)) {
        const offset = Math.max(2, Math.min(98, calcOffset(side, e)))
        const dh: DynamicHandle = {
          id: `dh-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
          side: side as DynamicHandle['side'],
          offset,
          paletteType,
        }
        const prev = (data.handles as DynamicHandle[] | undefined) ?? []
        updateNodeData(nodeId, { handles: [...prev, dh] })
      }
    },
    [paletteType, data.handles, nodeId, nodeType, updateNodeData, movingHandle, calcOffset],
  )

  // Right-click on border zone → show border ctx menu
  const onZoneContextMenu = useCallback(
    (side: string, e: React.MouseEvent) => {
      e.preventDefault()
      e.stopPropagation()
      const offset = Math.max(2, Math.min(98, calcOffset(side, e)))
      setBorderMenu({ side, offset, screenX: e.clientX, screenY: e.clientY })
      setBorderSubmenuOpen(false)
    },
    [calcOffset],
  )

  // Add handle from border ctx menu submenu
  function addHandleFromBorderMenu(pt: EdgePaletteType) {
    if (!borderMenu) return
    const dh: DynamicHandle = {
      id: `dh-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
      side: borderMenu.side as DynamicHandle['side'],
      offset: borderMenu.offset,
      paletteType: pt,
    }
    const prev = (data.handles as DynamicHandle[] | undefined) ?? []
    updateNodeData(nodeId, { handles: [...prev, dh] })
    setBorderMenu(null)
    setBorderSubmenuOpen(false)
  }

  // Handle right-click ctx menu
  const onHandleContextMenu = useCallback(
    (e: React.MouseEvent, dhId: string) => {
      e.preventDefault()
      e.stopPropagation()
      setHandleMenu({ dhId, screenX: e.clientX, screenY: e.clientY })
    },
    [],
  )

  function handleDeleteHandle() {
    if (!handleMenu) return
    const prev = (data.handles as DynamicHandle[] | undefined) ?? []
    updateNodeData(nodeId, { handles: prev.filter((h) => h.id !== handleMenu.dhId) })
    setEdges((eds) =>
      eds.filter((ed) =>
        !(ed.source === nodeId && ed.sourceHandle === handleMenu.dhId) &&
        !(ed.target === nodeId && ed.targetHandle === handleMenu.dhId)
      ),
    )
    setHandleMenu(null)
  }

  function handleMoveHandle() {
    if (!handleMenu) return
    const prev = (data.handles as DynamicHandle[] | undefined) ?? []
    const dh = prev.find((h) => h.id === handleMenu.dhId)
    if (dh) setMovingHandle(dh)
    setHandleMenu(null)
  }

  function handleAttachHandle() {
    if (!handleMenu) return
    const prev = (data.handles as DynamicHandle[] | undefined) ?? []
    const dh = prev.find((h) => h.id === handleMenu.dhId)
    if (dh) {
      setAttachingFrom({ nodeId, handleId: dh.id, paletteType: dh.paletteType })
    }
    setHandleMenu(null)
  }

  // Valid palette types for this node (for border submenu)
  const validPaletteTypes = allPaletteTypes.filter((p) => isPaletteValidForNode(nodeType, p.type))

  // Zone styles — always pointer events, cursor crosshair when palette or always for right-click
  const zoneBase = 'absolute nodrag nopan'

  const zones: { side: string; style: React.CSSProperties }[] = [
    { side: 'top', style: { top: -6, left: 0, right: 0, height: 12 } },
    { side: 'bottom', style: { bottom: -6, left: 0, right: 0, height: 12 } },
    { side: 'left', style: { top: 0, bottom: 0, left: -6, width: 12 } },
    { side: 'right', style: { top: 0, bottom: 0, right: -6, width: 12 } },
  ]

  const ghostColor = movingHandle
    ? paletteColors[movingHandle.paletteType] ?? '#888'
    : paletteType ? paletteColors[paletteType] ?? '#888' : '#888'

  return (
    <>
      {/* Border hover zones — always active for right-click + ghost */}
      {zones.map((z) => (
        <div
          key={z.side}
          ref={(el) => { zoneRefs.current[z.side] = el }}
          className={zoneBase}
          style={{
            ...z.style,
            cursor: showPaletteZones ? 'cell' : 'crosshair',
            pointerEvents: 'all',
            opacity: 0,
            zIndex: 4,
          }}
          onMouseMove={(e) => onZoneMove(z.side, e)}
          onMouseLeave={onZoneLeave}
          onClick={(e) => onZoneClick(z.side, e)}
          onContextMenu={(e) => onZoneContextMenu(z.side, e)}
        />
      ))}

      {/* Ghost preview dot — only when palette active or moving */}
      {ghost && showPaletteZones && (
        <GhostDot side={ghost.side} offset={ghost.offset} color={ghostColor} />
      )}

      {/* All handles */}
      {handles.map((dh) => (
        <DynamicHandleEl
          key={dh.id}
          dh={dh}
          isMoving={movingHandle?.id === dh.id}
          onContextMenu={(e) => onHandleContextMenu(e, dh.id)}
        />
      ))}

      {/* Border right-click context menu — portalled */}
      {borderMenu && createPortal(
        <>
          <div className="fixed inset-0 z-40" onClick={() => { setBorderMenu(null); setBorderSubmenuOpen(false) }} />
          <div
            className="fixed z-50 bg-popover border border-border rounded-md shadow-lg py-1 min-w-[160px]"
            style={{ left: borderMenu.screenX, top: borderMenu.screenY }}
          >
            <div
              className="relative"
              onMouseEnter={() => setBorderSubmenuOpen(true)}
              onMouseLeave={() => setBorderSubmenuOpen(false)}
            >
              <button
                className="w-full text-left px-3 py-1.5 text-sm hover:bg-accent hover:text-accent-foreground transition-colors flex items-center justify-between"
              >
                Add Handle
                <ChevronRight className="h-3.5 w-3.5 text-muted-foreground ml-4" />
              </button>
              {borderSubmenuOpen && (
                <div
                  className="absolute left-full top-0 bg-popover border border-border rounded-md shadow-lg py-1 min-w-[130px] z-50"
                >
                  {validPaletteTypes.map((p) => (
                    <button
                      key={p.type}
                      className="w-full text-left px-3 py-1.5 text-sm hover:bg-accent hover:text-accent-foreground transition-colors flex items-center gap-2"
                      onClick={() => addHandleFromBorderMenu(p.type)}
                    >
                      <span
                        className="inline-block w-2.5 h-2.5 rounded-full shrink-0"
                        style={{ background: p.color }}
                      />
                      {p.label}
                    </button>
                  ))}
                </div>
              )}
            </div>
          </div>
        </>,
        document.body,
      )}

      {/* Handle right-click context menu — portalled */}
      {handleMenu && createPortal(
        <>
          <div className="fixed inset-0 z-40" onClick={() => setHandleMenu(null)} />
          <div
            className="fixed z-50 bg-popover border border-border rounded-md shadow-lg py-1 min-w-[140px]"
            style={{ left: handleMenu.screenX, top: handleMenu.screenY }}
          >
            <button
              className="w-full text-left px-3 py-1.5 text-sm hover:bg-accent hover:text-accent-foreground transition-colors"
              onClick={handleMoveHandle}
            >
              Move Handle
            </button>
            <button
              className="w-full text-left px-3 py-1.5 text-sm hover:bg-accent hover:text-accent-foreground transition-colors"
              onClick={handleAttachHandle}
            >
              Attach
            </button>
            <button
              className="w-full text-left px-3 py-1.5 text-sm text-red-400 hover:bg-accent hover:text-red-300 transition-colors"
              onClick={handleDeleteHandle}
            >
              Delete Handle
            </button>
          </div>
        </>,
        document.body,
      )}
    </>
  )
}

function GhostDot({ side, offset, color }: { side: string; offset: number; color: string }) {
  return (
    <div
      className="absolute rounded-full pointer-events-none"
      style={{
        width: 10,
        height: 10,
        background: color,
        opacity: 0.5,
        zIndex: 11,
        ...ghostPositionStyle(side, offset),
      }}
    />
  )
}

function DynamicHandleEl({
  dh,
  isMoving,
  onContextMenu,
}: {
  dh: DynamicHandle
  isMoving: boolean
  onContextMenu: (e: React.MouseEvent) => void
}) {
  const glow = useHandleHighlight(dh.paletteType)
  const color = paletteColors[dh.paletteType] ?? '#888'
  const isReceive = dh.paletteType === 'receive'

  return (
    <Handle
      type={isReceive ? 'target' : 'source'}
      position={sideToPosition[dh.side] ?? Position.Right}
      id={dh.id}
      isConnectable
      onContextMenu={onContextMenu}
      style={{
        background: color,
        zIndex: 10,
        opacity: isMoving ? 0.3 : 1,
        ...handleOffsetStyle(dh.side, dh.offset),
        ...glow,
      }}
    />
  )
}

function handleOffsetStyle(side: string, offset: number): React.CSSProperties {
  switch (side) {
    case 'top':
    case 'bottom': return { left: `${offset}%` }
    case 'left':
    case 'right': return { top: `${offset}%` }
    default: return {}
  }
}

function ghostPositionStyle(side: string, offset: number): React.CSSProperties {
  switch (side) {
    case 'top': return { top: 0, left: `${offset}%`, transform: 'translate(-50%, -50%)' }
    case 'bottom': return { bottom: 0, left: `${offset}%`, top: 'auto', transform: 'translate(-50%, 50%)' }
    case 'left': return { left: 0, top: `${offset}%`, transform: 'translate(-50%, -50%)' }
    case 'right': return { right: 0, top: `${offset}%`, left: 'auto', transform: 'translate(50%, -50%)' }
    default: return {}
  }
}

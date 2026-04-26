import { useCallback, useRef } from 'react'

export function useResizeHandle(
  onResize: (width: number, height: number) => void,
  minWidth = 320,
  minHeight = 300,
) {
  const startRef = useRef<{ x: number; y: number; w: number; h: number } | null>(null)

  const handlePointerDown = useCallback(
    (e: React.PointerEvent, currentWidth: number, currentHeight: number) => {
      e.stopPropagation()
      e.currentTarget.setPointerCapture(e.pointerId)
      startRef.current = { x: e.clientX, y: e.clientY, w: currentWidth, h: currentHeight }
    },
    [],
  )

  const handlePointerMove = useCallback(
    (e: React.PointerEvent) => {
      if (!startRef.current) return
      e.stopPropagation()
      const dx = e.clientX - startRef.current.x
      const dy = e.clientY - startRef.current.y
      onResize(
        Math.min(Math.max(minWidth, startRef.current.w + dx), window.innerWidth - 40),
        Math.min(Math.max(minHeight, startRef.current.h + dy), window.innerHeight - 120),
      )
    },
    [onResize, minWidth, minHeight],
  )

  const handlePointerUp = useCallback((e: React.PointerEvent) => {
    e.stopPropagation()
    startRef.current = null
  }, [])

  return { handlePointerDown, handlePointerMove, handlePointerUp }
}

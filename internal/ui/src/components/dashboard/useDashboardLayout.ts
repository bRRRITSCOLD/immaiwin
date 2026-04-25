import { useState, useCallback } from 'react'
import { arrayMove } from '@dnd-kit/sortable'

export type CardId = 'trades' | 'options' | 'futures' | 'news'

export interface CardState {
  id: CardId
  label: string
  visible: boolean
  width: number
  height: number
}

const DEFAULT_CARDS: CardState[] = [
  { id: 'trades', label: 'Polymarket', visible: true, width: 600, height: 520 },
  { id: 'options', label: 'Options', visible: true, width: 600, height: 520 },
  { id: 'futures', label: 'Futures', visible: true, width: 600, height: 520 },
  { id: 'news', label: 'News', visible: true, width: 600, height: 520 },
]

const STORAGE_KEY = 'dashboard-layout'

function loadLayout(): CardState[] {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return DEFAULT_CARDS
    const parsed = JSON.parse(raw) as CardState[]
    // Always use canonical label from defaults (label not user-editable)
    const defaultsByid = Object.fromEntries(DEFAULT_CARDS.map((c) => [c.id, c]))
    const merged = parsed.map((c) => ({ ...c, label: defaultsByid[c.id]?.label ?? c.label }))
    // Append any new default cards not yet in saved layout
    const ids = new Set(parsed.map((c) => c.id))
    const missing = DEFAULT_CARDS.filter((c) => !ids.has(c.id))
    return [...merged, ...missing]
  } catch {
    return DEFAULT_CARDS
  }
}

function saveLayout(cards: CardState[]) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(cards))
  } catch { /* ignore quota errors */ }
}

export function useDashboardLayout() {
  const [cards, setCards] = useState<CardState[]>(() => loadLayout())

  const updateCards = useCallback((updater: (prev: CardState[]) => CardState[]) => {
    setCards((prev) => {
      const next = updater(prev)
      saveLayout(next)
      return next
    })
  }, [])

  const reorder = useCallback((activeId: CardId, overId: CardId) => {
    updateCards((prev) => {
      const oldIdx = prev.findIndex((c) => c.id === activeId)
      const newIdx = prev.findIndex((c) => c.id === overId)
      return arrayMove(prev, oldIdx, newIdx)
    })
  }, [updateCards])

  const toggleVisible = useCallback((id: CardId) => {
    updateCards((prev) => prev.map((c) => c.id === id ? { ...c, visible: !c.visible } : c))
  }, [updateCards])

  const resize = useCallback((id: CardId, width: number, height: number) => {
    updateCards((prev) => prev.map((c) => c.id === id ? { ...c, width, height } : c))
  }, [updateCards])

  return { cards, reorder, toggleVisible, resize }
}

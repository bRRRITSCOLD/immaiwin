// Shared keydown handler for JSON textareas across the workflow
// canvas. Tab inserts 2 spaces at the caret instead of shifting
// browser focus, matching every code editor convention. Caller
// passes the controlled value + setter so the helper can update
// caret position after the insertion (otherwise the cursor jumps
// to the end on next render).
import type { KeyboardEvent } from 'react'

const INDENT = '  '

export function handleJsonTextareaTab(
  e: KeyboardEvent<HTMLTextAreaElement>,
  value: string,
  commit: (next: string) => void,
) {
  if (e.key !== 'Tab') return
  // Allow shift+tab to keep its native "unfocus" affordance —
  // outdent in a freeform textarea isn't useful here, but keeping
  // shift+tab native means users can still escape the field with
  // the keyboard.
  if (e.shiftKey) return
  e.preventDefault()
  const ta = e.currentTarget
  const start = ta.selectionStart
  const end = ta.selectionEnd
  const next = value.slice(0, start) + INDENT + value.slice(end)
  commit(next)
  // Restore caret to AFTER the inserted indent on the next render
  // tick — React replaces the DOM node's value asynchronously.
  requestAnimationFrame(() => {
    ta.selectionStart = start + INDENT.length
    ta.selectionEnd = start + INDENT.length
  })
}

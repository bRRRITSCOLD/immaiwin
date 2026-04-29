// NumberField — controlled number input that lets users type intermediate
// states like "0.", "0.0", "" without snapping back to a parsed integer.
//
// The shadcn `Input type="number"` component composed with a typical
// `value={n} onChange={(e) => setN(Number(e.target.value))}` pattern is
// broken for floats: the keystroke `0.` parses to Number("0.") === 0,
// the controlled re-render writes "0" back into the DOM, and the period
// is eaten on every iteration. Users can't enter `0.5`, `0.001`, etc.
//
// Fix: this component holds its own STRING buffer for the in-flight
// value, mirrors the parent's number into the buffer only when the
// numeric value actually changes (avoids overwriting `"0."` while
// typing), and only emits onChange(number) when the buffer parses to a
// finite number. Empty buffer emits the configured `emptyValue`
// (default 0). Trailing-dot buffer (`"3."`) keeps `3` as the emitted
// number, but the DOM still shows `3.` so the user can keep typing.
//
// Drop-in replacement for `<Input type="number" value={n} onChange={...} />`
// at every callsite.

import * as React from 'react'
import { Input } from './input'

interface NumberFieldProps
  extends Omit<React.ComponentProps<'input'>, 'type' | 'value' | 'onChange'> {
  value: number
  onChange(value: number): void
  emptyValue?: number
}

export function NumberField({
  value,
  onChange,
  emptyValue = 0,
  className,
  ...rest
}: NumberFieldProps) {
  const [buf, setBuf] = React.useState<string>(() => formatNumber(value))

  // Sync the buffer when the parent's numeric value changes IF that
  // change didn't originate from this component's last keystroke
  // (compare parsed buffer vs incoming value). Avoids stomping `"0."`
  // mid-typing.
  React.useEffect(() => {
    const parsed = parseFinite(buf)
    if (parsed !== value) {
      setBuf(formatNumber(value))
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [value])

  function handleChange(e: React.ChangeEvent<HTMLInputElement>) {
    const next = e.target.value
    setBuf(next)
    if (next === '' || next === '-') {
      onChange(emptyValue)
      return
    }
    const n = Number(next)
    if (Number.isFinite(n)) {
      onChange(n)
    }
    // Trailing-dot / multi-dot / partial-negative buffer leaves the
    // parent's number unchanged until next valid keystroke. The buffer
    // still shows the in-flight string so the user can keep typing.
  }

  return <Input type="number" className={className} value={buf} onChange={handleChange} {...rest} />
}

function formatNumber(n: number): string {
  if (!Number.isFinite(n)) return ''
  return String(n)
}

function parseFinite(s: string): number | null {
  if (s === '') return null
  const n = Number(s)
  return Number.isFinite(n) ? n : null
}

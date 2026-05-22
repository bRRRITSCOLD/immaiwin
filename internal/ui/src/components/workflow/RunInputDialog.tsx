import { useEffect, useRef, useState } from 'react'
import { Button } from '~/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '~/components/ui/dialog'
import { Input } from '~/components/ui/input'
import { Switch } from '~/components/ui/switch'
import { Textarea } from '~/components/ui/textarea'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '~/components/ui/select'
import type { SchemaEntry } from './useWorkflowStore'
import { handleJsonTextareaTab } from './nodes/jsonTextareaTab'

/**
 * RunInputDialog renders a pre-flight input form for a workflow's
 * declared `input_schema` / `input_schema_json`. Operator types
 * values; on submit, parsed JSON-shaped input is passed to the
 * onSubmit callback (Run / Debug dispatchers). Workflows without
 * a declared schema never open the dialog — caller invokes
 * onSubmit(undefined) directly.
 *
 * Raw-schema path: textarea seeded with a minimal stub derived
 * from the schema's top-level properties (keys present, values
 * left blank). Author types JSON. Same cursor-stable + Tab→2-space
 * pattern as the other JSON textareas on the canvas.
 *
 * Typed-schema path: per-field controls. string→Input, number→
 * Input[type=number], boolean→Switch, enum→Select. Required fields
 * marked with `*`; submit refuses when any required is blank.
 */

interface Props {
  open: boolean
  onOpenChange(open: boolean): void
  // Title shown in dialog header — "Run" vs "Debug" so the
  // dispatcher distinction is visible.
  title: string
  // Workflow's typed schema (SchemaEntry[]) — when set, dialog
  // renders typed form fields.
  schema?: SchemaEntry[]
  // Raw JSON Schema — when non-empty, dialog renders a JSON
  // textarea instead of the typed form (raw wins, matching
  // engine validation priority).
  rawSchema?: string
  // Submit handler — receives the parsed input value
  // (object for typed schemas; whatever parsed for raw).
  onSubmit(input: unknown): void
}

export function RunInputDialog({ open, onOpenChange, title, schema, rawSchema, onSubmit }: Props) {
  const useRaw = !!(rawSchema && rawSchema.trim())
  const entries: SchemaEntry[] = schema ?? []

  // Typed form state — one key per SchemaEntry, default-seeded from
  // the entry's `default` (when typed) or empty.
  const [values, setValues] = useState<Record<string, string | boolean>>(() => {
    const init: Record<string, string | boolean> = {}
    for (const e of entries) {
      if (e.type === 'boolean') init[e.name] = false
      else init[e.name] = e.default ?? ''
    }
    return init
  })

  // Raw JSON state — seeded with a stub of the schema's
  // top-level properties so the author isn't staring at "{}".
  const [rawText, setRawText] = useState(() => buildRawStub(rawSchema))
  const [rawError, setRawError] = useState<string | null>(null)
  const [formError, setFormError] = useState<string | null>(null)
  const initRef = useRef(false)

  // Re-seed local state when the dialog opens (so reopening with a
  // changed schema doesn't carry stale form values).
  useEffect(() => {
    if (open && !initRef.current) {
      const init: Record<string, string | boolean> = {}
      for (const e of entries) {
        if (e.type === 'boolean') init[e.name] = false
        else init[e.name] = e.default ?? ''
      }
      setValues(init)
      setRawText(buildRawStub(rawSchema))
      setFormError(null)
      setRawError(null)
      initRef.current = true
    }
    if (!open) initRef.current = false
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  function submit() {
    if (useRaw) {
      if (rawText.trim() === '') {
        onSubmit(undefined)
        onOpenChange(false)
        return
      }
      try {
        const parsed = JSON.parse(rawText)
        setRawError(null)
        onSubmit(parsed)
        onOpenChange(false)
      } catch (err) {
        setRawError((err as Error).message)
      }
      return
    }
    // Typed path — assemble object from entries.
    const out: Record<string, unknown> = {}
    for (const e of entries) {
      const v = values[e.name]
      if (e.required && (v === '' || v === undefined || v === null)) {
        setFormError(`Required field missing: ${e.name}`)
        return
      }
      if (v === '' || v === undefined) continue // skip empty optional
      switch (e.type) {
        case 'number': {
          const n = Number(v)
          if (Number.isNaN(n)) {
            setFormError(`Field ${e.name} must be a number`)
            return
          }
          out[e.name] = n
          break
        }
        case 'boolean':
          out[e.name] = Boolean(v)
          break
        default:
          out[e.name] = v
      }
    }
    setFormError(null)
    onSubmit(out)
    onOpenChange(false)
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
        </DialogHeader>

        {useRaw ? (
          <div className="space-y-2">
            <p className="text-xs text-muted-foreground">
              Workflow declares a raw JSON Schema. Supply JSON-shaped input below.
            </p>
            <Textarea
              className="font-mono text-xs min-h-[180px] resize-y"
              rows={10}
              value={rawText}
              onKeyDown={(e) => handleJsonTextareaTab(e, rawText, setRawText)}
              onChange={(e) => {
                setRawText(e.target.value)
                setRawError(null)
              }}
            />
            {rawError && <p className="text-xs text-red-500">{rawError}</p>}
          </div>
        ) : (
          <div className="space-y-3">
            {entries.length === 0 && (
              <p className="text-xs text-muted-foreground italic">
                Workflow has no declared input fields — dispatch will run with empty input.
              </p>
            )}
            {entries.map((e) => (
              <div key={e.name} className="space-y-1">
                <label className="text-xs font-medium">
                  {e.name}
                  {e.required && <span className="text-red-500"> *</span>}
                  {e.description && (
                    <span className="ml-2 font-normal text-muted-foreground">
                      — {e.description}
                    </span>
                  )}
                </label>
                {e.type === 'boolean' ? (
                  <Switch
                    checked={Boolean(values[e.name])}
                    onCheckedChange={(v) => setValues((s) => ({ ...s, [e.name]: v }))}
                  />
                ) : e.type === 'enum' ? (
                  <Select
                    value={(values[e.name] as string) ?? ''}
                    onValueChange={(v) => setValues((s) => ({ ...s, [e.name]: v }))}
                  >
                    <SelectTrigger className="h-8 text-xs">
                      <SelectValue placeholder="— pick —" />
                    </SelectTrigger>
                    <SelectContent>
                      {(e.enum ?? []).map((opt) => (
                        <SelectItem key={opt} value={opt}>
                          {opt}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                ) : (
                  <Input
                    className="h-8 text-xs"
                    type={e.type === 'number' ? 'number' : 'text'}
                    value={(values[e.name] as string) ?? ''}
                    onChange={(ev) => setValues((s) => ({ ...s, [e.name]: ev.target.value }))}
                  />
                )}
              </div>
            ))}
            {formError && <p className="text-xs text-red-500">{formError}</p>}
          </div>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button onClick={submit}>{title}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// buildRawStub generates a starter JSON object from the schema's
// top-level properties. Each property is seeded with the JSON
// Schema `default` annotation when present; otherwise a type-
// appropriate placeholder ("" for string, 0 for number, false for
// boolean, [] for array, {} for object). Authors get a usable
// starting payload instead of having to type the whole structure.
function buildRawStub(raw: string | undefined): string {
  if (!raw || !raw.trim()) return '{}'
  try {
    const parsed = JSON.parse(raw)
    if (!parsed || typeof parsed !== 'object') return '{}'
    const props = (parsed as { properties?: Record<string, unknown> }).properties
    if (!props || typeof props !== 'object') return '{}'
    const stub: Record<string, unknown> = {}
    for (const [k, v] of Object.entries(props)) {
      stub[k] = stubValueForProperty(v)
    }
    return JSON.stringify(stub, null, 2)
  } catch {
    return '{}'
  }
}

// stubValueForProperty picks a default for a single JSON Schema
// property. Honor `default` first (the canonical mechanism for
// telling consumers "this is the suggested value"); fall back to
// a type-appropriate empty value so the resulting JSON parses
// cleanly without further edits.
//
// For nested `object` types with declared `properties`, recurse and
// seed each child. For `array` types with an `items` schema, seed
// a single element from that schema (a tuple-form `items` array
// seeds one entry per index). Depth-capped at 8 to avoid runaway
// recursion on self-referential `$ref` schemas (which we otherwise
// can't resolve without a full resolver).
function stubValueForProperty(p: unknown, depth = 0): unknown {
  if (!p || typeof p !== 'object') return ''
  const obj = p as {
    default?: unknown
    type?: string
    properties?: Record<string, unknown>
    items?: unknown
  }
  if (obj.default !== undefined) return obj.default
  if (depth >= 8) {
    switch (obj.type) {
      case 'array':
        return []
      case 'object':
        return {}
      default:
        return ''
    }
  }
  switch (obj.type) {
    case 'number':
    case 'integer':
      return 0
    case 'boolean':
      return false
    case 'array': {
      if (!obj.items) return []
      if (Array.isArray(obj.items)) {
        return obj.items.map((it) => stubValueForProperty(it, depth + 1))
      }
      return [stubValueForProperty(obj.items, depth + 1)]
    }
    case 'object': {
      if (!obj.properties || typeof obj.properties !== 'object') return {}
      const out: Record<string, unknown> = {}
      for (const [k, v] of Object.entries(obj.properties)) {
        out[k] = stubValueForProperty(v, depth + 1)
      }
      return out
    }
    default:
      return ''
  }
}

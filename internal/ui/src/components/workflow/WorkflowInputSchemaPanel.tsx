import { useEffect, useRef, useState } from 'react'
import { ChevronDown, ChevronUp, Plus, Settings2, Trash2 } from 'lucide-react'
import { Button } from '~/components/ui/button'
import { Input } from '~/components/ui/input'
import { Switch } from '~/components/ui/switch'
import { Textarea } from '~/components/ui/textarea'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '~/components/ui/select'
import type { SchemaEntry } from './useWorkflowStore'
import { handleJsonTextareaTab } from './nodes/jsonTextareaTab'

/**
 * Schema-only editor for the workflow's RUN INPUT contract.
 *
 * Distinct from `WorkflowConfigPanel` (which edits both schema AND
 * persistent values) — input is per-run / dynamic, so there is no
 * value to render here. Each entry declares `{name, type,
 * description?, required?, enum[]?}` and downstream surfaces consume
 * the schema (sub_workflow auto-derive, future typed Run dialog,
 * engine-side validation).
 *
 * Empty schema is the legacy back-compat behaviour — workflow
 * accepts any free-form input.
 */
interface Props {
  schema?: SchemaEntry[]
  onSchemaChange(schema: SchemaEntry[]): void
  // Optional raw JSON Schema for nested / array contracts the flat
  // SchemaEntry shape can't express. When set, wins over the typed
  // editor for engine validation + sub_workflow auto-derive.
  rawSchema?: string
  onRawSchemaChange(raw: string): void
  // Header label — reused for both input and output schema panels.
  // Defaults to "Input schema" to keep existing callsites stable.
  title?: string
  // Body description override — input vs output have slightly
  // different framing.
  description?: string
}

const TYPES = ['string', 'number', 'boolean', 'enum'] as const

export function WorkflowInputSchemaPanel({ schema, onSchemaChange, rawSchema, onRawSchemaChange, title = 'Input schema', description = 'Declare the shape of input this workflow accepts at run time. Sub-workflow consumers auto-derive a JSON Schema from this; the Run dialog renders typed fields. Empty = accept any input.' }: Props) {
  const [open, setOpen] = useState(false)
  const [expandedSettings, setExpandedSettings] = useState<Record<number, boolean>>({})
  const entries: SchemaEntry[] = schema ?? []
  // Raw-schema editor mode toggle. Default: typed-form (the simple
  // 80% case). Workflow author flips ON when they need nested
  // objects / arrays. Persisted as `input_schema_json != ""` on the
  // workflow record — toggle reflects "is the raw field non-empty".
  const [useRaw, setUseRaw] = useState(() => Boolean(rawSchema && rawSchema.trim()))
  // Local textarea state for the raw editor (same anti-cursor-jump
  // pattern as AsToolPanel — committing on every keystroke would
  // re-stringify and yank the caret to the end).
  const [rawText, setRawText] = useState(rawSchema ?? '')
  const lastSyncedRawRef = useRef(rawText)
  const [rawError, setRawError] = useState<string | null>(null)
  useEffect(() => {
    const next = rawSchema ?? ''
    if (next !== lastSyncedRawRef.current) {
      setRawText(next)
      lastSyncedRawRef.current = next
      setRawError(null)
    }
  }, [rawSchema])

  function setEntry(idx: number, patch: Partial<SchemaEntry>) {
    const next = entries.map((e, i) => (i === idx ? { ...e, ...patch } : e))
    onSchemaChange(next)
  }

  function removeEntry(idx: number) {
    onSchemaChange(entries.filter((_, i) => i !== idx))
  }

  function addEntry() {
    onSchemaChange([
      ...entries,
      { name: '', type: 'string' },
    ])
  }

  return (
    <div className="rounded-md border border-border bg-card/40">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-center justify-between px-3 py-2 text-sm font-medium hover:bg-accent/30 transition-colors"
      >
        <span className="flex items-center gap-2">
          {title}
          {entries.length > 0 && (
            <span className="text-xs text-muted-foreground">({entries.length})</span>
          )}
        </span>
        {open ? <ChevronUp className="h-3.5 w-3.5" /> : <ChevronDown className="h-3.5 w-3.5" />}
      </button>

      {open && (
        <div className="space-y-3 border-t border-border/40 px-3 py-3">
          <p className="text-[11px] text-muted-foreground leading-snug">
            {description}
          </p>

          <div className="flex items-center gap-2 text-xs">
            <Switch
              checked={useRaw}
              onCheckedChange={(v) => {
                setUseRaw(v)
                if (!v) {
                  // Turning OFF raw mode clears the stored raw schema
                  // so the typed editor's contract becomes
                  // authoritative again. Local text kept so the user
                  // can toggle back without retyping.
                  onRawSchemaChange('')
                }
              }}
            />
            <span className="text-muted-foreground">Raw JSON Schema (nested / arrays)</span>
          </div>

          {useRaw ? (
            <div className="space-y-1">
              <Textarea
                className="font-mono text-[10px] min-h-[140px] resize-y"
                rows={8}
                placeholder='{"type":"object","properties":{"user":{"type":"object","properties":{"name":{"type":"string"}}},"tags":{"type":"array","items":{"type":"string"}}},"required":["user"]}'
                value={rawText}
                onKeyDown={(e) => handleJsonTextareaTab(e, rawText, setRawText)}
                onChange={(e) => {
                  const v = e.target.value
                  setRawText(v)
                  if (v.trim() === '') {
                    setRawError(null)
                    lastSyncedRawRef.current = ''
                    onRawSchemaChange('')
                    return
                  }
                  try {
                    const parsed = JSON.parse(v)
                    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
                      setRawError(null)
                      lastSyncedRawRef.current = v
                      onRawSchemaChange(v)
                    } else {
                      setRawError('Schema must be a JSON object.')
                    }
                  } catch (err) {
                    setRawError((err as Error).message)
                    // Don't commit mid-typing invalid JSON; keep
                    // local state, surface the error inline.
                  }
                }}
              />
              {rawError && (
                <p className="text-[10px] text-red-500">{rawError}</p>
              )}
              <p className="text-[10px] text-muted-foreground/70">
                Wins over the typed editor for engine validation + sub_workflow auto-derive.
              </p>
            </div>
          ) : null}

          {!useRaw && (<>
          {entries.length === 0 && (
            <p className="text-xs text-muted-foreground italic">No input fields declared.</p>
          )}
          {entries.map((entry, idx) => {
            const settingsOpen = !!expandedSettings[idx]
            return (
              <div key={idx} className="space-y-2 rounded border border-border/60 bg-background/40 p-2">
                <div className="flex items-center gap-2">
                  <Input
                    className="h-7 text-xs flex-1"
                    placeholder="field name"
                    value={entry.name}
                    onChange={(e) => setEntry(idx, { name: e.target.value })}
                  />
                  <Select value={entry.type} onValueChange={(v) => setEntry(idx, { type: v as SchemaEntry['type'] })}>
                    <SelectTrigger className="h-7 w-24 text-xs">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {TYPES.map((t) => (
                        <SelectItem key={t} value={t}>
                          {t}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <Button
                    size="icon"
                    variant="ghost"
                    className="h-7 w-7"
                    onClick={() =>
                      setExpandedSettings((s) => ({ ...s, [idx]: !settingsOpen }))
                    }
                    title="More options"
                  >
                    <Settings2 className="h-3.5 w-3.5" />
                  </Button>
                  <Button
                    size="icon"
                    variant="ghost"
                    className="h-7 w-7 text-red-500 hover:text-red-400"
                    onClick={() => removeEntry(idx)}
                    title="Remove field"
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                  </Button>
                </div>
                {settingsOpen && (
                  <div className="space-y-2 pl-1">
                    <Input
                      className="h-7 text-xs"
                      placeholder="description (helps consumers + UI tooltips)"
                      value={entry.description ?? ''}
                      onChange={(e) => setEntry(idx, { description: e.target.value })}
                    />
                    <div className="flex items-center gap-2 text-xs">
                      <Switch
                        checked={!!entry.required}
                        onCheckedChange={(v) => setEntry(idx, { required: v })}
                      />
                      <span className="text-muted-foreground">Required</span>
                    </div>
                    {entry.type === 'enum' && (
                      <Input
                        className="h-7 text-xs"
                        placeholder="enum values (comma-separated)"
                        value={(entry.enum ?? []).join(', ')}
                        onChange={(e) =>
                          setEntry(idx, {
                            enum: e.target.value
                              .split(',')
                              .map((s) => s.trim())
                              .filter(Boolean),
                          })
                        }
                      />
                    )}
                  </div>
                )}
              </div>
            )
          })}
          <Button size="sm" variant="outline" className="h-7 text-xs" onClick={addEntry}>
            <Plus className="h-3 w-3 mr-1" />
            Add field
          </Button>
          </>)}
        </div>
      )}
    </div>
  )
}

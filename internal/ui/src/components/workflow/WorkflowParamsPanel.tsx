import { useEffect, useMemo, useState } from 'react'
import { ChevronDown, ChevronUp, Plus, Save, Settings2, Trash2 } from 'lucide-react'
import { Button } from '~/components/ui/button'
import { Input } from '~/components/ui/input'
import { NumberField } from '~/components/ui/number-field'
import { Switch } from '~/components/ui/switch'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '~/components/ui/select'
import type { ParamEntry } from './useWorkflowStore'

interface Props {
  params: Record<string, string>
  onChange(params: Record<string, string>): void
  // Persists the workflow doc (params + params_schema ride along on the
  // existing PUT payload). Same handler the toolbar Save button uses.
  onSave(): void
  // Optional typed declaration. When undefined we synthesise a schema
  // from existing param keys (all string, none required) so legacy
  // workflows render with the same unified UI without losing values.
  schema?: ParamEntry[]
  onSchemaChange(schema: ParamEntry[]): void
}

// effectiveSchema returns the schema we render against. When the
// workflow has no explicit schema, we derive one from the existing
// params keys (all string) so legacy workflows still get the unified
// per-row layout without forcing the author to re-declare everything.
// Caller treats the returned schema as authoritative for which params
// exist and in what order; the underlying `params` dict is the value
// store keyed by entry.name.
function effectiveSchema(schema: ParamEntry[] | undefined, params: Record<string, string>): ParamEntry[] {
  if (schema && schema.length > 0) return schema
  const out: ParamEntry[] = []
  for (const k of Object.keys(params)) {
    out.push({ name: k, type: 'string' })
  }
  return out
}

export function WorkflowParamsPanel({ params, onChange, onSave, schema, onSchemaChange }: Props) {
  const [open, setOpen] = useState(false)
  const [expandedSettings, setExpandedSettings] = useState<Record<number, boolean>>({})

  // The visible schema. Either the persisted params_schema, or a synth
  // one derived from existing params keys. Persistence-wise we only
  // call onSchemaChange when the author actually edits a schema field;
  // a workflow that never touched schema fields keeps params_schema
  // empty/undefined and the legacy free-form back-end shape.
  const visibleSchema = useMemo(
    () => effectiveSchema(schema, params),
    [schema, params],
  )
  const hasExplicitSchema = !!schema && schema.length > 0

  // Mutate schema (always promotes to explicit). Caller passes a
  // function that returns the next schema given the current visible one.
  function updateSchema(mutate: (cur: ParamEntry[]) => ParamEntry[]) {
    onSchemaChange(mutate(visibleSchema))
  }

  function setEntryValue(name: string, value: string) {
    onChange({ ...params, [name]: value })
  }

  function setEntry(idx: number, patch: Partial<ParamEntry>) {
    const prev = visibleSchema[idx]!
    updateSchema((cur) => {
      const next = [...cur]
      next[idx] = { ...prev, ...patch }
      return next
    })
    // Rename: carry the value over so the user doesn't lose what they
    // typed when they tweak the name.
    if (patch.name !== undefined && patch.name !== prev.name) {
      const oldName = prev.name
      const newName = patch.name
      if (oldName in params) {
        const nextParams: Record<string, string> = {}
        for (const [k, v] of Object.entries(params)) {
          nextParams[k === oldName ? newName : k] = v
        }
        onChange(nextParams)
      }
    }
  }

  function removeEntry(idx: number) {
    const removed = visibleSchema[idx]
    updateSchema((cur) => cur.filter((_, i) => i !== idx))
    if (removed) {
      const nextParams = { ...params }
      delete nextParams[removed.name]
      onChange(nextParams)
    }
  }

  function addEntry() {
    let name = 'param'
    let i = 1
    const used = new Set(visibleSchema.map((p) => p.name))
    while (used.has(name)) name = `param${i++}`
    updateSchema((cur) => [...cur, { name, type: 'string' }])
  }

  function toggleSettings(idx: number) {
    setExpandedSettings((p) => ({ ...p, [idx]: !p[idx] }))
  }

  return (
    <div
      className="rounded-lg border bg-card text-card-foreground shadow-md overflow-hidden"
      style={{ resize: open ? 'horizontal' : 'none', minWidth: 280, maxWidth: 720, width: 360 }}
    >
      <button
        className="flex items-center justify-between w-full px-3 py-2 text-xs font-medium hover:bg-muted/50 transition-colors"
        onClick={() => setOpen((v) => !v)}
      >
        <span>
          Parameters ({visibleSchema.length})
          {hasExplicitSchema && <span className="ml-1.5 text-[9px] text-muted-foreground">typed</span>}
        </span>
        {open ? <ChevronUp className="h-3.5 w-3.5" /> : <ChevronDown className="h-3.5 w-3.5" />}
      </button>

      {open && (
        <div className="border-t px-3 py-2 space-y-2">
          <p className="text-[10px] text-muted-foreground">
            Fields: <code className="text-[10px]">{'{{params.key}}'}</code> · JS: <code className="text-[10px]">params.key</code>
          </p>

          {visibleSchema.length === 0 && (
            <p className="text-[10px] text-muted-foreground italic">No parameters yet</p>
          )}

          {visibleSchema.map((entry, idx) => {
            const value = params[entry.name] ?? ''
            const settingsOpen = !!expandedSettings[idx]
            return (
              <div key={idx} className="rounded border border-border/40 bg-muted/10 p-1.5 space-y-1.5">
                {/* Top row: name, value, settings toggle, trash. Type
                    lives in the per-row settings (rarely changes after
                    initial declaration; the value widget shape already
                    reflects the chosen type so the select itself is
                    secondary detail). */}
                <div className="flex items-center gap-1 h-7">
                  <Input
                    className="h-7 text-[11px] w-[110px] shrink-0 px-2"
                    placeholder="key"
                    value={entry.name}
                    onChange={(e) => setEntry(idx, { name: e.target.value })}
                  />
                  <div className="flex-1 min-w-0 h-7 flex items-center">
                    <ValueWidget entry={entry} value={value} setValue={(v) => setEntryValue(entry.name, v)} />
                  </div>
                  <button
                    className={`shrink-0 h-7 w-6 flex items-center justify-center transition-colors ${settingsOpen ? 'text-foreground' : 'text-muted-foreground hover:text-foreground'}`}
                    onClick={() => toggleSettings(idx)}
                    title="Per-parameter settings"
                  >
                    <Settings2 className="h-3.5 w-3.5" />
                  </button>
                  <button
                    className="shrink-0 h-7 w-6 flex items-center justify-center text-muted-foreground hover:text-destructive transition-colors"
                    onClick={() => removeEntry(idx)}
                    title="Remove parameter"
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                  </button>
                </div>

                {/* Required indicator without expanding settings. */}
                {entry.required && !settingsOpen && (
                  <span className="text-[9px] text-red-500">required</span>
                )}

                {/* Per-parameter settings — description, default, required,
                    enum opts. Span full row width (no indent) so the
                    inputs feel like a continuation of the row rather
                    than a side note pushed to one column. */}
                {settingsOpen && (
                  <div className="border-t border-border/40 pt-1.5 space-y-1.5">
                    <Select
                      value={entry.type}
                      onValueChange={(v) => setEntry(idx, { type: v as ParamEntry['type'] })}
                    >
                      <SelectTrigger className="h-7 text-[11px] w-full">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent className="z-[9999]">
                        <SelectItem value="string">string</SelectItem>
                        <SelectItem value="number">number</SelectItem>
                        <SelectItem value="boolean">boolean</SelectItem>
                        <SelectItem value="enum">enum</SelectItem>
                      </SelectContent>
                    </Select>
                    <Input
                      className="h-7 text-[11px] w-full"
                      placeholder="description (optional)"
                      value={entry.description ?? ''}
                      onChange={(e) => setEntry(idx, { description: e.target.value })}
                    />
                    <div className="flex items-center gap-2 h-7">
                      <Input
                        className="h-7 text-[11px] flex-1"
                        placeholder="default (optional)"
                        value={entry.default ?? ''}
                        onChange={(e) => setEntry(idx, { default: e.target.value })}
                      />
                      <label className="flex items-center gap-1 text-[10px] shrink-0 h-7">
                        <Switch
                          checked={!!entry.required}
                          onCheckedChange={(v) => setEntry(idx, { required: v })}
                        />
                        required
                      </label>
                    </div>
                    {entry.type === 'enum' && (
                      <EnumOptsInput
                        value={entry.enum ?? []}
                        onChange={(opts) => setEntry(idx, { enum: opts })}
                      />
                    )}
                  </div>
                )}
              </div>
            )
          })}

          <button
            className="flex items-center gap-1 text-[10px] text-muted-foreground hover:text-foreground transition-colors"
            onClick={addEntry}
          >
            <Plus className="h-3 w-3" />
            Add parameter
          </button>

          <Button
            variant="secondary"
            size="sm"
            className="w-full h-7 text-[11px]"
            onClick={onSave}
          >
            <Save className="h-3 w-3 mr-1" />
            Save parameters
          </Button>
        </div>
      )}
    </div>
  )
}

// EnumOptsInput holds a local string buffer so the user can type
// commas + partial values without each keystroke being reparsed (which
// would strip the trailing comma + prevent typing it at all). Splits
// to options only on blur. Syncs from props when the upstream value
// changes (e.g. switching rows or remote workflow load).
function EnumOptsInput({ value, onChange }: { value: string[]; onChange(v: string[]): void }) {
  const joined = value.join(', ')
  const [buf, setBuf] = useState(joined)
  // Resync from parent only when the parent's joined string differs
  // from what the user's buffer would commit. Without this guard we'd
  // clobber every keystroke since parent emits a new array.
  useEffect(() => {
    const committed = buf
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean)
      .join(', ')
    if (joined !== committed) {
      setBuf(joined)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [joined])
  return (
    <Input
      className="h-7 text-[11px] w-full"
      placeholder="enum options (comma-separated)"
      value={buf}
      onChange={(e) => setBuf(e.target.value)}
      onBlur={() => {
        const opts = buf
          .split(',')
          .map((s) => s.trim())
          .filter(Boolean)
        onChange(opts)
      }}
    />
  )
}

// ValueWidget renders the type-appropriate value editor inline with the
// row. Kept tight (h-6) so the row stays scannable; description /
// default / enum options live under the per-row settings expander.
function ValueWidget({
  entry,
  value,
  setValue,
}: {
  entry: ParamEntry
  value: string
  setValue(v: string): void
}) {
  if (entry.type === 'boolean') {
    return (
      <div className="flex items-center gap-2 h-7 w-full px-1">
        <Switch
          checked={value === 'true'}
          onCheckedChange={(v) => setValue(v ? 'true' : 'false')}
        />
        <span className="text-[10px] text-muted-foreground">{value === 'true' ? 'true' : 'false'}</span>
      </div>
    )
  }
  if (entry.type === 'enum') {
    return (
      <Select
        value={value || '__none__'}
        onValueChange={(v) => setValue(v === '__none__' ? '' : v)}
      >
        <SelectTrigger className="h-7 text-[11px] w-full">
          <SelectValue placeholder="select" />
        </SelectTrigger>
        <SelectContent className="z-[9999]">
          {!entry.required && <SelectItem value="__none__">— none —</SelectItem>}
          {(entry.enum ?? []).map((opt) => (
            <SelectItem key={opt} value={opt}>{opt}</SelectItem>
          ))}
        </SelectContent>
      </Select>
    )
  }
  if (entry.type === 'number') {
    return (
      <NumberField
        className="h-7 text-[11px] w-full"
        value={value === '' ? 0 : Number(value)}
        onChange={(v) => setValue(String(v))}
      />
    )
  }
  return (
    <Input
      className="h-7 text-[11px] w-full"
      placeholder={entry.default ?? 'value'}
      value={value}
      onChange={(e) => setValue(e.target.value)}
    />
  )
}

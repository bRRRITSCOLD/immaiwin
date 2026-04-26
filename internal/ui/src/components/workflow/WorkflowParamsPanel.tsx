import { useState } from 'react'
import { ChevronDown, ChevronUp, Plus, Trash2 } from 'lucide-react'
import { Input } from '~/components/ui/input'

interface Props {
  params: Record<string, string>
  onChange(params: Record<string, string>): void
}

export function WorkflowParamsPanel({ params, onChange }: Props) {
  const [open, setOpen] = useState(false)
  const entries = Object.entries(params)

  function setValue(key: string, value: string) {
    onChange({ ...params, [key]: value })
  }

  function renameKey(oldKey: string, newKey: string) {
    if (newKey === oldKey || newKey === '') return
    const next: Record<string, string> = {}
    for (const [k, v] of Object.entries(params)) {
      next[k === oldKey ? newKey : k] = v
    }
    onChange(next)
  }

  function remove(key: string) {
    const next = { ...params }
    delete next[key]
    onChange(next)
  }

  function addRow() {
    let key = 'param'
    let i = 1
    while (key in params) {
      key = `param${i++}`
    }
    onChange({ ...params, [key]: '' })
  }

  return (
    <div
      className="rounded-lg border bg-card text-card-foreground shadow-md overflow-hidden"
      style={{ resize: open ? 'horizontal' : 'none', minWidth: 260, maxWidth: 700, width: 300 }}
    >
      <button
        className="flex items-center justify-between w-full px-3 py-2 text-xs font-medium hover:bg-muted/50 transition-colors"
        onClick={() => setOpen((v) => !v)}
      >
        <span>Parameters ({entries.length})</span>
        {open ? <ChevronUp className="h-3.5 w-3.5" /> : <ChevronDown className="h-3.5 w-3.5" />}
      </button>

      {open && (
        <div className="border-t px-3 py-2 space-y-2">
          <p className="text-[10px] text-muted-foreground">
            Fields: <code className="text-[10px]">{'{{params.key}}'}</code> · JS: <code className="text-[10px]">params.key</code>
          </p>

          {entries.length === 0 && (
            <p className="text-[10px] text-muted-foreground italic">No parameters yet</p>
          )}

          {entries.map(([key, value]) => (
            <div key={key} className="flex items-start gap-1.5">
              <Input
                className="h-6 text-[11px] w-[90px] shrink-0 px-2 mt-0.5"
                placeholder="key"
                defaultValue={key}
                onBlur={(e) => renameKey(key, e.target.value)}
              />
              <textarea
                className="flex-1 min-h-[28px] px-2 py-1 text-[11px] rounded-md border border-input bg-transparent
                           text-foreground placeholder:text-muted-foreground focus:outline-none focus:ring-1
                           focus:ring-ring resize-y break-all leading-4"
                placeholder="value"
                value={value}
                rows={1}
                onChange={(e) => setValue(key, e.target.value)}
              />
              <button
                className="text-muted-foreground hover:text-destructive transition-colors shrink-0 mt-1"
                onClick={() => remove(key)}
              >
                <Trash2 className="h-3.5 w-3.5" />
              </button>
            </div>
          ))}

          <button
            className="flex items-center gap-1 text-[10px] text-muted-foreground hover:text-foreground transition-colors"
            onClick={addRow}
          >
            <Plus className="h-3 w-3" />
            Add parameter
          </button>
        </div>
      )}
    </div>
  )
}

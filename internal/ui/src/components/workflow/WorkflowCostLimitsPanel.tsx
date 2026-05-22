// WorkflowCostLimitsPanel — collapsible per-workflow USD caps editor.
//
// Wires `Workflow.cost_limits.{max_run_usd,max_daily_usd}` (see Go
// `workflow.CostLimits`). Zero on either field means "no cap on that
// axis"; the executor's pre-run + agent-loop guards short-circuit when
// the cap is 0.
//
// Save lives on the existing toolbar Save button — we mutate the active
// workflow doc via `updateActiveCostLimits`, then the workflows.tsx
// `handleSave` PUTs the whole workflow (cost_limits ride along via the
// `{ ...wf, nodes, edges, config }` spread).

import { useState } from 'react'
import { ChevronDown, ChevronUp, DollarSign, Save } from 'lucide-react'
import { Button } from '~/components/ui/button'
import { NumberField } from '~/components/ui/number-field'
import type { CostLimits } from './useWorkflowStore'

interface Props {
  costLimits: CostLimits | null | undefined
  onChange(limits: CostLimits | null): void
  // Persists the whole workflow (cost_limits ride along on the existing
  // PUT /api/v1/workflows/:id payload). Wired to the same handleSave the
  // toolbar Save button uses, so this button is a convenience for users
  // who don't realize the toolbar Save covers cost limits too.
  onSave(): void
}

export function WorkflowCostLimitsPanel({ costLimits, onChange, onSave }: Props) {
  const [open, setOpen] = useState(false)
  const runCap = costLimits?.max_run_usd ?? 0
  const dailyCap = costLimits?.max_daily_usd ?? 0
  const hasAnyCap = runCap > 0 || dailyCap > 0

  function set(field: keyof CostLimits, value: number) {
    const next: CostLimits = {
      max_run_usd: costLimits?.max_run_usd ?? 0,
      max_daily_usd: costLimits?.max_daily_usd ?? 0,
      [field]: value,
    }
    // Persist as null when both fields are 0 so the BSON doc stays slim.
    if (next.max_run_usd === 0 && next.max_daily_usd === 0) {
      onChange(null)
    } else {
      onChange(next)
    }
  }

  return (
    <div
      className="rounded-lg border bg-card text-card-foreground shadow-md overflow-hidden"
      style={{ minWidth: 260, maxWidth: 700, width: 300 }}
    >
      <button
        className="flex items-center justify-between w-full px-3 py-2 text-xs font-medium hover:bg-muted/50 transition-colors"
        onClick={() => setOpen((v) => !v)}
      >
        <span className="flex items-center gap-1.5">
          <DollarSign className="h-3.5 w-3.5" />
          Cost limits {hasAnyCap && <span className="text-amber-600 dark:text-amber-400">·</span>}
        </span>
        {open ? <ChevronUp className="h-3.5 w-3.5" /> : <ChevronDown className="h-3.5 w-3.5" />}
      </button>

      {open && (
        <div className="border-t px-3 py-2 space-y-2">
          <p className="text-[10px] text-muted-foreground">
            Caps in USD. <code className="text-[10px]">0</code> = unlimited. Per-run aborts
            mid-loop; daily blocks new starts and aborts mid-run.
          </p>

          <label className="flex items-center justify-between gap-2 text-[11px]">
            <span className="shrink-0 w-[90px]">Max per run</span>
            <NumberField
              step="0.01"
              min="0"
              className="h-6 text-[11px] flex-1 px-2"
              value={runCap}
              placeholder="0"
              onChange={(v) => set('max_run_usd', v)}
            />
          </label>

          <label className="flex items-center justify-between gap-2 text-[11px]">
            <span className="shrink-0 w-[90px]">Max per day</span>
            <NumberField
              step="0.01"
              min="0"
              className="h-6 text-[11px] flex-1 px-2"
              value={dailyCap}
              placeholder="0"
              onChange={(v) => set('max_daily_usd', v)}
            />
          </label>

          <Button
            variant="secondary"
            size="sm"
            className="w-full h-7 text-[11px]"
            onClick={onSave}
          >
            <Save className="h-3 w-3 mr-1" />
            Save cost limits
          </Button>
        </div>
      )}
    </div>
  )
}

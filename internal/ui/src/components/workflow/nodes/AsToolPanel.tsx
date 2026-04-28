import { useReactFlow } from '@xyflow/react'
import { useState } from 'react'
import { Wrench, ChevronDown, ChevronRight } from 'lucide-react'
import { Textarea } from '~/components/ui/textarea'
import { Input } from '~/components/ui/input'

interface Props {
  nodeId: string
  data: Record<string, unknown>
  defaultName?: string
  defaultSchema?: Record<string, unknown>
}

interface AsToolData {
  enabled?: boolean
  name?: string
  description?: string
  input_schema?: Record<string, unknown>
}

const DEFAULT_SCHEMA: Record<string, unknown> = { type: 'object', properties: {} }

/**
 * Collapsible "Expose as Tool" panel mounted on existing node components.
 * Lets a workflow author opt-in a node as a callable tool for AI Agent
 * nodes connected via the "tool" edge. Server reads data.as_tool when
 * building the agent's tool catalog.
 */
export function AsToolPanel({ nodeId, data, defaultName, defaultSchema }: Props) {
  const { updateNodeData } = useReactFlow()
  const asTool = (data?.as_tool as AsToolData | undefined) ?? {}
  const [open, setOpen] = useState(Boolean(asTool.enabled))

  function update(patch: Partial<AsToolData>) {
    updateNodeData(nodeId, {
      as_tool: { ...asTool, ...patch },
    })
  }

  const enabled = Boolean(asTool.enabled)
  const name = asTool.name ?? ''
  const description = asTool.description ?? ''
  const schemaText = asTool.input_schema
    ? JSON.stringify(asTool.input_schema, null, 2)
    : JSON.stringify(defaultSchema ?? DEFAULT_SCHEMA, null, 2)

  return (
    <div className="border-t border-border/50">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="nodrag flex w-full items-center justify-between gap-2 px-3 py-1.5 text-[10px] font-medium text-muted-foreground hover:text-foreground"
      >
        <span className="flex items-center gap-1.5">
          <Wrench className={`h-3 w-3 ${enabled ? 'text-purple-400' : ''}`} />
          Expose as Tool
          {enabled && <span className="text-purple-400">●</span>}
        </span>
        {open ? <ChevronDown className="h-3 w-3" /> : <ChevronRight className="h-3 w-3" />}
      </button>

      {open && (
        <div className="space-y-2 px-3 pb-2">
          <label className="nodrag flex items-center gap-1.5 text-[10px]">
            <input
              type="checkbox"
              checked={enabled}
              onChange={(e) => update({ enabled: e.target.checked })}
            />
            Enable as agent tool
          </label>

          {enabled && (
            <>
              <div>
                <p className="text-[10px] text-muted-foreground">Tool name (LLM-facing)</p>
                <Input
                  className="nodrag h-7 text-xs"
                  placeholder={defaultName ?? 'fetch_weather'}
                  value={name}
                  onChange={(e) => update({ name: e.target.value })}
                />
              </div>

              <div>
                <p className="text-[10px] text-muted-foreground">Description (helps the LLM choose this tool)</p>
                <Textarea
                  className="nodrag text-xs min-h-[40px] resize-y"
                  rows={2}
                  placeholder="Fetch the 7-day weather forecast for a US ZIP code."
                  value={description}
                  onChange={(e) => update({ description: e.target.value })}
                />
              </div>

              <div>
                <p className="text-[10px] text-muted-foreground">Input schema (JSON Schema)</p>
                <Textarea
                  className="nodrag font-mono text-[10px] min-h-[80px] resize-y"
                  rows={5}
                  value={schemaText}
                  onChange={(e) => {
                    try {
                      const parsed = JSON.parse(e.target.value)
                      update({ input_schema: parsed })
                    } catch {
                      // ignore — keep last valid value
                    }
                  }}
                />
              </div>
            </>
          )}
        </div>
      )}
    </div>
  )
}

import { useReactFlow } from '@xyflow/react'
import { useEffect, useRef, useState } from 'react'
import { Wrench, ChevronDown, ChevronRight } from 'lucide-react'
import { Textarea } from '~/components/ui/textarea'
import { Input } from '~/components/ui/input'
import { handleJsonTextareaTab } from './jsonTextareaTab'

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

// Stable JSON of the store value so the effect's "did the upstream
// schema change?" check uses string equality, not object-identity
// (React Flow rebuilds the data object on every render).
function stringifySchema(s: Record<string, unknown> | undefined, fallback: Record<string, unknown>): string {
  return JSON.stringify(s ?? fallback, null, 2)
}

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

  // Local textarea state — committing on every keystroke via the
  // round-trip JSON.parse → JSON.stringify path was destroying the
  // caret position (each render replaced `value` with re-formatted
  // text, jumping the cursor to the end). Now: typing only mutates
  // local state. On parse-success the store gets the new schema;
  // mid-typing partial JSON is held locally without spamming
  // updateNodeData. External changes (loading a saved workflow)
  // resync via the effect below.
  const fallbackSchema = defaultSchema ?? DEFAULT_SCHEMA
  const [schemaText, setSchemaText] = useState(() => stringifySchema(asTool.input_schema, fallbackSchema))
  const lastSyncedRef = useRef(schemaText)

  useEffect(() => {
    const next = stringifySchema(asTool.input_schema, fallbackSchema)
    // Only adopt upstream changes when they differ from what we last
    // committed — otherwise our own update would loop back and clobber
    // mid-edit local state. eslint disable: fallbackSchema/asTool are
    // stable enough for this comparison via the stringified result.
    if (next !== lastSyncedRef.current) {
      setSchemaText(next)
      lastSyncedRef.current = next
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [JSON.stringify(asTool.input_schema)])

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
                  onKeyDown={(e) => handleJsonTextareaTab(e, schemaText, setSchemaText)}
                  onChange={(e) => {
                    const v = e.target.value
                    setSchemaText(v)
                    try {
                      const parsed = JSON.parse(v)
                      lastSyncedRef.current = stringifySchema(parsed, fallbackSchema)
                      update({ input_schema: parsed })
                    } catch {
                      // Partial / invalid JSON mid-typing — keep
                      // local state, leave store untouched until the
                      // next valid parse.
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

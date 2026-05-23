import { useEffect, useRef, useState } from 'react'
import { useReactFlow } from '@xyflow/react'
import { ChevronDown, ChevronRight, Shuffle } from 'lucide-react'
import { Textarea } from '~/components/ui/textarea'
import { handleJsonTextareaTab } from './jsonTextareaTab'

interface Props {
  nodeId: string
  data: Record<string, unknown> | undefined
}

/**
 * OutputTransformPanel — collapsible editor for `data.output_transform`,
 * mounted on every actionable node. Template is JSON-shaped and
 * resolved at runtime against the node's RAW output as
 * `{{input.<field>}}` (the node IS the producer here), plus the
 * standard `{{context.X}}` / `{{config.X}}` / `{{run_input.X}}`
 * namespaces.
 *
 * Empty = no reshape; downstream sees raw output. Raw always lands on
 * StepResult.Output so debug surfaces aren't lossy; transformed is on
 * StepResult.TransformedOutput and flows to the next node.
 *
 * Author opens via the chevron — collapsed by default so the common
 * "no transform needed" case keeps node cards short.
 */
export function OutputTransformPanel({ nodeId, data }: Props) {
  const { updateNodeData } = useReactFlow()
  const value = data?.output_transform
  const initialText =
    value === undefined || value === null
      ? ''
      : typeof value === 'string'
        ? value
        : JSON.stringify(value, null, 2)

  const [text, setText] = useState(initialText)
  const lastSyncedRef = useRef(initialText)
  const [parseError, setParseError] = useState<string | null>(null)
  const [open, setOpen] = useState(!!initialText)

  useEffect(() => {
    const next =
      value === undefined || value === null
        ? ''
        : typeof value === 'string'
          ? value
          : JSON.stringify(value, null, 2)
    if (next !== lastSyncedRef.current) {
      setText(next)
      lastSyncedRef.current = next
      setParseError(null)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [JSON.stringify(value)])

  const isSet = text.trim().length > 0

  return (
    <div className="nodrag px-3 py-2 border-t border-border/40">
      <button
        type="button"
        className="flex items-center gap-1.5 text-[10px] text-muted-foreground hover:text-foreground transition-colors w-full"
        onClick={() => setOpen((s) => !s)}
      >
        {open ? <ChevronDown className="h-3 w-3" /> : <ChevronRight className="h-3 w-3" />}
        <Shuffle className={`h-3 w-3 ${isSet ? 'text-violet-400' : ''}`} />
        <span>Output transform{isSet ? ' (active)' : ''}</span>
      </button>
      {open && (
        <div className="mt-1.5 space-y-1">
          <Textarea
            className="font-mono text-[10px] min-h-[80px] resize-y"
            rows={4}
            placeholder='{ "id": "{{input.id}}", "name": "{{input.name}}" }'
            value={text}
            onKeyDown={(e) => handleJsonTextareaTab(e, text, setText)}
            onChange={(e) => {
              const v = e.target.value
              setText(v)
              if (v.trim() === '') {
                setParseError(null)
                lastSyncedRef.current = ''
                updateNodeData(nodeId, { output_transform: null })
                return
              }
              try {
                const parsed = JSON.parse(v)
                setParseError(null)
                lastSyncedRef.current = v
                updateNodeData(nodeId, { output_transform: parsed })
              } catch (err) {
                setParseError((err as Error).message)
              }
            }}
          />
          {parseError && <p className="text-[10px] text-red-500">{parseError}</p>}
          <p className="text-[10px] text-muted-foreground/70">
            Reshapes this node's response before downstream nodes / consuming agents see it. Raw output stays in the debug panel.
          </p>
        </div>
      )}
    </div>
  )
}

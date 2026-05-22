import { NodeResizer, type NodeProps, useReactFlow } from '@xyflow/react'
import { Shuffle } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { Textarea } from '~/components/ui/textarea'
import { StepNameInput } from './StepNameInput'
import { DynamicHandles } from './DynamicHandles'
import { NodeDebugPanel, BreakpointMarker } from '../RunResultsContext'
import { handleJsonTextareaTab } from './jsonTextareaTab'

/**
 * TransformNode reshapes its upstream input via a JSON-shaped
 * `payload` template. Same template engine as ReturnNode —
 * `{{context.<step>.output.<key>}}`, `{{input.<key>}}`,
 * `{{run_input.<key>}}`, `{{config.<key>}}` — but mid-graph rather
 * than a workflow-level return contract.
 *
 * Typical use case: trim or rename fields from an upstream output
 * (mongo find / http body / agent answer) before it feeds into a
 * downstream agent or sandbox, cutting token spend without spinning
 * up a sandbox container. Pure-template: no expression language, no
 * array mapping. For array projection use `for_each`; for arbitrary
 * computation use `sandbox_script`.
 */
export function TransformNode({ id, data, selected }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const payloadValue = data?.payload
  const initialText =
    payloadValue === undefined || payloadValue === null
      ? ''
      : typeof payloadValue === 'string'
        ? payloadValue
        : JSON.stringify(payloadValue, null, 2)

  const [payloadText, setPayloadText] = useState(initialText)
  const lastSyncedRef = useRef(initialText)
  const [parseError, setParseError] = useState<string | null>(null)

  useEffect(() => {
    const next =
      payloadValue === undefined || payloadValue === null
        ? ''
        : typeof payloadValue === 'string'
          ? payloadValue
          : JSON.stringify(payloadValue, null, 2)
    if (next !== lastSyncedRef.current) {
      setPayloadText(next)
      lastSyncedRef.current = next
      setParseError(null)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [JSON.stringify(payloadValue)])

  return (
    <div className="relative min-w-[280px] h-full">
      <BreakpointMarker id={id} />
      <div className="overflow-x-hidden rounded-lg border-2 border-violet-500 bg-card text-card-foreground shadow-sm h-full">
        <NodeResizer minWidth={280} minHeight={140} isVisible={selected} />
        <div className="flex items-center gap-2 px-4 py-2.5 border-b border-violet-500/40">
          <Shuffle className="h-4 w-4 text-violet-400 shrink-0" />
          <span className="text-sm font-medium">Transform</span>
        </div>
        <StepNameInput id={id} data={data} />
        <div className="px-3 py-2">
          <p className="text-[10px] text-muted-foreground mb-1">
            Payload (JSON, template-resolved at runtime). Empty = pass through upstream input.
          </p>
          <Textarea
            className="nodrag font-mono text-[10px] min-h-[120px] resize-y"
            rows={6}
            placeholder='{ "id": "{{context.find.output.docs[0].id}}", "name": "{{context.find.output.docs[0].name}}" }'
            value={payloadText}
            onKeyDown={(e) => handleJsonTextareaTab(e, payloadText, setPayloadText)}
            onChange={(e) => {
              const v = e.target.value
              setPayloadText(v)
              if (v.trim() === '') {
                setParseError(null)
                lastSyncedRef.current = ''
                updateNodeData(id, { payload: null })
                return
              }
              try {
                const parsed = JSON.parse(v)
                setParseError(null)
                lastSyncedRef.current = v
                updateNodeData(id, { payload: parsed })
              } catch (err) {
                setParseError((err as Error).message)
              }
            }}
          />
          {parseError && (
            <p className="mt-1 text-[10px] text-red-500">{parseError}</p>
          )}
          <p className="mt-1 text-[10px] text-muted-foreground/70">
            Reshapes input for the next node. Use to trim large payloads before an agent / sandbox.
          </p>
        </div>
        <NodeDebugPanel id={id} />
      </div>
      <DynamicHandles nodeId={id} nodeType="transform" data={data as Record<string, unknown>} />
    </div>
  )
}

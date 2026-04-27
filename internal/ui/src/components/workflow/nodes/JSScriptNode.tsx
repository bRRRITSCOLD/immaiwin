import { NodeResizer, type NodeProps, useReactFlow } from '@xyflow/react'
import { Code2, Maximize2 } from 'lucide-react'
import { useState } from 'react'
import { ScriptEditor } from '~/components/ScriptEditor'
import { StepNameInput } from './StepNameInput'
import { DynamicHandles } from './DynamicHandles'
import { NodeDebugPanel, BreakpointMarker } from '../RunResultsContext'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from '~/components/ui/dialog'

export function JSScriptNode({ id, data, selected }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const script = (data?.script as string) ?? ''
  const [open, setOpen] = useState(false)

  return (
    <div className="relative min-w-[280px] h-full">
      <BreakpointMarker id={id} />
      <div className="overflow-x-hidden rounded-lg border-2 border-yellow-400 bg-card text-card-foreground shadow-sm h-full">
        <NodeResizer minWidth={280} minHeight={80} isVisible={selected} />
        <div className="flex items-center gap-2 px-4 py-2.5 border-b border-yellow-400/40">
          <Code2 className="h-4 w-4 text-yellow-400 shrink-0" />
          <span className="text-sm font-medium flex-1">JS Script</span>
          <button
            className="nodrag text-muted-foreground hover:text-foreground transition-colors"
            onClick={() => setOpen(true)}
          >
            <Maximize2 className="h-3.5 w-3.5" />
          </button>
        </div>

        <StepNameInput id={id} data={data} />

        <div className="px-3 py-2">
          <p className="text-[10px] text-muted-foreground mb-1">
            Script — <code className="text-[10px]">input</code> · <code className="text-[10px]">context</code> · <code className="text-[10px]">params</code> — see legend ↙
          </p>
          <pre
            className="nodrag text-xs text-muted-foreground max-h-[72px] overflow-hidden leading-5 whitespace-pre-wrap cursor-pointer rounded bg-muted/30 px-2 py-1.5"
            onClick={() => setOpen(true)}
          >
            {script || <span className="italic opacity-60">click to edit script</span>}
          </pre>
        </div>

        <NodeDebugPanel id={id} />
      </div>

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="max-w-5xl sm:max-w-5xl w-[95vw] h-[75vh] flex flex-col">
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2">
              <Code2 className="h-4 w-4 text-yellow-400" />
              JS Script {data?.name ? `— ${data.name}` : ''}
            </DialogTitle>
          </DialogHeader>
          <p className="text-xs text-muted-foreground">
            Globals: <code className="text-xs">input</code> · <code className="text-xs">context</code> · <code className="text-xs">params</code>
          </p>
          <div className="flex-1 min-h-0">
            <ScriptEditor
              height="100%"
              language="javascript"
              value={script}
              onChange={(v) => updateNodeData(id, { script: v })}
              options={{ fontSize: 14 }}
            />
          </div>
        </DialogContent>
      </Dialog>
      <DynamicHandles nodeId={id} nodeType="js_script" data={data as Record<string, unknown>} />
    </div>
  )
}

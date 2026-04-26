import { Handle, Position, type NodeProps, useReactFlow } from '@xyflow/react'
import { Code2, ChevronDown, ChevronUp } from 'lucide-react'
import { useState } from 'react'
import Editor from '@monaco-editor/react'
import { StepNameInput } from './StepNameInput'
import { NodeDebugPanel } from '../RunResultsContext'

export function JSTransformNode({ id, data }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const script = (data?.script as string) ?? ''
  const [expanded, setExpanded] = useState(false)

  return (
    <div className={`rounded-lg border bg-card text-card-foreground shadow-sm ${expanded ? 'w-[480px]' : 'w-[280px]'}`}>
      <div className="flex items-center gap-2 px-4 py-2.5 border-b">
        <Code2 className="h-4 w-4 text-yellow-400 shrink-0" />
        <span className="text-sm font-medium flex-1">JS Transform</span>
        <button
          className="nodrag text-muted-foreground hover:text-foreground transition-colors"
          onClick={() => setExpanded((v) => !v)}
        >
          {expanded ? <ChevronUp className="h-3.5 w-3.5" /> : <ChevronDown className="h-3.5 w-3.5" />}
        </button>
      </div>

      <StepNameInput id={id} data={data} />

      <div className="px-3 py-2">
        <p className="text-[10px] text-muted-foreground mb-1">
          Script — <code className="text-[10px]">input</code> · <code className="text-[10px]">context</code> · <code className="text-[10px]">params</code> — see legend ↙
        </p>
        <div className="nodrag nowheel" onKeyDown={(e) => e.stopPropagation()}>
          {expanded ? (
            <Editor
              height="240px"
              defaultLanguage="javascript"
              value={script}
              onChange={(v) => updateNodeData(id, { script: v ?? '' })}
              theme="vs-dark"
              options={{
                minimap: { enabled: false },
                fontSize: 12,
                lineNumbers: 'off',
                scrollBeyondLastLine: false,
                wordWrap: 'on',
                automaticLayout: true,
              }}
            />
          ) : (
            <pre className="text-xs text-muted-foreground max-h-[48px] overflow-hidden leading-5 whitespace-pre-wrap cursor-pointer" onClick={() => setExpanded(true)}>
              {script || <span className="italic">click ▾ to open editor</span>}
            </pre>
          )}
        </div>
      </div>

      <NodeDebugPanel id={id} />
      <Handle type="target" position={Position.Left} />
      <Handle type="source" position={Position.Right} id="success" style={{ top: '35%' }} />
      <Handle
        type="source"
        position={Position.Right}
        id="error"
        style={{ top: '65%', background: 'rgb(239,68,68)' }}
      />
    </div>
  )
}

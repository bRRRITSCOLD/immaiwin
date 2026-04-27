import { NodeResizer, type NodeProps, useReactFlow } from '@xyflow/react'
import { Bug, Container, Maximize2 } from 'lucide-react'
import { useState } from 'react'
import { ScriptEditor } from '~/components/ScriptEditor'
import { StepNameInput } from './StepNameInput'
import { DynamicHandles } from './DynamicHandles'
import { NodeDebugPanel, BreakpointMarker } from '../RunResultsContext'
import { SandboxDebugDialog } from '../SandboxDebugDialog'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from '~/components/ui/dialog'

const LANGUAGES = [
  { value: 'javascript', label: 'JavaScript' },
  { value: 'python', label: 'Python' },
  { value: 'golang', label: 'Go' },
  { value: 'rust', label: 'Rust' },
  { value: 'php', label: 'PHP' },
] as const

type SandboxLanguage = (typeof LANGUAGES)[number]['value']

const monacoLanguageMap: Record<SandboxLanguage, string> = {
  javascript: 'javascript',
  python: 'python',
  golang: 'go',
  rust: 'rust',
  php: 'php',
}

const borderColorMap: Record<SandboxLanguage, string> = {
  javascript: 'border-cyan-500',
  python: 'border-blue-500',
  golang: 'border-teal-500',
  rust: 'border-orange-500',
  php: 'border-purple-500',
}

const iconColorMap: Record<SandboxLanguage, string> = {
  javascript: 'text-cyan-500',
  python: 'text-blue-500',
  golang: 'text-teal-500',
  rust: 'text-orange-500',
  php: 'text-purple-500',
}

const accentColorMap: Record<SandboxLanguage, string> = {
  javascript: 'border-cyan-500/40',
  python: 'border-blue-500/40',
  golang: 'border-teal-500/40',
  rust: 'border-orange-500/40',
  php: 'border-purple-500/40',
}

const helloWorldTemplates: Record<SandboxLanguage, string> = {
  javascript: `// input, context, params available
output({ hello: "world", input })`,
  python: `# input, context, params available
output({"hello": "world", "input": input})`,
  golang: `// input, context, params available
// fmt.Println → stderr | output() → result
output(map[string]any{"hello": "world", "input": input})`,
  rust: `// input, context, params available
// eprintln! → stderr | output() → result
output(json!({"hello": "world", "input": input}));`,
  php: `// $input, $context, $params available
// echo/print → stderr | output() → result
output(["hello" => "world", "input" => $input]);`,
}

const templateValues = new Set(Object.values(helloWorldTemplates))

export function SandboxScriptNode({ id, data, selected }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const script = (data?.script as string) ?? ''
  const language = ((data?.language as string) ?? 'javascript') as SandboxLanguage
  const timeout = (data?.timeout as number) ?? 30
  const memLimit = (data?.mem_limit as number) ?? 128
  const cpuLimit = (data?.cpu_limit as number) ?? 0.5
  const network = (data?.network as boolean) ?? false
  const [open, setOpen] = useState(false)
  const [debugOpen, setDebugOpen] = useState(false)
  const supportsDebug = language === 'javascript' || language === 'python'

  const borderColor = borderColorMap[language] ?? 'border-cyan-500'
  const iconColor = iconColorMap[language] ?? 'text-cyan-500'
  const accentColor = accentColorMap[language] ?? 'border-cyan-500/40'

  function handleLanguageChange(newLang: string) {
    const updates: Record<string, unknown> = { language: newLang }
    // Auto-fill hello world template when script is empty or still a template
    if (!script || templateValues.has(script)) {
      updates.script = helloWorldTemplates[newLang as SandboxLanguage] ?? ''
    }
    updateNodeData(id, updates)
  }

  return (
    <div className="relative min-w-[280px]">
      <BreakpointMarker id={id} />
      <div className={`overflow-x-hidden rounded-lg border-2 ${borderColor} bg-card text-card-foreground shadow-sm`}>
        <NodeResizer minWidth={280} minHeight={80} isVisible={selected} />
        <div className={`flex items-center gap-2 px-4 py-2.5 border-b ${accentColor}`}>
          <Container className={`h-4 w-4 ${iconColor} shrink-0`} />
          <span className="text-sm font-medium flex-1">Sandbox</span>

          {/* Language picker */}
          <select
            className="nodrag text-xs bg-muted border border-border rounded px-1.5 py-0.5 outline-none cursor-pointer"
            value={language}
            onChange={(e) => handleLanguageChange(e.target.value)}
          >
            {LANGUAGES.map((l) => (
              <option key={l.value} value={l.value}>{l.label}</option>
            ))}
          </select>

          {supportsDebug && (
            <button
              className="nodrag text-muted-foreground hover:text-red-400 transition-colors"
              onClick={() => setDebugOpen(true)}
              title="Debug"
            >
              <Bug className="h-3.5 w-3.5" />
            </button>
          )}

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
            Sandbox — <code className="text-[10px]">input</code> · <code className="text-[10px]">context</code> · <code className="text-[10px]">params</code> — isolated container
          </p>
          <pre
            className="nodrag text-xs text-muted-foreground max-h-[72px] overflow-hidden leading-5 whitespace-pre-wrap cursor-pointer rounded bg-muted/30 px-2 py-1.5"
            onClick={() => setOpen(true)}
          >
            {script || <span className="italic opacity-60">click to edit script</span>}
          </pre>
        </div>

        {/* Resource badges */}
        <div className="px-3 pb-2 flex flex-wrap gap-1.5">
          <span className="text-[9px] px-1.5 py-0.5 rounded bg-muted text-muted-foreground">
            {timeout}s timeout
          </span>
          <span className="text-[9px] px-1.5 py-0.5 rounded bg-muted text-muted-foreground">
            {memLimit}MB
          </span>
          <span className="text-[9px] px-1.5 py-0.5 rounded bg-muted text-muted-foreground">
            {cpuLimit} CPU
          </span>
          {network && (
            <span className="text-[9px] px-1.5 py-0.5 rounded bg-orange-900/40 text-orange-300">
              network
            </span>
          )}
        </div>

        <NodeDebugPanel id={id} />
      </div>

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="max-w-5xl sm:max-w-5xl w-[95vw] h-[85vh] flex flex-col">
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2">
              <Container className={`h-4 w-4 ${iconColor}`} />
              Sandbox Script {data?.name ? `— ${data.name}` : ''}
            </DialogTitle>
          </DialogHeader>

          {/* Controls row */}
          <div className="flex flex-wrap items-center gap-3 text-xs">
            <label className="flex items-center gap-1.5">
              Language
              <select
                className="bg-muted border border-border rounded px-2 py-1 outline-none text-xs"
                value={language}
                onChange={(e) => handleLanguageChange(e.target.value)}
              >
                {LANGUAGES.map((l) => (
                  <option key={l.value} value={l.value}>{l.label}</option>
                ))}
              </select>
            </label>

            <label className="flex items-center gap-1.5">
              Timeout (s)
              <input
                type="number"
                min={1}
                max={300}
                className="bg-muted border border-border rounded px-2 py-1 w-16 outline-none text-xs"
                value={timeout}
                onChange={(e) => updateNodeData(id, { timeout: Number(e.target.value) || 30 })}
              />
            </label>

            <label className="flex items-center gap-1.5">
              Memory (MB)
              <input
                type="number"
                min={16}
                max={1024}
                className="bg-muted border border-border rounded px-2 py-1 w-16 outline-none text-xs"
                value={memLimit}
                onChange={(e) => updateNodeData(id, { mem_limit: Number(e.target.value) || 128 })}
              />
            </label>

            <label className="flex items-center gap-1.5">
              CPU
              <input
                type="number"
                min={0.1}
                max={4}
                step={0.1}
                className="bg-muted border border-border rounded px-2 py-1 w-16 outline-none text-xs"
                value={cpuLimit}
                onChange={(e) => updateNodeData(id, { cpu_limit: Number(e.target.value) || 0.5 })}
              />
            </label>

            <label className="flex items-center gap-1.5 cursor-pointer">
              <input
                type="checkbox"
                checked={network}
                onChange={(e) => updateNodeData(id, { network: e.target.checked })}
                className="accent-cyan-500"
              />
              Network
            </label>
          </div>

          <p className="text-xs text-muted-foreground">
            Globals: <code className="text-xs">input</code> · <code className="text-xs">context</code> · <code className="text-xs">params</code> — call <code className="text-xs">output(val)</code> for result
          </p>

          <div className="flex-1 min-h-0">
            <ScriptEditor
              height="100%"
              language={monacoLanguageMap[language]}
              value={script}
              onChange={(v) => updateNodeData(id, { script: v })}
              options={{ fontSize: 14 }}
            />
          </div>
        </DialogContent>
      </Dialog>
      {supportsDebug && (
        <SandboxDebugDialog
          open={debugOpen}
          onOpenChange={setDebugOpen}
          language={language}
          code={script}
          onCodeChange={(v) => updateNodeData(id, { script: v })}
        />
      )}
      <DynamicHandles nodeId={id} nodeType="sandbox_script" data={data as Record<string, unknown>} />
    </div>
  )
}

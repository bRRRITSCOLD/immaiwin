import { NodeResizer, type NodeProps, useReactFlow } from '@xyflow/react'
import { Bug, Container, HelpCircle, Maximize2, Package } from 'lucide-react'
import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { Tooltip, TooltipTrigger, TooltipContent } from '~/components/ui/tooltip'
import { NumberField } from '~/components/ui/number-field'
import { StepNameInput } from './StepNameInput'
import { DynamicHandles } from './DynamicHandles'
import { AsToolPanel } from './AsToolPanel'
import { NodeDebugPanel, BreakpointMarker, ApprovalMarker } from '../RunResultsContext'
import { SandboxDebugDialog, type DialogMode } from '../SandboxDebugDialog'

const LANGUAGES = [
  { value: 'javascript', label: 'JavaScript' },
  { value: 'python', label: 'Python' },
  { value: 'golang', label: 'Go' },
  { value: 'rust', label: 'Rust' },
  { value: 'php', label: 'PHP' },
] as const

type SandboxLanguage = (typeof LANGUAGES)[number]['value']

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
// packages: const cheerio = await import('cheerio')
// console.log → stderr | output() → result
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

const defaultImageMap: Record<SandboxLanguage, string> = {
  javascript: 'immaiwin/sandbox-node:20',
  python: 'immaiwin/sandbox-python:3.12',
  golang: 'immaiwin/sandbox-go:1.22',
  rust: 'immaiwin/sandbox-rust:1.86',
  php: 'immaiwin/sandbox-php:8.3',
}

const packagesPlaceholder: Record<SandboxLanguage, string> = {
  javascript: 'e.g. cheerio, axios',
  python: 'e.g. requests, beautifulsoup4',
  golang: 'e.g. github.com/go-resty/resty/v2',
  rust: 'e.g. serde, reqwest',
  php: 'e.g. guzzlehttp/guzzle',
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
  const customImage = (data?.custom_image as string) ?? ''
  const packages = (data?.packages as string) ?? ''
  const [dialogOpen, setDialogOpen] = useState(false)
  const [dialogMode, setDialogMode] = useState<DialogMode>('run')
  const supportsDebug = language === 'javascript' || language === 'python'

  function openDialog(mode: DialogMode) {
    setDialogMode(mode)
    setDialogOpen(true)
  }

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

  const requireApproval = (data?.require_node_approval as boolean) ?? false

  return (
    <div className="relative min-w-[280px] h-full">
      <BreakpointMarker id={id} />
      <ApprovalMarker id={id} enabled={requireApproval} onToggle={(_, next) => updateNodeData(id, { require_node_approval: next })} />
      <div className={`overflow-x-hidden rounded-lg border-2 ${borderColor} bg-card text-card-foreground shadow-sm h-full`}>
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
              onClick={() => openDialog('debug')}
              title="Debug"
            >
              <Bug className="h-3.5 w-3.5" />
            </button>
          )}

          <button
            className="nodrag text-muted-foreground hover:text-foreground transition-colors"
            onClick={() => openDialog('run')}
            title="Open editor"
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
            onClick={() => openDialog('run')}
          >
            {script || <span className="italic opacity-60">click to edit script</span>}
          </pre>
        </div>

        {/* Resource controls */}
        <div className="px-3 pb-2 flex flex-wrap items-center gap-x-2 gap-y-1">
          <label className="nodrag flex items-center gap-1 text-[9px] text-muted-foreground">
            Timeout
            <input
              type="number"
              min={1}
              max={300}
              className="w-10 bg-muted border border-border rounded px-1 py-0.5 text-[9px] outline-none"
              value={timeout}
              onChange={(e) => updateNodeData(id, { timeout: Number(e.target.value) || 30 })}
            />
            <span>s</span>
          </label>
          <label className="nodrag flex items-center gap-1 text-[9px] text-muted-foreground">
            Mem
            <input
              type="number"
              min={16}
              max={1024}
              className="w-10 bg-muted border border-border rounded px-1 py-0.5 text-[9px] outline-none"
              value={memLimit}
              onChange={(e) => updateNodeData(id, { mem_limit: Number(e.target.value) || 128 })}
            />
            <span>MB</span>
          </label>
          <label className="nodrag flex items-center gap-1 text-[9px] text-muted-foreground">
            CPU
            <NumberField
              min={0.1}
              max={4}
              step={0.1}
              className="w-10 bg-muted border border-border rounded px-1 py-0.5 text-[9px] outline-none h-auto shadow-none"
              value={cpuLimit}
              emptyValue={0.5}
              onChange={(v) => updateNodeData(id, { cpu_limit: v || 0.5 })}
            />
          </label>
          <label className="nodrag flex items-center gap-1 text-[9px] text-muted-foreground cursor-pointer">
            <input
              type="checkbox"
              checked={network}
              onChange={(e) => updateNodeData(id, { network: e.target.checked })}
              className="accent-cyan-500 h-2.5 w-2.5"
            />
            Network
          </label>
        </div>

        {/* Custom image */}
        <div className="px-3 pb-2">
          <label className="nodrag flex items-center gap-1 text-[9px] text-muted-foreground">
            Image
            <Tooltip>
              <TooltipTrigger asChild>
                <Link
                  to="/docs/custom-images"
                  target="_blank"
                  className="nodrag inline-flex"
                  onClick={(e) => e.stopPropagation()}
                >
                  <HelpCircle className="h-2.5 w-2.5 text-muted-foreground hover:text-foreground transition-colors cursor-help" />
                </Link>
              </TooltipTrigger>
              <TooltipContent side="top">
                Click for docs &amp; examples
              </TooltipContent>
            </Tooltip>
            <input
              type="text"
              className="flex-1 bg-muted border border-border rounded px-1.5 py-0.5 text-[9px] outline-none"
              value={customImage}
              onChange={(e) => updateNodeData(id, { custom_image: e.target.value })}
              placeholder={defaultImageMap[language] ?? 'custom image tag'}
            />
          </label>
        </div>

        {/* Packages — auto-builds image with these installed */}
        {!customImage && (
          <div className="px-3 pb-2">
            <label className="nodrag flex items-center gap-1 text-[9px] text-muted-foreground">
              <Package className="h-2.5 w-2.5 shrink-0" />
              Packages
              <Tooltip>
                <TooltipTrigger>
                  <HelpCircle className="h-2.5 w-2.5 text-muted-foreground hover:text-foreground transition-colors cursor-help" />
                </TooltipTrigger>
                <TooltipContent side="top">
                  Comma-separated. Auto-builds Docker image with these installed.
                </TooltipContent>
              </Tooltip>
              <input
                type="text"
                className="flex-1 bg-muted border border-border rounded px-1.5 py-0.5 text-[9px] outline-none"
                value={packages}
                onChange={(e) => updateNodeData(id, { packages: e.target.value })}
                placeholder={packagesPlaceholder[language] ?? 'comma-separated packages'}
              />
            </label>
          </div>
        )}

        <AsToolPanel
          nodeId={id}
          data={data as Record<string, unknown>}
          defaultName="sandbox_script"
          defaultSchema={{ type: 'object', properties: { input: {} }, additionalProperties: true }}
        />
        <NodeDebugPanel id={id} />
      </div>

      <SandboxDebugDialog
        open={dialogOpen}
        onOpenChange={setDialogOpen}
        language={language}
        code={script}
        onCodeChange={(v) => updateNodeData(id, { script: v })}
        initialMode={dialogMode}
        customImage={customImage || undefined}
        packages={packages || undefined}
        network={network}
      />
      <DynamicHandles nodeId={id} nodeType="sandbox_script" data={data as Record<string, unknown>} />
    </div>
  )
}

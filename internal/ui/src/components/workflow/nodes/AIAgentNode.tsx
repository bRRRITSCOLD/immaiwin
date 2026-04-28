import { NodeResizer, type NodeProps, useReactFlow } from '@xyflow/react'
import { Bot, Wrench, HelpCircle } from 'lucide-react'
import { useState } from 'react'
import { Textarea } from '~/components/ui/textarea'
import { Input } from '~/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '~/components/ui/select'
import { Tooltip, TooltipTrigger, TooltipContent } from '~/components/ui/tooltip'
import { StepNameInput } from './StepNameInput'
import { DynamicHandles } from './DynamicHandles'
import { SkillsPanel } from './SkillsPanel'
import { NodeDebugPanel, BreakpointMarker } from '../RunResultsContext'
import { useWorkflowStore } from '../useWorkflowStore'

const LLM_TYPES = ['anthropic', 'openai', 'ollama'] as const

export function AIAgentNode({ id, data, selected }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const connections = useWorkflowStore((s) => s.connections)
  const llmConnections = connections.filter((c) => (LLM_TYPES as readonly string[]).includes(c.type))

  const [advancedOpen, setAdvancedOpen] = useState(false)

  const systemPrompt = (data?.system_prompt as string) ?? ''
  const userInput = (data?.user_input as string) ?? ''
  const llmConnId = (data?.llm_connection_id as string) ?? ''
  const modelOverride = (data?.model_override as string) ?? ''
  const memorySessionId = (data?.memory_session_id as string) ?? ''
  const maxIters = (data?.max_iterations as number) ?? 8
  const maxTokens = (data?.max_tokens as number) ?? 4096
  const temperature = (data?.temperature as number) ?? 1
  const timeoutSec = (data?.timeout_seconds as number) ?? 300

  return (
    <div className="relative min-w-[320px] h-full">
      <BreakpointMarker id={id} />
      <div className="overflow-x-hidden rounded-lg border-2 border-purple-400 bg-card text-card-foreground shadow-sm h-full">
        <NodeResizer minWidth={320} minHeight={120} isVisible={selected} />
        <div className="flex items-center gap-2 px-4 py-2.5 border-b border-purple-400/40">
          <Bot className="h-4 w-4 text-purple-400 shrink-0" />
          <span className="text-sm font-medium">AI Agent</span>
        </div>
        <StepNameInput id={id} data={data} />

        {/* LLM connection picker */}
        <div className="px-3 py-2 space-y-2">
          <div>
            <p className="text-[10px] text-muted-foreground">LLM Connection (anthropic / openai / ollama)</p>
            <Select
              value={llmConnId || '__none__'}
              onValueChange={(v) => updateNodeData(id, { llm_connection_id: v === '__none__' ? '' : v })}
            >
              <SelectTrigger className="nodrag h-7 text-xs">
                <SelectValue placeholder="Select LLM connection" />
              </SelectTrigger>
              <SelectContent className="z-[9999]">
                <SelectItem value="__none__">— Required —</SelectItem>
                {llmConnections.map((c) => (
                  <SelectItem key={c.id} value={c.id}>
                    {c.name} ({c.type})
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          {/* System prompt */}
          <div>
            <p className="text-[10px] text-muted-foreground">
              System prompt — supports <code className="text-[10px]">{'{{…}}'}</code> templates
            </p>
            <Textarea
              className="nodrag text-xs min-h-[60px] resize-y"
              rows={3}
              placeholder="You are a helpful assistant. Use the available tools to..."
              value={systemPrompt}
              onChange={(e) => updateNodeData(id, { system_prompt: e.target.value })}
            />
          </div>

          {/* User input */}
          <div>
            <p className="text-[10px] text-muted-foreground">
              User input — defaults to JSON of upstream input. Templates supported.
            </p>
            <Textarea
              className="nodrag text-xs min-h-[40px] resize-y"
              rows={2}
              placeholder="e.g. {{input.question}}"
              value={userInput}
              onChange={(e) => updateNodeData(id, { user_input: e.target.value })}
            />
          </div>
        </div>

        {/* Advanced collapsible */}
        <div className="border-t border-border/50">
          <button
            type="button"
            onClick={() => setAdvancedOpen((v) => !v)}
            className="nodrag flex w-full items-center justify-between gap-2 px-3 py-1.5 text-[10px] font-medium text-muted-foreground hover:text-foreground"
          >
            <span className="flex items-center gap-1.5">
              <Wrench className="h-3 w-3" />
              Advanced
            </span>
            <span>{advancedOpen ? '▾' : '▸'}</span>
          </button>
          {advancedOpen && (
            <div className="px-3 pb-2 space-y-2">
              <div>
                <p className="text-[10px] text-muted-foreground">Model override (optional)</p>
                <Input
                  className="nodrag h-7 text-xs"
                  placeholder="e.g. claude-opus-4-7"
                  value={modelOverride}
                  onChange={(e) => updateNodeData(id, { model_override: e.target.value })}
                />
              </div>
              <div>
                <p className="text-[10px] text-muted-foreground">Memory session ID (template-enabled). Empty = no persistence.</p>
                <Input
                  className="nodrag h-7 text-xs"
                  placeholder="e.g. user_{{params.user_id}}"
                  value={memorySessionId}
                  onChange={(e) => updateNodeData(id, { memory_session_id: e.target.value })}
                />
              </div>
              <div className="grid grid-cols-2 gap-2">
                <div>
                  <div className="flex items-center gap-1">
                    <p className="text-[10px] text-muted-foreground">Max iterations</p>
                    <Tooltip>
                      <TooltipTrigger asChild>
                        <button type="button" className="nodrag text-muted-foreground hover:text-foreground" aria-label="Max iterations help">
                          <HelpCircle className="h-3 w-3" />
                        </button>
                      </TooltipTrigger>
                      <TooltipContent side="top" className="max-w-[280px] text-[11px] leading-snug">
                        <p className="font-medium mb-1">ReAct loop iteration cap.</p>
                        <p>One iteration = 1 LLM call + (optional) tool calls + observations fed back. Loop ends when the model returns text instead of tool calls (final answer).</p>
                        <p className="mt-1">Typical run: 1 iter to call a tool, 1 iter to summarize → set ≥ 2. Multi-step research may need 4–8.</p>
                        <p className="mt-1">Hitting the cap aborts with "max iterations exceeded". Raise if the agent keeps cutting off mid-task; lower to bound cost.</p>
                      </TooltipContent>
                    </Tooltip>
                  </div>
                  <Input
                    className="nodrag h-7 text-xs"
                    type="number"
                    min={1}
                    max={50}
                    value={maxIters}
                    onChange={(e) => updateNodeData(id, { max_iterations: Number(e.target.value) })}
                  />
                </div>
                <div>
                  <p className="text-[10px] text-muted-foreground">Max tokens</p>
                  <Input
                    className="nodrag h-7 text-xs"
                    type="number"
                    min={256}
                    max={16384}
                    value={maxTokens}
                    onChange={(e) => updateNodeData(id, { max_tokens: Number(e.target.value) })}
                  />
                </div>
                <div>
                  <p className="text-[10px] text-muted-foreground">Temperature</p>
                  <Input
                    className="nodrag h-7 text-xs"
                    type="number"
                    min={0}
                    max={2}
                    step={0.1}
                    value={temperature}
                    onChange={(e) => updateNodeData(id, { temperature: Number(e.target.value) })}
                  />
                </div>
                <div>
                  <p className="text-[10px] text-muted-foreground">Timeout (s)</p>
                  <Input
                    className="nodrag h-7 text-xs"
                    type="number"
                    min={30}
                    max={3600}
                    value={timeoutSec}
                    onChange={(e) => updateNodeData(id, { timeout_seconds: Number(e.target.value) })}
                  />
                </div>
              </div>
            </div>
          )}
        </div>

        <div className="px-3 py-2 border-t border-border/50">
          <p className="text-[10px] text-muted-foreground">
            Connect tool nodes via the <span className="text-purple-400 font-medium">tool</span> edge to expose them to the agent.
            Built-in <code className="text-[10px]">code_execute</code> tool is always available (sandbox required).
          </p>
        </div>

        <SkillsPanel nodeId={id} data={data as Record<string, unknown>} />
        <NodeDebugPanel id={id} />
      </div>
      <DynamicHandles nodeId={id} nodeType="ai_agent" data={data as Record<string, unknown>} />
    </div>
  )
}

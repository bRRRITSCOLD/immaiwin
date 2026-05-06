import { NodeResizer, type NodeProps, useReactFlow, useNodes } from '@xyflow/react'
import { Bot, Wrench, HelpCircle } from 'lucide-react'
import { useContext, useState } from 'react'
import { Textarea } from '~/components/ui/textarea'
import { Input } from '~/components/ui/input'
import { NumberField } from '~/components/ui/number-field'
import { Tooltip, TooltipTrigger, TooltipContent } from '~/components/ui/tooltip'
import { Switch } from '~/components/ui/switch'
import { StepNameInput } from './StepNameInput'
import { DynamicHandles } from './DynamicHandles'
import { SkillsPanel } from './SkillsPanel'
import { AgentTimelinePanel } from './AgentTimelinePanel'
import { NodeDebugPanel, BreakpointMarker, ApprovalMarker, AgentRunContext } from '../RunResultsContext'
import { ConnectionPicker } from './ConnectionPicker'

const LLM_TYPES = ['anthropic', 'openai', 'ollama'] as const

export function AIAgentNode({ id, data, selected }: NodeProps) {
  const { updateNodeData } = useReactFlow()

  const [advancedOpen, setAdvancedOpen] = useState(false)

  const systemPrompt = (data?.system_prompt as string) ?? ''
  const userInput = (data?.user_input as string) ?? ''
  const modelOverride = (data?.model_override as string) ?? ''
  const memorySessionId = (data?.memory_session_id as string) ?? ''
  const maxIters = (data?.max_iterations as number) ?? 8
  const maxTokens = (data?.max_tokens as number) ?? 4096
  const temperature = (data?.temperature as number) ?? 1
  const timeoutSec = (data?.timeout_seconds as number) ?? 300
  const requireApproval = (data?.require_approval as boolean) ?? false
  const stopOnToolError = (data?.stop_on_tool_error as boolean) ?? false
  const requireNodeApproval = (data?.require_node_approval as boolean) ?? false
  const outputSchema = (data?.output_schema as string) ?? ''

  // Trigger awareness — surfaced in tooltip copy. Both manual and
  // non-manual triggers now route through the approval gate: manual
  // uses the live WS approveCh, non-manual uses out-of-band approval
  // (run lands in `pending_approval`, user clicks Approve/Reject from
  // /runs/:id). So the toggle is always live.
  const allNodes = useNodes()
  const triggerNode = allNodes.find((n) => n.type === 'trigger')
  const triggerType = ((triggerNode?.data as Record<string, unknown> | undefined)?.['trigger_type'] as string) ?? 'manual'
  const isManualTrigger = triggerType === 'manual'

  return (
    <div className="relative min-w-[320px] h-full">
      <BreakpointMarker id={id} />
      <ApprovalMarker id={id} enabled={requireNodeApproval} onToggle={(_, next) => updateNodeData(id, { require_node_approval: next })} />
      <div className="overflow-x-hidden rounded-lg border-2 border-purple-400 bg-card text-card-foreground shadow-sm h-full">
        <NodeResizer minWidth={320} minHeight={120} isVisible={selected} />
        <div className="flex items-center gap-2 px-4 py-2.5 border-b border-purple-400/40">
          <Bot className="h-4 w-4 text-purple-400 shrink-0" />
          <span className="text-sm font-medium">AI Agent</span>
          <AgentCostBadge id={id} />
        </div>
        <StepNameInput id={id} data={data} />

        {/* LLM connection picker */}
        <div className="px-3 py-2 space-y-2">
          <div>
            <p className="text-[10px] text-muted-foreground">LLM Connection (anthropic / openai / ollama)</p>
            <ConnectionPicker
              nodeId={id}
              connectionType={[...LLM_TYPES]}
              data={data as Record<string, unknown>}
              field="llm_connection_id"
              variant="full"
              requireExplicit
            />
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
                  <NumberField
                    className="nodrag h-7 text-xs"
                    min={1}
                    max={50}
                    value={maxIters}
                    onChange={(v) => updateNodeData(id, { max_iterations: v })}
                  />
                </div>
                <div>
                  <p className="text-[10px] text-muted-foreground">Max tokens</p>
                  <NumberField
                    className="nodrag h-7 text-xs"
                    min={256}
                    max={16384}
                    value={maxTokens}
                    onChange={(v) => updateNodeData(id, { max_tokens: v })}
                  />
                </div>
                <div>
                  <p className="text-[10px] text-muted-foreground">Temperature</p>
                  <NumberField
                    className="nodrag h-7 text-xs"
                    min={0}
                    max={2}
                    step={0.1}
                    value={temperature}
                    onChange={(v) => updateNodeData(id, { temperature: v })}
                  />
                </div>
                <div>
                  <p className="text-[10px] text-muted-foreground">Timeout (s)</p>
                  <NumberField
                    className="nodrag h-7 text-xs"
                    min={30}
                    max={3600}
                    value={timeoutSec}
                    onChange={(v) => updateNodeData(id, { timeout_seconds: v })}
                  />
                </div>
              </div>

              {/* Output schema — when non-empty, agent must deliver via
                  submit_final_answer tool whose input_schema = this. Pairs
                  with eval json_path_eq for guaranteed-shape outputs. */}
              <div>
                <div className="flex items-center gap-1">
                  <p className="text-[10px] text-muted-foreground">Output schema (JSON Schema)</p>
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <button
                        type="button"
                        className="nodrag text-muted-foreground hover:text-foreground"
                        aria-label="Output schema help"
                      >
                        <HelpCircle className="h-3 w-3" />
                      </button>
                    </TooltipTrigger>
                    <TooltipContent side="top" className="max-w-[300px] text-[11px] leading-snug">
                      <p className="font-medium mb-1">Force structured output.</p>
                      <p>When set, a synthetic <code>submit_final_answer</code> tool with this schema is appended to the agent's catalog. The model is instructed to call it instead of returning free text. Args are validated, then become this agent's <code>output</code>.</p>
                      <p className="mt-1">Empty = free-text output (current default).</p>
                      <p className="mt-1">Pairs naturally with eval <code>json_path_eq</code> assertions.</p>
                    </TooltipContent>
                  </Tooltip>
                </div>
                <Textarea
                  className="nodrag text-xs min-h-[60px] resize-y font-mono"
                  rows={4}
                  placeholder='{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}'
                  value={outputSchema}
                  onChange={(e) => updateNodeData(id, { output_schema: e.target.value })}
                />
              </div>

              {/* Require approval toggle — gates each tool call on a
                  human verdict. Manual trigger: live WS approve/reject
                  buttons on the agent timeline. Non-manual trigger:
                  run lands in `pending_approval`; resolver acts via
                  /runs/:id (Stage 1) or, eventually, an emailed/Slack
                  link (Stage 2 backlog). Either way: deadlock-free. */}
              <div className="flex items-center justify-between gap-2 pt-1">
                <div className="flex items-center gap-1">
                  <p className="text-[10px] text-muted-foreground">
                    Require approval per tool call
                  </p>
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <button
                        type="button"
                        className="nodrag text-muted-foreground hover:text-foreground"
                        aria-label="Require approval help"
                      >
                        <HelpCircle className="h-3 w-3" />
                      </button>
                    </TooltipTrigger>
                    <TooltipContent side="top" className="max-w-[300px] text-[11px] leading-snug">
                      <p className="font-medium mb-1">Human-in-the-loop tool gate.</p>
                      <p>When on, the agent pauses before each tool call and waits for an Approve / Reject decision.</p>
                      <p className="mt-1">Rejections feed the model an error observation so it can react in the next iter. Off = tools fire immediately.</p>
                      {isManualTrigger ? (
                        <p className="mt-1 text-muted-foreground/80">Manual trigger: decision via the live agent timeline buttons.</p>
                      ) : (
                        <p className="mt-1 text-muted-foreground/80">
                          Non-manual trigger ("<code>{triggerType}</code>"): run lands in <code>pending_approval</code>; resolve via /runs/:id Approve/Reject.
                        </p>
                      )}
                    </TooltipContent>
                  </Tooltip>
                </div>
                <Switch
                  className="nodrag"
                  checked={requireApproval}
                  onCheckedChange={(v) => updateNodeData(id, { require_approval: v })}
                />
              </div>

              {/* stop_on_tool_error: when on, any tool error from the
                  agent's catalog (skill/sandbox/as_tool) terminates
                  the run with status=error and follows the agent's
                  error edge in the BFS. Default off keeps the LLM's
                  ability to self-correct on transient tool failures
                  (flaky upstream API, etc.). Sandbox-engine errors
                  (k3s pod failed, docker daemon down, …) terminate
                  regardless because the model can't recover from
                  infra failures. */}
              <div className="flex items-center justify-between gap-2 pt-1">
                <div className="flex items-center gap-1">
                  <p className="text-[10px] text-muted-foreground">
                    Stop run on tool error
                  </p>
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <button
                        type="button"
                        className="nodrag text-muted-foreground hover:text-foreground"
                        aria-label="Stop on tool error help"
                      >
                        <HelpCircle className="h-3 w-3" />
                      </button>
                    </TooltipTrigger>
                    <TooltipContent side="top" className="max-w-[320px] text-[11px] leading-snug">
                      <p className="font-medium mb-1">Strict-error mode.</p>
                      <p>When on: any tool error (skill, sandbox, as_tool target) ends the agent immediately. Run badge flips to <code>error</code>; BFS follows the agent's error edge.</p>
                      <p className="mt-1">When off (default): tool errors are fed back to the LLM as observations so the model can pivot or retry within <code>max_iterations</code>. Useful for resilient agents that wrap flaky APIs.</p>
                      <p className="mt-1 text-muted-foreground/80">Sandbox-engine errors (k3s pod failed, docker daemon down) terminate regardless of this flag — the LLM has no path to recover from infra failures.</p>
                    </TooltipContent>
                  </Tooltip>
                </div>
                <Switch
                  className="nodrag"
                  checked={stopOnToolError}
                  onCheckedChange={(v) => updateNodeData(id, { stop_on_tool_error: v })}
                />
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
        <AgentTimelinePanel id={id} />
        <NodeDebugPanel id={id} />
      </div>
      <DynamicHandles nodeId={id} nodeType="ai_agent" data={data as Record<string, unknown>} />
    </div>
  )
}

// AgentCostBadge tallies token + cost across every iter of the live run
// for this agent node and renders inline in the header. Hidden when no
// iters yet (idle/no-run). Updates on every `agent_llm` event arrival
// because AgentRunContext is rebuilt by the page-level memo.
function AgentCostBadge({ id }: { id: string }) {
  const ctx = useContext(AgentRunContext)
  const iters = ctx?.[id] ?? []
  if (iters.length === 0) return null
  let inTok = 0
  let outTok = 0
  let cost = 0
  for (const iter of iters) {
    inTok += iter.llm?.usage?.input_tokens ?? 0
    outTok += iter.llm?.usage?.output_tokens ?? 0
    cost += iter.llm?.usage?.cost_usd ?? 0
  }
  return (
    <span className="ml-auto flex items-center gap-2 text-[10px] text-muted-foreground tabular-nums">
      <span title="input → output tokens">
        {inTok.toLocaleString()}/{outTok.toLocaleString()} tok
      </span>
      {cost > 0 && (
        <span className="text-foreground/80 font-medium" title="estimated cost (provider pricing table)">
          ${cost.toFixed(4)}
        </span>
      )}
    </span>
  )
}

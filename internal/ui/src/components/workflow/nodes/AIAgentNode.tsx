import { NodeResizer, type NodeProps, useReactFlow, useNodes, useEdges } from '@xyflow/react'
import { Bot, Wrench, HelpCircle, Plus } from 'lucide-react'
import { useContext, useEffect, useState } from 'react'
import { api } from '~/lib/api'
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
import { OutputTransformPanel } from './OutputTransformPanel'
import { ConnectionPicker } from './ConnectionPicker'
import { OnErrorPolicySelect } from './OnErrorPolicySelect'
import { AsToolPanel } from './AsToolPanel'
import { handleJsonTextareaTab } from './jsonTextareaTab'

// Default input schema for an ai_agent exposed as a tool — single
// `task` string. Authors override per-agent via AsToolPanel's
// input_schema field.
const AI_AGENT_TOOL_SCHEMA: Record<string, unknown> = {
  type: 'object',
  properties: {
    task: { type: 'string', description: 'Instruction or question for the sub-agent.' },
  },
  required: ['task'],
}

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
  const onErrorPolicy = (data?.on_error as string) ?? 'stop'
  const requireNodeApproval = (data?.require_node_approval as boolean) ?? false
  const outputSchema = (data?.output_schema as string) ?? ''
  // Per-agent tool authorization policy. Stored on the node as
  // `allowed_tools` — the textarea binds to it as a raw string so
  // the user can press Enter mid-edit without the eager
  // split-filter-rejoin loop swallowing the newline. The engine's
  // `stringSliceData` coercer splits on both `\n` and `,`, so a
  // legacy `[]string` value still round-trips fine.
  const allowedToolsText = (() => {
    const v = data?.allowed_tools
    if (typeof v === 'string') return v
    if (Array.isArray(v)) return (v as string[]).join('\n')
    return ''
  })()

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
                  placeholder="e.g. user_{{config.user_id}}"
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
                  onKeyDown={(e) => handleJsonTextareaTab(e, outputSchema, (v) => updateNodeData(id, { output_schema: v }))}
                  onChange={(e) => updateNodeData(id, { output_schema: e.target.value })}
                />
              </div>

              {/* Allowed tools — per-agent authorization policy. Empty
                  = open (every tool from built-ins / skills / as_tool
                  edges is exposed to the LLM). Non-empty = allow-list
                  filter applied to the catalog right before dispatch.
                  Defence-in-depth: a hallucinated call to a denied
                  name falls through to unknown-tool, NEVER fires the
                  handler. */}
              <div className="space-y-1 pt-1">
                <div className="flex items-center gap-1">
                  <p className="text-[10px] text-muted-foreground">
                    Allowed tools (one per line; empty = all allowed)
                  </p>
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <button
                        type="button"
                        className="nodrag text-muted-foreground hover:text-foreground"
                        aria-label="Allowed tools help"
                      >
                        <HelpCircle className="h-3 w-3" />
                      </button>
                    </TooltipTrigger>
                    <TooltipContent side="top" className="max-w-[340px] text-[11px] leading-snug">
                      <p className="font-medium mb-1">Per-agent tool ACL.</p>
                      <p>Names use the same form the LLM sees: built-ins like <code>code_execute</code>, as_tool node names (the <code>name</code> field on each tool node), and skill tools as <code>&lt;slug&gt;__&lt;tool&gt;</code>.</p>
                      <p className="mt-1">Empty = every tool exposed (back-compat with workflows authored before this field). Non-empty = STRICT allow-list — anything not listed is removed from the catalog and a hallucinated call to it surfaces as <code>unknown tool</code>.</p>
                      <p className="mt-1 text-muted-foreground/80">Skill tools are <em>self-contained</em>: each skill's code runs directly in the sandbox runtime, not through the <code>code_execute</code> tool. Allowing <code>&lt;slug&gt;__&lt;tool&gt;</code> alone is enough — you only need <code>code_execute</code> if you want the LLM to write + run arbitrary code itself.</p>
                    </TooltipContent>
                  </Tooltip>
                </div>
                <Textarea
                  className="nodrag text-xs min-h-[48px] resize-y font-mono"
                  rows={3}
                  placeholder={'code_execute\nget_weather\nweather-formatter__format_weather'}
                  value={allowedToolsText}
                  // Persist the RAW textarea string — the engine
                  // splits on \n + , so a trailing newline mid-edit
                  // doesn't get re-normalised away on every keystroke.
                  onChange={(e) => updateNodeData(id, { allowed_tools: e.target.value })}
                />
                <RegisteredToolChips
                  agentId={id}
                  agentData={data as Record<string, unknown>}
                  allowedText={allowedToolsText}
                  onAdd={(name) => {
                    const cur = allowedToolsText
                    const lines = cur.split('\n').map((s) => s.trim())
                    if (lines.includes(name)) return
                    const sep = cur.length > 0 && !cur.endsWith('\n') ? '\n' : ''
                    updateNodeData(id, { allowed_tools: cur + sep + name + '\n' })
                  }}
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
            </div>
          )}
        </div>

        <div className="px-3 py-2 border-t border-border/50">
          <p className="text-[10px] text-muted-foreground">
            Connect tool nodes via the <span className="text-purple-400 font-medium">tool</span> edge to expose them to the agent.
          </p>
          <p className="text-[10px] text-muted-foreground/80 mt-1">
            <span className="font-medium">Built-in tools (always available):</span>{' '}
            <code className="text-[10px]">code_execute</code> — run code in the sandbox (5 langs, gVisor-isolated; requires sandbox configured).{' '}
            <code className="text-[10px]">fan_out</code> — dispatch <code className="text-[10px]">{`{calls:[{tool,args}], parallelism?}`}</code> in parallel against this agent's other tools; returns <code className="text-[10px]">{`{results:[{output,error}]}`}</code> in input order. Sibling failures don't abort.
          </p>
        </div>

        <SkillsPanel nodeId={id} data={data as Record<string, unknown>} />
        <AgentTimelinePanel id={id} />
        <AsToolPanel
          nodeId={id}
          data={data as Record<string, unknown>}
          defaultName="sub_agent"
          defaultSchema={AI_AGENT_TOOL_SCHEMA}
        />
        <div className="px-3 py-2 border-t border-border/50">
          <OnErrorPolicySelect nodeId={id} value={onErrorPolicy} nodeType="ai_agent" />
        </div>
        <OutputTransformPanel nodeId={id} data={data as Record<string, unknown>} />
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

// RegisteredToolChips lists the tool names the agent would expose
// at run time (built-ins + skill tools + `as_tool` edge targets),
// rendered as clickable chips that append into the allowed_tools
// textarea on click. Predicts the catalog client-side from canvas
// state — same naming rules the executor's buildAgentToolCatalog
// follows (skill tools = `<sanitized-slug>__<tool-id>`, as_tool =
// the target's `data.as_tool.name` || `data.name` || `<type>_<id>`).
// Eliminates the hyphen/underscore typo class that wiped the
// catalog at runtime even though the workflow ran without error.

interface SkillToolDef { id: string }
interface SkillManifest { tools?: SkillToolDef[] }
interface SkillRegistryRecord {
  slug_id: string
  version: string
  manifest: SkillManifest
}
interface SkillRequest { slug_id: string }

function sanitizeToolNameJS(name: string): string {
  // Mirror the Go sanitizeToolName (agent_tools.go:320): keep
  // a-z A-Z 0-9 _ -, replace everything else with _.
  return name.replace(/[^a-zA-Z0-9_-]/g, '_')
}

function RegisteredToolChips({
  agentId,
  agentData,
  allowedText,
  onAdd,
}: {
  agentId: string
  agentData: Record<string, unknown>
  allowedText: string
  onAdd: (name: string) => void
}) {
  const nodes = useNodes()
  const edges = useEdges()

  const [registry, setRegistry] = useState<SkillRegistryRecord[]>([])
  // Only fetch when the agent declares skills; the registry call is
  // tenant-scoped + cheap but no reason to make it on every render
  // of an agent that doesn't use skills.
  const skills = (agentData?.skills as SkillRequest[] | undefined) ?? []
  useEffect(() => {
    if (skills.length === 0) return
    let cancelled = false
    api.get<SkillRegistryRecord[]>('/api/v1/skills')
      .then((recs) => {
        if (!cancelled) setRegistry(recs)
      })
      .catch(() => {
        // Silent — chips are a convenience, not a blocker. Failure
        // just means the user types skill tool names manually.
      })
    return () => { cancelled = true }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [JSON.stringify(skills)])

  // Built-in tools (registered on every agent catalog by buildAgentToolCatalog).
  const builtinNames = ['code_execute', 'fan_out']

  // Edge-bound as_tool target names. Mirror the executor's lookup:
  // outgoing edges with sourceHandle === 'tool' (EdgeHandleTool)
  // whose target node has data.as_tool.enabled.
  const asToolNames: string[] = []
  for (const e of edges) {
    if (e.source !== agentId) continue
    if (e.sourceHandle !== 'tool') continue
    const target = nodes.find((n) => n.id === e.target)
    if (!target) continue
    const td = (target.data ?? {}) as Record<string, unknown>
    const asTool = td.as_tool as Record<string, unknown> | undefined
    if (!asTool || asTool.enabled !== true) continue
    const name =
      (typeof asTool.name === 'string' && asTool.name) ||
      (typeof td.name === 'string' && (td.name as string)) ||
      `${target.type ?? 'node'}_${target.id}`
    asToolNames.push(sanitizeToolNameJS(name))
  }

  // Skill tool names, computed from the registry once it's loaded.
  const skillNames: string[] = []
  for (const req of skills) {
    const rec = registry.find((r) => r.slug_id === req.slug_id)
    if (!rec || !rec.manifest?.tools) continue
    const prefix = sanitizeToolNameJS(rec.slug_id.replace(/\//g, '_'))
    for (const t of rec.manifest.tools) {
      skillNames.push(sanitizeToolNameJS(`${prefix}__${t.id}`))
    }
  }

  const all = [...builtinNames, ...asToolNames, ...skillNames]
  if (all.length === 0) return null

  // Mark chips that are already in the allowed list so the user
  // sees what's wired vs what they could add.
  const allowedSet = new Set(
    allowedText.split('\n').map((s) => s.trim()).filter((s) => s.length > 0),
  )

  return (
    <div className="flex flex-wrap gap-1 pt-1.5">
      <span className="text-[10px] text-muted-foreground self-center">
        Registered:
      </span>
      {all.map((name) => {
        const inList = allowedSet.has(name)
        return (
          <button
            key={name}
            type="button"
            className={`nodrag flex items-center gap-0.5 rounded border px-1.5 py-0.5 text-[10px] font-mono leading-none transition-colors ${
              inList
                ? 'border-purple-500/40 bg-purple-500/10 text-purple-300 cursor-default'
                : 'border-border text-muted-foreground hover:border-purple-500/40 hover:text-purple-300'
            }`}
            onClick={() => {
              if (inList) return
              onAdd(name)
            }}
            title={inList ? 'already allowed' : 'click to add'}
          >
            {!inList && <Plus className="h-2.5 w-2.5" />}
            {name}
          </button>
        )
      })}
    </div>
  )
}

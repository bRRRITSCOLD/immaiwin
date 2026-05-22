import { NodeResizer, type NodeProps, useReactFlow } from '@xyflow/react'
import { Workflow as WorkflowIcon } from 'lucide-react'
import { useEffect, useState } from 'react'
import { api } from '~/lib/api'
import { StepNameInput } from './StepNameInput'
import { AsToolPanel } from './AsToolPanel'
import { DynamicHandles } from './DynamicHandles'
import { NodeDebugPanel, BreakpointMarker } from '../RunResultsContext'
import { OnErrorPolicySelect } from './OnErrorPolicySelect'
import { useWorkflowStore } from '../useWorkflowStore'

interface SchemaEntryLike {
  name: string
  type: string
  description?: string
  required?: boolean
  enum?: string[]
}

interface WorkflowSummary {
  id: string
  name: string
  input_schema?: SchemaEntryLike[]
  input_schema_json?: string
}

// inputSchemaToJSON converts the workflow's typed `input_schema`
// (SchemaEntry[]) into a JSON Schema object suitable for an
// agent tool's `input_schema` field. Keeps consumers in sync
// with the target workflow's declared contract — no more
// hand-writing JSON Schema for every sub_workflow node.
function inputSchemaToJSON(entries: SchemaEntryLike[]): Record<string, unknown> {
  const properties: Record<string, unknown> = {}
  const required: string[] = []
  for (const e of entries) {
    if (!e.name) continue
    const prop: Record<string, unknown> = {}
    switch (e.type) {
      case 'number':
        prop.type = 'number'
        break
      case 'boolean':
        prop.type = 'boolean'
        break
      case 'enum':
        prop.type = 'string'
        if (e.enum && e.enum.length > 0) prop.enum = e.enum
        break
      default:
        prop.type = 'string'
    }
    if (e.description) prop.description = e.description
    properties[e.name] = prop
    if (e.required) required.push(e.name)
  }
  const schema: Record<string, unknown> = {
    type: 'object',
    properties,
  }
  if (required.length > 0) schema.required = required
  return schema
}

/**
 * SubWorkflowNode wires an agent's `tool` edge to another workflow.
 * The workflow_id picker filters out the current workflow to make
 * direct self-reference impossible at edit time (server still
 * enforces cycle detection across nested chains).
 */
export function SubWorkflowNode({ id, data, selected }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const workflowId = (data?.workflow_id as string) ?? ''
  const currentWorkflowId = useWorkflowStore((s) => s.activeId)
  const [workflows, setWorkflows] = useState<WorkflowSummary[]>([])
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let cancelled = false
    api
      .get<WorkflowSummary[]>('/api/v1/workflows')
      .then((wfs) => {
        if (cancelled) return
        // Strip the current workflow from the picker — direct
        // self-reference is the cheapest "cycle" the server would
        // refuse, so don't even let users select it.
        const filtered = currentWorkflowId
          ? wfs.filter((w) => w.id !== currentWorkflowId)
          : wfs
        setWorkflows(filtered)
      })
      .catch(() => setWorkflows([]))
      .finally(() => !cancelled && setLoading(false))
    return () => {
      cancelled = true
    }
  }, [currentWorkflowId])

  // When the target workflow is picked, auto-fill the as_tool
  // input_schema from its declared schema. Priority:
  //   1. target.input_schema_json (raw JSON Schema)  — wins for
  //      nested / array contracts; parsed and dropped in as-is.
  //   2. target.input_schema (typed SchemaEntry[])    — converted
  //      to a flat JSON Schema (string/number/boolean/enum).
  //   3. neither → leave the as_tool schema alone (legacy /
  //      consumer overrides untouched).
  const targetWf = workflows.find((w) => w.id === workflowId)
  const targetInputSchema = targetWf?.input_schema
  const targetInputSchemaJSON = targetWf?.input_schema_json
  useEffect(() => {
    let derived: Record<string, unknown> | null = null
    if (targetInputSchemaJSON && targetInputSchemaJSON.trim()) {
      try {
        const parsed = JSON.parse(targetInputSchemaJSON)
        if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
          derived = parsed as Record<string, unknown>
        }
      } catch {
        // Bad raw schema on the target — fall through to the
        // typed path so the consumer at least gets something.
      }
    }
    if (!derived && targetInputSchema && targetInputSchema.length > 0) {
      derived = inputSchemaToJSON(targetInputSchema)
    }
    if (!derived) return
    const currentAsTool = (data?.as_tool as { input_schema?: unknown } | undefined) ?? {}
    const currentJSON = JSON.stringify(currentAsTool.input_schema ?? {})
    const derivedJSON = JSON.stringify(derived)
    if (currentJSON !== derivedJSON) {
      updateNodeData(id, {
        as_tool: { ...currentAsTool, input_schema: derived },
      })
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [JSON.stringify(targetInputSchema), targetInputSchemaJSON])

  return (
    <div className="relative min-w-[280px] h-full">
      <BreakpointMarker id={id} />
      <div className="overflow-x-hidden rounded-lg border-2 border-fuchsia-500 bg-card text-card-foreground shadow-sm h-full">
        <NodeResizer minWidth={280} minHeight={120} isVisible={selected} />
        <div className="flex items-center gap-2 px-4 py-2.5 border-b border-fuchsia-500/40">
          <WorkflowIcon className="h-4 w-4 text-fuchsia-400 shrink-0" />
          <span className="text-sm font-medium">Sub-workflow</span>
        </div>
        <StepNameInput id={id} data={data} />
        <div className="px-3 py-2">
          <p className="text-[10px] text-muted-foreground mb-1">Target workflow</p>
          <select
            className="nodrag w-full h-7 text-xs rounded border border-border bg-background px-2"
            value={workflowId}
            disabled={loading}
            onChange={(e) => updateNodeData(id, { workflow_id: e.target.value })}
          >
            <option value="">{loading ? 'Loading…' : '— pick a workflow —'}</option>
            {workflows.map((wf) => (
              <option key={wf.id} value={wf.id}>
                {wf.name || wf.id}
              </option>
            ))}
          </select>
          {workflowId && !loading && !workflows.find((w) => w.id === workflowId) && (
            <p className="mt-1 text-[10px] text-amber-500">
              Selected workflow not found in your tenant.
            </p>
          )}
        </div>
        <AsToolPanel
          nodeId={id}
          data={data as Record<string, unknown>}
          defaultName="call_sub_workflow"
        />
        <div className="px-3 py-2 border-t border-border/50">
          <OnErrorPolicySelect nodeId={id} value={(data?.on_error as string) ?? 'stop'} />
        </div>
        <NodeDebugPanel id={id} />
      </div>
      <DynamicHandles nodeId={id} nodeType="sub_workflow" data={data as Record<string, unknown>} />
    </div>
  )
}

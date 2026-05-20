import { create } from 'zustand'
import type { Node, Edge } from '@xyflow/react'

export interface CostLimits {
  max_run_usd: number
  max_daily_usd: number
}

// ApprovalChannel routes pending_approval events out-of-band when the
// gate fires. None = stored intent-to-disable.
//
// Channel-keyed `target` semantics:
//   smtp           → recipient email
//   slack_webhook  → incoming-webhook URL (URL is the secret)
//   slack_bot      → connection_id of a `slack`-typed Connection
//                    (token stays encrypted in the connection config)
//   none           → empty
export interface ApprovalChannel {
  type: 'smtp' | 'slack_webhook' | 'slack_bot' | 'none'
  target?: string
  from?: string
  // Slack channel ID or DM user ID (e.g. "C01234ABCDE", "U01234ABCDE").
  // Required for slack_bot when the connection has no default_channel.
  channel?: string
}

export interface ParamEntry {
  name: string
  type: 'string' | 'number' | 'boolean' | 'enum'
  description?: string
  default?: string
  required?: boolean
  enum?: string[]
}

export interface Workflow {
  id: string
  name: string
  params: Record<string, string>
  nodes: Node[]
  edges: Edge[]
  cost_limits?: CostLimits | null
  params_schema?: ParamEntry[]
  approval_channel?: ApprovalChannel | null
  // version is server-stamped (`$inc` on every Upsert). Newly-created
  // and duplicated workflows start at 1; clients must never set this.
  version?: number
  created_at: string
  updated_at: string
  // Trigger-routing gate. Disabled workflows drop from every trigger
  // worker's sync-tick active set (cron / RMQ / Redis-subscribe);
  // manual `Run` from the canvas still works. Backend defaults
  // missing field to true (UnmarshalJSON + boot backfill).
  enabled?: boolean
  disabled_at?: string
  disabled_reason?: string
}

export type ConnectionType = 'mongodb' | 'redis' | 'rabbitmq' | 'anthropic' | 'openai' | 'ollama' | 'slack' | 'websocket'

export interface Connection {
  id: string
  name: string
  type: ConnectionType
  config: Record<string, string>
  created_at: string
  updated_at: string
}

export type EdgePaletteType = 'start' | 'success' | 'error' | 'item' | 'tool' | 'receive'

export interface AttachingFrom {
  nodeId: string
  handleId: string
  paletteType: string
}

interface WorkflowStore {
  workflows: Workflow[]
  connections: Connection[]
  activeId: string | null
  selectedEdgeType: EdgePaletteType | null
  attachingFrom: AttachingFrom | null
  setWorkflows(wfs: Workflow[]): void
  setConnections(conns: Connection[]): void
  setActive(id: string | null): void
  setSelectedEdgeType(type: EdgePaletteType | null): void
  setAttachingFrom(af: AttachingFrom | null): void
  updateActiveGraph(nodes: Node[], edges: Edge[], params: Record<string, string>): void
  updateActiveCostLimits(cost_limits: CostLimits | null): void
  updateActiveParamsSchema(params_schema: ParamEntry[]): void
  updateActiveApprovalChannel(approval_channel: ApprovalChannel | null): void
  activeWorkflow(): Workflow | null
}

export const useWorkflowStore = create<WorkflowStore>((set, get) => ({
  workflows: [],
  connections: [],
  activeId: null,
  selectedEdgeType: null,
  attachingFrom: null,

  setWorkflows(wfs) {
    set({ workflows: wfs })
  },

  setConnections(conns) {
    set({ connections: conns })
  },

  setActive(id) {
    set({ activeId: id, selectedEdgeType: null, attachingFrom: null })
  },

  setSelectedEdgeType(type) {
    const cur = get().selectedEdgeType
    set({ selectedEdgeType: cur === type ? null : type })
  },

  setAttachingFrom(af) {
    set({ attachingFrom: af })
  },

  updateActiveGraph(nodes, edges, params) {
    const { activeId, workflows } = get()
    if (!activeId) return
    set({
      workflows: workflows.map((w) =>
        w.id === activeId ? { ...w, nodes, edges, params } : w,
      ),
    })
  },

  updateActiveCostLimits(cost_limits) {
    const { activeId, workflows } = get()
    if (!activeId) return
    set({
      workflows: workflows.map((w) =>
        w.id === activeId ? { ...w, cost_limits } : w,
      ),
    })
  },

  updateActiveApprovalChannel(approval_channel) {
    const { activeId, workflows } = get()
    if (!activeId) return
    set({
      workflows: workflows.map((w) =>
        w.id === activeId ? { ...w, approval_channel } : w,
      ),
    })
  },

  updateActiveParamsSchema(params_schema) {
    const { activeId, workflows } = get()
    if (!activeId) return
    set({
      workflows: workflows.map((w) =>
        w.id === activeId ? { ...w, params_schema } : w,
      ),
    })
  },

  activeWorkflow() {
    const { workflows, activeId } = get()
    return workflows.find((w) => w.id === activeId) ?? null
  },
}))

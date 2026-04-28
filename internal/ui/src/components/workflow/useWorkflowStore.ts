import { create } from 'zustand'
import type { Node, Edge } from '@xyflow/react'

export interface Workflow {
  id: string
  name: string
  params: Record<string, string>
  nodes: Node[]
  edges: Edge[]
  created_at: string
  updated_at: string
}

export type ConnectionType = 'mongodb' | 'redis' | 'rabbitmq' | 'polymarket' | 'schwab' | 'anthropic' | 'openai' | 'ollama'

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

  activeWorkflow() {
    const { workflows, activeId } = get()
    return workflows.find((w) => w.id === activeId) ?? null
  },
}))

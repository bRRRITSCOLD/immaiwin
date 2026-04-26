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

interface WorkflowStore {
  workflows: Workflow[]
  activeId: string | null
  setWorkflows(wfs: Workflow[]): void
  setActive(id: string | null): void
  updateActiveGraph(nodes: Node[], edges: Edge[], params: Record<string, string>): void
  activeWorkflow(): Workflow | null
}

export const useWorkflowStore = create<WorkflowStore>((set, get) => ({
  workflows: [],
  activeId: null,

  setWorkflows(wfs) {
    set({ workflows: wfs })
  },

  setActive(id) {
    set({ activeId: id })
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

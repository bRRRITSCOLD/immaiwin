import { useReactFlow, type NodeProps } from '@xyflow/react'

export function StepNameInput({ id, data }: { id: string; data: NodeProps['data'] }) {
  const { updateNodeData } = useReactFlow()
  return (
    <input
      className="nodrag w-full px-4 py-1 text-[10px] text-muted-foreground bg-transparent
                 border-b border-border/20 focus:border-border/60 outline-none placeholder:italic"
      placeholder="step name (optional) — see legend ↙ for context access patterns"
      value={(data?.name as string) ?? ''}
      onChange={(e) => updateNodeData(id, { name: e.target.value })}
    />
  )
}

import { WorkflowInputSchemaPanel } from './WorkflowInputSchemaPanel'
import type { SchemaEntry } from './useWorkflowStore'

/**
 * Output schema editor. Reuses WorkflowInputSchemaPanel with a
 * different title + description; semantics are identical (typed
 * SchemaEntry[] for the simple path, raw JSON Schema toggle for
 * nested contracts). Single editor surface for both
 * input and output schemas — authors learn once, apply twice.
 */
interface Props {
  schema?: SchemaEntry[]
  onSchemaChange(schema: SchemaEntry[]): void
  rawSchema?: string
  onRawSchemaChange(raw: string): void
}

export function WorkflowOutputSchemaPanel(props: Props) {
  return (
    <WorkflowInputSchemaPanel
      {...props}
      title="Output schema"
      description="Declare the shape of value the workflow's `return` node produces. Sub-workflow consumers see this contract; the engine validates the resolved return payload against it. Empty = no contract enforcement."
    />
  )
}

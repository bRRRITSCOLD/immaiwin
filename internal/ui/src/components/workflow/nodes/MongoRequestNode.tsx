import { NodeResizer, type NodeProps, useReactFlow } from '@xyflow/react'
import { Database, ChevronDown, ChevronRight } from 'lucide-react'
import { useEffect, useState } from 'react'
import { useForm } from '@tanstack/react-form'
import { z } from 'zod'
import { Input } from '~/components/ui/input'
import { Textarea } from '~/components/ui/textarea'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '~/components/ui/select'
import { Switch } from '~/components/ui/switch'
import { Field, FieldDescription, FieldError, FieldLabel } from '~/components/ui/field'
import { StepNameInput } from './StepNameInput'
import { DynamicHandles } from './DynamicHandles'
import { AsToolPanel } from './AsToolPanel'
import { ConnectionPicker } from './ConnectionPicker'
import { NodeDebugPanel, BreakpointMarker, ApprovalMarker } from '../RunResultsContext'
import { OutputTransformPanel } from './OutputTransformPanel'
import { OnErrorPolicySelect } from './OnErrorPolicySelect'

// ── operation list ───────────────────────────────────────────────────────────

const OPERATIONS = [
  'find',
  'find_one_and_update',
  'find_one_and_replace',
  'insert_one',
  'insert_many',
  'update_many',
  'delete_one',
  'delete_many',
  'aggregate',
  'count_documents',
  'distinct',
  'cursor_fetch',
] as const
type Operation = (typeof OPERATIONS)[number]

// ── tool schema (consumed by AsToolPanel default) ────────────────────────────

const MONGO_REQUEST_TOOL_SCHEMA = {
  type: 'object',
  required: ['operation'],
  properties: {
    operation: { type: 'string', enum: [...OPERATIONS] },
    collection: { type: 'string', description: 'Required for every op except cursor_fetch.' },
    connection_id: { type: 'string' },
    filter: { type: 'object' },
    projection: { type: 'object' },
    sort: { type: 'object' },
    skip: { type: 'integer' },
    limit: { type: 'integer' },
    batch_size: { type: 'integer', default: 100 },
    update: { type: 'object' },
    replacement: { type: 'object' },
    upsert: { type: 'boolean', default: false },
    return_document: { type: 'string', enum: ['before', 'after'] },
    array_filters: { type: 'array', items: { type: 'object' } },
    document: { type: 'object' },
    documents: { type: 'array', items: { type: 'object' } },
    ordered: { type: 'boolean' },
    pipeline: { type: 'array', items: { type: 'object' } },
    allow_disk_use: { type: 'boolean' },
    field: { type: 'string', description: 'Required for distinct.' },
    cursor_id: { type: 'string', description: 'Required for cursor_fetch.' },
  },
}

// ── zod schemas ──────────────────────────────────────────────────────────────

const operationSchema = z.enum(OPERATIONS)

const jsonObjectSchema = z
  .string()
  .optional()
  .refine((v) => {
    if (!v || v.trim() === '' || v.trim() === '{}') return true
    try {
      const parsed = JSON.parse(v)
      return parsed && typeof parsed === 'object' && !Array.isArray(parsed)
    } catch {
      return false
    }
  }, 'Must be a JSON object')

const jsonArraySchema = z
  .string()
  .optional()
  .refine((v) => {
    if (!v || v.trim() === '' || v.trim() === '[]') return true
    try {
      const parsed = JSON.parse(v)
      return Array.isArray(parsed)
    } catch {
      return false
    }
  }, 'Must be a JSON array')

const intSchema = z.number().int()

// ── helpers ──────────────────────────────────────────────────────────────────

function jsonText(v: unknown): string {
  if (v === undefined || v === null) return ''
  try {
    return JSON.stringify(v, null, 2)
  } catch {
    return ''
  }
}

function parseJsonObject(text: string | undefined): Record<string, unknown> | undefined {
  if (!text || text.trim() === '' || text.trim() === '{}') return undefined
  try {
    const v = JSON.parse(text)
    return v && typeof v === 'object' && !Array.isArray(v) ? v : undefined
  } catch {
    return undefined
  }
}

function parseJsonArray(text: string | undefined): unknown[] | undefined {
  if (!text || text.trim() === '' || text.trim() === '[]') return undefined
  try {
    const v = JSON.parse(text)
    return Array.isArray(v) ? v : undefined
  } catch {
    return undefined
  }
}

interface FormValues {
  operation: Operation
  collection: string
  filterText: string
  projectionText: string
  sortText: string
  skip: number
  limit: number
  batch_size: number
  updateText: string
  replacementText: string
  upsert: boolean
  return_document: 'before' | 'after' | ''
  arrayFiltersText: string
  documentText: string
  documentsText: string
  ordered: boolean
  pipelineText: string
  allow_disk_use: boolean
  field: string
  cursor_id: string
}

function valuesFromData(d: Record<string, unknown>): FormValues {
  return {
    operation: ((d.operation as string) ?? 'find') as Operation,
    collection: (d.collection as string) ?? '',
    filterText: jsonText(d.filter),
    projectionText: jsonText(d.projection),
    sortText: jsonText(d.sort),
    skip: (d.skip as number) ?? 0,
    limit: (d.limit as number) ?? 0,
    batch_size: (d.batch_size as number) ?? 100,
    updateText: jsonText(d.update),
    replacementText: jsonText(d.replacement),
    upsert: Boolean(d.upsert),
    return_document: ((d.return_document as string) ?? '') as 'before' | 'after' | '',
    arrayFiltersText: jsonText(d.array_filters),
    documentText: jsonText(d.document),
    documentsText: jsonText(d.documents),
    ordered: Boolean(d.ordered),
    pipelineText: jsonText(d.pipeline),
    allow_disk_use: Boolean(d.allow_disk_use),
    field: (d.field as string) ?? '',
    cursor_id: (d.cursor_id as string) ?? '',
  }
}

function valuesToData(v: FormValues): Record<string, unknown> {
  return {
    operation: v.operation,
    collection: v.collection || undefined,
    filter: parseJsonObject(v.filterText),
    projection: parseJsonObject(v.projectionText),
    sort: parseJsonObject(v.sortText),
    skip: v.skip || undefined,
    limit: v.limit || undefined,
    batch_size: v.batch_size,
    update: parseJsonObject(v.updateText),
    replacement: parseJsonObject(v.replacementText),
    upsert: v.upsert || undefined,
    return_document: v.return_document || undefined,
    array_filters: parseJsonArray(v.arrayFiltersText),
    document: parseJsonObject(v.documentText),
    documents: parseJsonArray(v.documentsText),
    ordered: v.ordered || undefined,
    pipeline: parseJsonArray(v.pipelineText),
    allow_disk_use: v.allow_disk_use || undefined,
    field: v.field || undefined,
    cursor_id: v.cursor_id || undefined,
  }
}

// Each op declares which collapsible sections to render.
const OP_SECTIONS: Record<Operation, ReadonlySet<string>> = {
  find: new Set(['query', 'pagination']),
  find_one_and_update: new Set(['query', 'update', 'find_and_modify']),
  find_one_and_replace: new Set(['query', 'replace', 'find_and_modify']),
  insert_one: new Set(['document']),
  insert_many: new Set(['documents']),
  update_many: new Set(['query', 'update']),
  delete_one: new Set(['query']),
  delete_many: new Set(['query']),
  aggregate: new Set(['pipeline']),
  count_documents: new Set(['query', 'pagination']),
  distinct: new Set(['distinct', 'query']),
  cursor_fetch: new Set(['cursor']),
}

function showSection(op: Operation, section: string): boolean {
  return OP_SECTIONS[op].has(section)
}

// ── component ────────────────────────────────────────────────────────────────

export function MongoRequestNode({ id, data, selected }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const d = (data ?? {}) as Record<string, unknown>

  const form = useForm({
    defaultValues: valuesFromData(d),
    onSubmit: ({ value }) => {
      updateNodeData(id, valuesToData(value))
    },
  })

  // Mirror form changes back to React Flow node data.
  useEffect(() => {
    const sub: unknown = form.store.subscribe(() => {
      updateNodeData(id, valuesToData(form.state.values))
    })
    return () => {
      if (typeof sub === 'function') (sub as () => void)()
      else if (sub && typeof (sub as { unsubscribe?: () => void }).unsubscribe === 'function') {
        ;(sub as { unsubscribe: () => void }).unsubscribe()
      }
    }
  }, [form, id, updateNodeData])

  const [openQuery, setOpenQuery] = useState(true)
  const [openUpdate, setOpenUpdate] = useState(true)
  const [openReplace, setOpenReplace] = useState(true)
  const [openFindMod, setOpenFindMod] = useState(false)
  const [openDocument, setOpenDocument] = useState(true)
  const [openDocuments, setOpenDocuments] = useState(true)
  const [openPipeline, setOpenPipeline] = useState(true)
  const [openPagination, setOpenPagination] = useState(false)
  const [openDistinct, setOpenDistinct] = useState(true)
  const [openCursor, setOpenCursor] = useState(true)
  const requireApproval = (data?.require_node_approval as boolean) ?? false

  return (
    <div className="relative min-w-[320px] h-full">
      <BreakpointMarker id={id} />
      <ApprovalMarker id={id} enabled={requireApproval} onToggle={(_, next) => updateNodeData(id, { require_node_approval: next })} />
      <div className="overflow-x-hidden rounded-lg border-2 border-green-600 bg-card text-card-foreground shadow-sm h-full">
        <NodeResizer minWidth={320} minHeight={80} isVisible={selected} />
        <div className="flex items-center gap-2 px-4 py-2.5 border-b border-green-600/40">
          <Database className="h-4 w-4 text-green-500 shrink-0" />
          <span className="text-sm font-medium flex-1">Mongo Request</span>
          <ConnectionPicker nodeId={id} connectionType="mongodb" data={data as Record<string, unknown>} requireExplicit />
        </div>
        <StepNameInput id={id} data={data} />

        <form
          onSubmit={(e) => {
            e.preventDefault()
            form.handleSubmit()
          }}
          className="text-xs"
        >
          <div className="px-3 py-2 space-y-2">
            <div className="flex gap-1.5">
              <form.Field
                name="operation"
                validators={{
                  onChange: ({ value }) =>
                    operationSchema.safeParse(value).success ? undefined : 'Invalid operation',
                }}
              >
                {(field) => (
                  <Select value={field.state.value} onValueChange={(v) => field.handleChange(v as Operation)}>
                    <SelectTrigger className="nodrag h-7 w-[180px] text-xs">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {OPERATIONS.map((o) => (
                        <SelectItem key={o} value={o} className="text-xs">
                          {o}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )}
              </form.Field>
              <form.Subscribe selector={(s) => s.values.operation}>
                {(op) =>
                  op === 'cursor_fetch' ? null : (
                    <form.Field name="collection">
                      {(field) => (
                        <div className="flex-1">
                          <Input
                            id={field.name}
                            className="nodrag h-7 text-xs"
                            placeholder="collection name"
                            value={field.state.value}
                            onBlur={field.handleBlur}
                            onChange={(e) => field.handleChange(e.target.value)}
                          />
                        </div>
                      )}
                    </form.Field>
                  )
                }
              </form.Subscribe>
            </div>
            <FieldDescription>
              Inputs accept JSON. String fields support <code className="text-[10px]">{'{{…}}'}</code> templates.
            </FieldDescription>
          </div>

          <form.Subscribe selector={(s) => s.values.operation}>
            {(op) => (
              <>
                {showSection(op, 'query') && (
                  <CollapsibleSection label="Query" open={openQuery} onToggle={() => setOpenQuery((v) => !v)}>
                    <JsonField form={form} name="filterText" placeholder='{ "active": true }' label="filter" />
                    {(op === 'find' || op === 'find_one_and_update' || op === 'find_one_and_replace') && (
                      <>
                        <JsonField form={form} name="projectionText" placeholder='{ "_id": 0 }' label="projection" />
                        <JsonField form={form} name="sortText" placeholder='{ "created_at": -1 }' label="sort" />
                      </>
                    )}
                  </CollapsibleSection>
                )}

                {showSection(op, 'update') && (
                  <CollapsibleSection label="Update" open={openUpdate} onToggle={() => setOpenUpdate((v) => !v)}>
                    <JsonField form={form} name="updateText" placeholder='{ "$set": { "field": "value" } }' label="update" />
                    <JsonField form={form} name="arrayFiltersText" placeholder='[{ "elem.id": 1 }]' label="array_filters" array />
                    <SwitchFieldRow form={form} name="upsert" label="upsert" />
                  </CollapsibleSection>
                )}

                {showSection(op, 'replace') && (
                  <CollapsibleSection label="Replacement" open={openReplace} onToggle={() => setOpenReplace((v) => !v)}>
                    <JsonField form={form} name="replacementText" placeholder='{ "field": "value" }' label="replacement" />
                    <SwitchFieldRow form={form} name="upsert" label="upsert" />
                  </CollapsibleSection>
                )}

                {showSection(op, 'find_and_modify') && (
                  <CollapsibleSection label="Return doc" open={openFindMod} onToggle={() => setOpenFindMod((v) => !v)}>
                    <form.Field name="return_document">
                      {(field) => (
                        <Field>
                          <FieldLabel htmlFor={field.name}>return_document</FieldLabel>
                          <Select
                            value={field.state.value}
                            onValueChange={(v) => field.handleChange(v as 'before' | 'after' | '')}
                          >
                            <SelectTrigger className="nodrag h-7 text-xs">
                              <SelectValue placeholder="(driver default)" />
                            </SelectTrigger>
                            <SelectContent>
                              <SelectItem value="before" className="text-xs">before</SelectItem>
                              <SelectItem value="after" className="text-xs">after</SelectItem>
                            </SelectContent>
                          </Select>
                        </Field>
                      )}
                    </form.Field>
                  </CollapsibleSection>
                )}

                {showSection(op, 'document') && (
                  <CollapsibleSection label="Document" open={openDocument} onToggle={() => setOpenDocument((v) => !v)}>
                    <JsonField
                      form={form}
                      name="documentText"
                      placeholder='{ "name": "alice" }'
                      label="document"
                      description="If empty, the upstream node's input is used as the document."
                    />
                  </CollapsibleSection>
                )}

                {showSection(op, 'documents') && (
                  <CollapsibleSection label="Documents" open={openDocuments} onToggle={() => setOpenDocuments((v) => !v)}>
                    <JsonField
                      form={form}
                      name="documentsText"
                      placeholder='[{ "name": "alice" }, { "name": "bob" }]'
                      label="documents"
                      array
                      description="If empty, the upstream node's input is used as the array."
                    />
                    <SwitchFieldRow form={form} name="ordered" label="ordered" />
                  </CollapsibleSection>
                )}

                {showSection(op, 'pipeline') && (
                  <CollapsibleSection label="Pipeline" open={openPipeline} onToggle={() => setOpenPipeline((v) => !v)}>
                    <JsonField
                      form={form}
                      name="pipelineText"
                      placeholder='[{ "$match": { "active": true } }, { "$group": { "_id": "$type", "n": { "$sum": 1 } } }]'
                      label="pipeline"
                      array
                      description="If empty, the upstream node's input is used as the pipeline."
                    />
                    <SwitchFieldRow form={form} name="allow_disk_use" label="allow_disk_use" />
                    <NumberFieldRow form={form} name="batch_size" label="batch_size" />
                  </CollapsibleSection>
                )}

                {showSection(op, 'pagination') && (
                  <CollapsibleSection label="Pagination" open={openPagination} onToggle={() => setOpenPagination((v) => !v)}>
                    <NumberFieldRow form={form} name="skip" label="skip" />
                    <NumberFieldRow form={form} name="limit" label="limit" />
                    {op === 'find' && <NumberFieldRow form={form} name="batch_size" label="batch_size" />}
                  </CollapsibleSection>
                )}

                {showSection(op, 'distinct') && (
                  <CollapsibleSection label="Distinct field" open={openDistinct} onToggle={() => setOpenDistinct((v) => !v)}>
                    <form.Field name="field">
                      {(field) => (
                        <Field>
                          <FieldLabel htmlFor={field.name}>field</FieldLabel>
                          <Input
                            id={field.name}
                            className="nodrag h-7 text-xs"
                            placeholder="status"
                            value={field.state.value}
                            onChange={(e) => field.handleChange(e.target.value)}
                          />
                        </Field>
                      )}
                    </form.Field>
                  </CollapsibleSection>
                )}

                {showSection(op, 'cursor') && (
                  <CollapsibleSection label="Cursor" open={openCursor} onToggle={() => setOpenCursor((v) => !v)}>
                    <form.Field name="cursor_id">
                      {(field) => (
                        <Field>
                          <FieldLabel htmlFor={field.name}>cursor_id</FieldLabel>
                          <Input
                            id={field.name}
                            className="nodrag h-7 text-xs font-mono"
                            placeholder="{{context.previous.output.cursor.id}}"
                            value={field.state.value}
                            onChange={(e) => field.handleChange(e.target.value)}
                          />
                        </Field>
                      )}
                    </form.Field>
                    <NumberFieldRow form={form} name="batch_size" label="batch_size" />
                  </CollapsibleSection>
                )}
              </>
            )}
          </form.Subscribe>
        </form>

        <AsToolPanel
          nodeId={id}
          data={data as Record<string, unknown>}
          defaultName="mongo_request"
          defaultSchema={MONGO_REQUEST_TOOL_SCHEMA}
        />
        <div className="px-3 py-2 border-t border-border/50">
          <OnErrorPolicySelect nodeId={id} value={(data?.on_error as string) ?? 'stop'} />
        </div>
        <OutputTransformPanel nodeId={id} data={data as Record<string, unknown>} />
        <NodeDebugPanel id={id} />
      </div>
      <DynamicHandles nodeId={id} nodeType="mongo_request" data={data as Record<string, unknown>} />
    </div>
  )
}

// ── small helpers ────────────────────────────────────────────────────────────

function CollapsibleSection({
  label,
  open,
  onToggle,
  children,
}: {
  label: string
  open: boolean
  onToggle: () => void
  children: React.ReactNode
}) {
  return (
    <div className="border-t border-border/50">
      <button
        type="button"
        onClick={onToggle}
        className="nodrag flex w-full items-center justify-between px-3 py-1.5 text-[10px] font-medium text-muted-foreground hover:text-foreground"
      >
        {label}
        {open ? <ChevronDown className="h-3 w-3" /> : <ChevronRight className="h-3 w-3" />}
      </button>
      {open && <div className="px-3 pb-2 space-y-1.5">{children}</div>}
    </div>
  )
}

interface FormFieldProps {
  // form is intentionally typed as `any` — TanStack form's full generic for
  // these field-rendering helpers is unwieldy and adds no value here since
  // `name` is constrained by FormValues.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  form: any
  name: keyof FormValues
  label: string
}

function JsonField({
  form,
  name,
  placeholder,
  label,
  array,
  description,
}: FormFieldProps & { placeholder: string; array?: boolean; description?: string }) {
  const schema = array ? jsonArraySchema : jsonObjectSchema
  return (
    <form.Field
      name={name}
      validators={{
        onChange: ({ value }: { value: string | undefined }) => {
          const r = schema.safeParse(value)
          return r.success ? undefined : r.error.issues[0]?.message
        },
      }}
    >
      {(field: {
        name: string
        state: { value: string; meta: { errors: unknown[] } }
        handleChange: (v: string) => void
        handleBlur: () => void
      }) => {
        const invalid = field.state.meta.errors.length > 0
        return (
          <Field>
            <FieldLabel htmlFor={field.name}>{label}</FieldLabel>
            {description && <FieldDescription>{description}</FieldDescription>}
            <Textarea
              id={field.name}
              className="nodrag font-mono text-[10px] min-h-[60px] resize-y"
              rows={3}
              placeholder={placeholder}
              value={field.state.value}
              onChange={(e) => field.handleChange(e.target.value)}
              onBlur={field.handleBlur}
              aria-invalid={invalid}
            />
            {invalid && <FieldError errors={field.state.meta.errors as string[]} />}
          </Field>
        )
      }}
    </form.Field>
  )
}

function NumberFieldRow({ form, name, label }: FormFieldProps) {
  return (
    <form.Field
      name={name}
      validators={{
        onChange: ({ value }: { value: number }) =>
          intSchema.safeParse(value).success ? undefined : 'Must be an integer',
      }}
    >
      {(field: {
        name: string
        state: { value: number; meta: { errors: unknown[] } }
        handleChange: (v: number) => void
      }) => (
        <Field>
          <FieldLabel htmlFor={field.name}>{label}</FieldLabel>
          <Input
            id={field.name}
            type="number"
            className="nodrag h-7 text-xs"
            value={field.state.value}
            onChange={(e) => {
              const n = Number(e.target.value)
              if (!Number.isNaN(n)) field.handleChange(n)
            }}
          />
        </Field>
      )}
    </form.Field>
  )
}

function SwitchFieldRow({ form, name, label }: FormFieldProps) {
  return (
    <form.Field name={name}>
      {(field: { state: { value: boolean }; handleChange: (v: boolean) => void }) => (
        <label className="nodrag flex items-center justify-between gap-2 text-[10px]">
          <span>{label}</span>
          <Switch checked={field.state.value} onCheckedChange={(v) => field.handleChange(v)} />
        </label>
      )}
    </form.Field>
  )
}

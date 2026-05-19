import { NodeResizer, type NodeProps, useReactFlow } from '@xyflow/react'
import { Radio, ChevronDown, ChevronRight } from 'lucide-react'
import { useEffect, useState } from 'react'
import { useForm } from '@tanstack/react-form'
import { z } from 'zod'
import { Input } from '~/components/ui/input'
import { Textarea } from '~/components/ui/textarea'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '~/components/ui/select'
import { Field, FieldDescription, FieldError, FieldLabel } from '~/components/ui/field'
import { StepNameInput } from './StepNameInput'
import { DynamicHandles } from './DynamicHandles'
import { AsToolPanel } from './AsToolPanel'
import { ConnectionPicker } from './ConnectionPicker'
import { NodeDebugPanel, BreakpointMarker, ApprovalMarker } from '../RunResultsContext'
import { OnErrorPolicySelect } from './OnErrorPolicySelect'

const OPERATIONS = [
  'publish',
  'get',
  'set',
  'del',
  'incr',
  'decr',
  'expire',
  'ttl',
  'exists',
  'keys',
  'mget',
  'mset',
  'hget',
  'hset',
  'hgetall',
  'hdel',
  'lpush',
  'rpush',
  'lpop',
  'rpop',
  'lrange',
  'llen',
  'sadd',
  'srem',
  'smembers',
  'sismember',
  'zadd',
  'zrem',
  'zrange',
  'zscore',
  'zincrby',
  'xadd',
  'xrange',
  'xlen',
] as const
type Operation = (typeof OPERATIONS)[number]

const REDIS_REQUEST_TOOL_SCHEMA = {
  type: 'object',
  required: ['operation'],
  properties: {
    operation: { type: 'string', enum: [...OPERATIONS] },
    connection_id: { type: 'string' },
    key: { type: 'string' },
    keys: { type: 'array', items: { type: 'string' } },
    field: { type: 'string' },
    fields: { type: 'array', items: { type: 'string' } },
    value: { type: 'string' },
    values: { type: 'array' },
    pairs: { type: 'object', additionalProperties: { type: 'string' } },
    member: { type: 'string' },
    members: { description: 'Array of members (most ops) or map[member]score for zadd.' },
    ttl_seconds: { type: 'number' },
    pattern: { type: 'string' },
    channel: { type: 'string' },
    payload: { type: 'string' },
    start: {},
    stop: {},
    increment: { type: 'number' },
    stream: { type: 'string' },
  },
}

// ── zod ──────────────────────────────────────────────────────────────────────

const operationSchema = z.enum(OPERATIONS)
const intSchema = z.number().int()

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

  // common scalars
  key: string
  field: string
  member: string
  channel: string
  pattern: string
  stream: string
  value: string
  payload: string

  // numeric
  ttl_seconds: number
  start: number
  stop: number
  increment: number

  // JSON-text (multiple lines)
  keysText: string
  fieldsText: string
  valuesText: string
  membersText: string
  pairsText: string

  // start/stop string variants for streams
  xstart: string
  xstop: string
}

function valuesFromData(d: Record<string, unknown>): FormValues {
  return {
    operation: ((d.operation as string) ?? 'publish') as Operation,
    key: (d.key as string) ?? '',
    field: (d.field as string) ?? '',
    member: (d.member as string) ?? '',
    channel: (d.channel as string) ?? '',
    pattern: (d.pattern as string) ?? '',
    stream: (d.stream as string) ?? '',
    value: (d.value as string) ?? '',
    payload: (d.payload as string) ?? '',
    ttl_seconds: (d.ttl_seconds as number) ?? 0,
    start: typeof d.start === 'number' ? d.start : 0,
    stop: typeof d.stop === 'number' ? d.stop : -1,
    increment: (d.increment as number) ?? 0,
    keysText: jsonText(d.keys),
    fieldsText: jsonText(d.fields),
    valuesText: jsonText(d.values),
    membersText: jsonText(d.members),
    pairsText: jsonText(d.pairs),
    xstart: typeof d.start === 'string' ? d.start : '',
    xstop: typeof d.stop === 'string' ? d.stop : '',
  }
}

// Streams reuse start/stop as strings ('-' / '+'); list/zset use ints.
const STREAM_OPS = new Set<Operation>(['xrange'])

function valuesToData(v: FormValues): Record<string, unknown> {
  const isStream = STREAM_OPS.has(v.operation)
  return {
    operation: v.operation,
    key: v.key || undefined,
    field: v.field || undefined,
    member: v.member || undefined,
    channel: v.channel || undefined,
    pattern: v.pattern || undefined,
    stream: v.stream || undefined,
    value: v.value || undefined,
    payload: v.payload || undefined,
    ttl_seconds: v.ttl_seconds || undefined,
    increment: v.increment || undefined,
    keys: parseJsonArray(v.keysText),
    fields: parseJsonArray(v.fieldsText),
    values: parseJsonArray(v.valuesText) ?? parseJsonObject(v.valuesText),
    members: parseJsonArray(v.membersText) ?? parseJsonObject(v.membersText),
    pairs: parseJsonObject(v.pairsText),
    start: isStream ? v.xstart || undefined : v.start,
    stop: isStream ? v.xstop || undefined : v.stop,
  }
}

// Section visibility per operation. Each op declares which sections to show.
const OP_SECTIONS: Record<Operation, ReadonlySet<string>> = {
  publish: new Set(['publish']),
  get: new Set(['key']),
  set: new Set(['key', 'set']),
  del: new Set(['keys']),
  incr: new Set(['key']),
  decr: new Set(['key']),
  expire: new Set(['key', 'ttl']),
  ttl: new Set(['key']),
  exists: new Set(['keys']),
  keys: new Set(['pattern']),
  mget: new Set(['keys']),
  mset: new Set(['pairs']),
  hget: new Set(['key', 'field']),
  hset: new Set(['key', 'fields_kv']),
  hgetall: new Set(['key']),
  hdel: new Set(['key', 'fields']),
  lpush: new Set(['key', 'values']),
  rpush: new Set(['key', 'values']),
  lpop: new Set(['key']),
  rpop: new Set(['key']),
  lrange: new Set(['key', 'range']),
  llen: new Set(['key']),
  sadd: new Set(['key', 'members']),
  srem: new Set(['key', 'members']),
  smembers: new Set(['key']),
  sismember: new Set(['key', 'member']),
  zadd: new Set(['key', 'zmembers']),
  zrem: new Set(['key', 'members']),
  zrange: new Set(['key', 'range']),
  zscore: new Set(['key', 'member']),
  zincrby: new Set(['key', 'member', 'increment']),
  xadd: new Set(['stream', 'xvalues']),
  xrange: new Set(['stream', 'xrange']),
  xlen: new Set(['stream']),
}

function showSection(op: Operation, section: string): boolean {
  return OP_SECTIONS[op].has(section)
}

// ── component ────────────────────────────────────────────────────────────────

export function RedisRequestNode({ id, data, selected }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const d = (data ?? {}) as Record<string, unknown>

  const form = useForm({
    defaultValues: valuesFromData(d),
    onSubmit: ({ value }) => {
      updateNodeData(id, valuesToData(value))
    },
  })

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

  const [openMain, setOpenMain] = useState(true)
  const requireApproval = (data?.require_node_approval as boolean) ?? false

  return (
    <div className="relative min-w-[300px] h-full">
      <BreakpointMarker id={id} />
      <ApprovalMarker id={id} enabled={requireApproval} onToggle={(_, next) => updateNodeData(id, { require_node_approval: next })} />
      <div className="overflow-x-hidden rounded-lg border-2 border-orange-500 bg-card text-card-foreground shadow-sm h-full">
        <NodeResizer minWidth={300} minHeight={80} isVisible={selected} />
        <div className="flex items-center gap-2 px-4 py-2.5 border-b border-orange-500/40">
          <Radio className="h-4 w-4 text-orange-400 shrink-0" />
          <span className="text-sm font-medium flex-1">Redis Request</span>
          <ConnectionPicker nodeId={id} connectionType="redis" data={data as Record<string, unknown>} activeColor="text-orange-400" requireExplicit />
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
            <form.Field
              name="operation"
              validators={{
                onChange: ({ value }) =>
                  operationSchema.safeParse(value).success ? undefined : 'Invalid operation',
              }}
            >
              {(field) => (
                <Select value={field.state.value} onValueChange={(v) => field.handleChange(v as Operation)}>
                  <SelectTrigger className="nodrag h-7 w-full text-xs">
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
            <FieldDescription>
              JSON inputs accept arrays / objects. String fields support <code className="text-[10px]">{'{{…}}'}</code> templates.
            </FieldDescription>
          </div>

          <form.Subscribe selector={(s) => s.values.operation}>
            {(op) => (
              <CollapsibleSection
                label={op}
                open={openMain}
                onToggle={() => setOpenMain((v) => !v)}
              >
                {showSection(op, 'publish') && (
                  <>
                    <StringFieldRow form={form} name="channel" label="channel" placeholder="burrow:news:articles" />
                    <StringFieldRow
                      form={form}
                      name="payload"
                      label="payload"
                      placeholder="(empty → upstream input is JSON-marshalled)"
                    />
                  </>
                )}
                {showSection(op, 'key') && (
                  <StringFieldRow form={form} name="key" label="key" placeholder="my:key" />
                )}
                {showSection(op, 'keys') && (
                  <JsonField form={form} name="keysText" label="keys" placeholder='["a", "b", "c"]' array />
                )}
                {showSection(op, 'set') && (
                  <>
                    <StringFieldRow
                      form={form}
                      name="value"
                      label="value"
                      placeholder="(empty → upstream input is JSON-marshalled)"
                    />
                    <NumberFieldRow form={form} name="ttl_seconds" label="ttl_seconds" />
                  </>
                )}
                {showSection(op, 'ttl') && <NumberFieldRow form={form} name="ttl_seconds" label="ttl_seconds" />}
                {showSection(op, 'pattern') && (
                  <StringFieldRow form={form} name="pattern" label="pattern" placeholder="*" />
                )}
                {showSection(op, 'pairs') && (
                  <JsonField
                    form={form}
                    name="pairsText"
                    label="pairs"
                    placeholder='{ "key1": "v1", "key2": "v2" }'
                  />
                )}
                {showSection(op, 'field') && (
                  <StringFieldRow form={form} name="field" label="field" placeholder="status" />
                )}
                {showSection(op, 'fields') && (
                  <JsonField
                    form={form}
                    name="fieldsText"
                    label="fields"
                    placeholder='["field1", "field2"]'
                    array
                  />
                )}
                {showSection(op, 'fields_kv') && (
                  <JsonField
                    form={form}
                    name="fieldsText"
                    label="fields"
                    placeholder='{ "name": "alice", "active": "true" }'
                  />
                )}
                {showSection(op, 'values') && (
                  <JsonField
                    form={form}
                    name="valuesText"
                    label="values"
                    placeholder='["item1", "item2"]'
                    array
                  />
                )}
                {showSection(op, 'members') && (
                  <JsonField
                    form={form}
                    name="membersText"
                    label="members"
                    placeholder='["alice", "bob"]'
                    array
                  />
                )}
                {showSection(op, 'zmembers') && (
                  <JsonField
                    form={form}
                    name="membersText"
                    label="members (member → score)"
                    placeholder='{ "alice": 10, "bob": 7.5 }'
                  />
                )}
                {showSection(op, 'member') && (
                  <StringFieldRow form={form} name="member" label="member" />
                )}
                {showSection(op, 'increment') && (
                  <NumberFieldRow form={form} name="increment" label="increment" />
                )}
                {showSection(op, 'range') && (
                  <>
                    <NumberFieldRow form={form} name="start" label="start" />
                    <NumberFieldRow form={form} name="stop" label="stop" />
                  </>
                )}
                {showSection(op, 'stream') && (
                  <StringFieldRow form={form} name="stream" label="stream" placeholder="my:stream" />
                )}
                {showSection(op, 'xvalues') && (
                  <JsonField
                    form={form}
                    name="valuesText"
                    label="values (field → value)"
                    placeholder='{ "field": "value" }'
                  />
                )}
                {showSection(op, 'xrange') && (
                  <>
                    <StringFieldRow form={form} name="xstart" label="start" placeholder="-" />
                    <StringFieldRow form={form} name="xstop" label="stop" placeholder="+" />
                  </>
                )}
              </CollapsibleSection>
            )}
          </form.Subscribe>
        </form>

        <AsToolPanel
          nodeId={id}
          data={data as Record<string, unknown>}
          defaultName="redis_request"
          defaultSchema={REDIS_REQUEST_TOOL_SCHEMA}
        />
        <div className="px-3 py-2 border-t border-border/50">
          <OnErrorPolicySelect nodeId={id} value={(data?.on_error as string) ?? 'stop'} />
        </div>
        <NodeDebugPanel id={id} />
      </div>
      <DynamicHandles nodeId={id} nodeType="redis_request" data={data as Record<string, unknown>} />
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
  // form is intentionally typed as `any` — full TanStack form generics
  // add no value here since `name` is constrained by FormValues.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  form: any
  name: keyof FormValues
  label: string
}

function StringFieldRow({
  form,
  name,
  label,
  placeholder,
}: FormFieldProps & { placeholder?: string }) {
  return (
    <form.Field name={name}>
      {(field: { name: string; state: { value: string }; handleChange: (v: string) => void }) => (
        <Field>
          <FieldLabel htmlFor={field.name}>{label}</FieldLabel>
          <Input
            id={field.name}
            className="nodrag h-7 text-xs"
            placeholder={placeholder}
            value={field.state.value}
            onChange={(e) => field.handleChange(e.target.value)}
          />
        </Field>
      )}
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

function JsonField({
  form,
  name,
  label,
  placeholder,
  array,
}: FormFieldProps & { placeholder: string; array?: boolean }) {
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

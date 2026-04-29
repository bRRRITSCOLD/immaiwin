import { NodeResizer, type NodeProps, useReactFlow } from '@xyflow/react'
import { Globe, ChevronDown, ChevronRight } from 'lucide-react'
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
import { NodeDebugPanel, BreakpointMarker, ApprovalMarker } from '../RunResultsContext'

const METHODS = ['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'HEAD', 'OPTIONS'] as const
type Method = (typeof METHODS)[number]
type BodyType = 'none' | 'raw' | 'json' | 'form'

const HTTP_REQUEST_TOOL_SCHEMA = {
  type: 'object',
  properties: {
    url: { type: 'string', description: 'Absolute URL.' },
    method: { type: 'string', enum: [...METHODS], default: 'GET' },
    headers: { type: 'object', additionalProperties: { type: 'string' } },
    query: { type: 'object', additionalProperties: { type: 'string' } },
    body: { type: 'string', description: 'Raw request body.' },
    body_json: { description: 'JSON-serialised request body.' },
    body_form: { type: 'object', additionalProperties: { type: 'string' } },
    timeout_seconds: { type: 'number', default: 30 },
    follow_redirects: { type: 'boolean', default: true },
    max_redirects: { type: 'number', default: 10 },
    basic_auth_username: { type: 'string' },
    basic_auth_password: { type: 'string' },
    bearer_token: { type: 'string' },
    user_agent: { type: 'string' },
    tls_insecure_skip_verify: { type: 'boolean', default: false },
    parse_json: { type: 'boolean', default: false },
    max_response_bytes: { type: 'number', default: 10485760 },
    accept_any_status: { type: 'boolean', default: false },
  },
  required: ['url'],
}

// ── zod schemas ──────────────────────────────────────────────────────────────

const urlSchema = z
  .string()
  .min(1, 'Required')
  .refine((v) => /^\{\{.+\}\}/.test(v) || z.string().url().safeParse(v).success, 'Must be a valid URL or template')
const methodSchema = z.enum(METHODS)
const stringRecordJsonSchema = z
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
const jsonValueSchema = z
  .string()
  .optional()
  .refine((v) => {
    if (!v || v.trim() === '') return true
    try {
      JSON.parse(v)
      return true
    } catch {
      return false
    }
  }, 'Must be valid JSON')
const positiveIntSchema = z.number().int().positive()
const nonNegIntSchema = z.number().int().min(0)

// ── helpers ──────────────────────────────────────────────────────────────────

function detectBodyType(data: Record<string, unknown>): BodyType {
  if (data.body_json !== undefined && data.body_json !== null) return 'json'
  if (data.body_form && typeof data.body_form === 'object') return 'form'
  if (typeof data.body === 'string' && data.body !== '') return 'raw'
  return 'none'
}

function jsonText(v: unknown, fallback: string): string {
  if (v === undefined || v === null) return fallback
  try {
    return JSON.stringify(v, null, 2)
  } catch {
    return fallback
  }
}

function parseJsonObject(text: string | undefined): Record<string, string> | undefined {
  if (!text || text.trim() === '' || text.trim() === '{}') return undefined
  try {
    const v = JSON.parse(text)
    return v && typeof v === 'object' && !Array.isArray(v) ? v : undefined
  } catch {
    return undefined
  }
}

function parseJsonValue(text: string | undefined): unknown {
  if (!text || text.trim() === '') return undefined
  try {
    return JSON.parse(text)
  } catch {
    return undefined
  }
}

interface FormValues {
  url: string
  method: Method
  headersText: string
  queryText: string
  bodyType: BodyType
  body: string
  bodyJsonText: string
  bodyFormText: string
  timeout_seconds: number
  follow_redirects: boolean
  max_redirects: number
  bearer_token: string
  basic_auth_username: string
  basic_auth_password: string
  user_agent: string
  tls_insecure_skip_verify: boolean
  parse_json: boolean
  accept_any_status: boolean
  max_response_bytes: number
}

function valuesFromData(d: Record<string, unknown>): FormValues {
  return {
    url: (d.url as string) ?? '',
    method: (((d.method as string) ?? 'GET').toUpperCase() as Method),
    headersText: jsonText(d.headers, ''),
    queryText: jsonText(d.query, ''),
    bodyType: detectBodyType(d),
    body: (d.body as string) ?? '',
    bodyJsonText: jsonText(d.body_json, ''),
    bodyFormText: jsonText(d.body_form, ''),
    timeout_seconds: (d.timeout_seconds as number) ?? 30,
    follow_redirects: (d.follow_redirects as boolean) ?? true,
    max_redirects: (d.max_redirects as number) ?? 10,
    bearer_token: (d.bearer_token as string) ?? '',
    basic_auth_username: (d.basic_auth_username as string) ?? '',
    basic_auth_password: (d.basic_auth_password as string) ?? '',
    user_agent: (d.user_agent as string) ?? '',
    tls_insecure_skip_verify: Boolean(d.tls_insecure_skip_verify),
    parse_json: Boolean(d.parse_json),
    accept_any_status: Boolean(d.accept_any_status),
    max_response_bytes: (d.max_response_bytes as number) ?? 10485760,
  }
}

function valuesToData(v: FormValues): Record<string, unknown> {
  const headers = parseJsonObject(v.headersText)
  const query = parseJsonObject(v.queryText)
  const body = v.bodyType === 'raw' ? v.body : undefined
  const body_json = v.bodyType === 'json' ? parseJsonValue(v.bodyJsonText) : undefined
  const body_form = v.bodyType === 'form' ? parseJsonObject(v.bodyFormText) : undefined
  return {
    url: v.url,
    method: v.method,
    headers,
    query,
    body,
    body_json,
    body_form,
    timeout_seconds: v.timeout_seconds,
    follow_redirects: v.follow_redirects,
    max_redirects: v.max_redirects,
    bearer_token: v.bearer_token || undefined,
    basic_auth_username: v.basic_auth_username || undefined,
    basic_auth_password: v.basic_auth_password || undefined,
    user_agent: v.user_agent || undefined,
    tls_insecure_skip_verify: v.tls_insecure_skip_verify,
    parse_json: v.parse_json,
    accept_any_status: v.accept_any_status,
    max_response_bytes: v.max_response_bytes,
  }
}

// ── component ────────────────────────────────────────────────────────────────

export function HTTPRequestNode({ id, data, selected }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const d = (data ?? {}) as Record<string, unknown>

  const form = useForm({
    defaultValues: valuesFromData(d),
    onSubmit: ({ value }) => {
      updateNodeData(id, valuesToData(value))
    },
  })

  // Mirror form changes back to React Flow node data on every value change.
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

  const initialBodyType = form.state.values.bodyType
  const [openHeaders, setOpenHeaders] = useState(false)
  const [openQuery, setOpenQuery] = useState(false)
  const [openBody, setOpenBody] = useState(initialBodyType !== 'none')
  const [openAuth, setOpenAuth] = useState(
    Boolean(form.state.values.bearer_token || form.state.values.basic_auth_username),
  )
  const [openOpts, setOpenOpts] = useState(false)

  const requireApproval = (data?.require_node_approval as boolean) ?? false

  return (
    <div className="relative min-w-[320px] h-full">
      <BreakpointMarker id={id} />
      <ApprovalMarker id={id} enabled={requireApproval} onToggle={(_, next) => updateNodeData(id, { require_node_approval: next })} />
      <div className="overflow-x-hidden rounded-lg border-2 border-sky-400 bg-card text-card-foreground shadow-sm h-full">
        <NodeResizer minWidth={320} minHeight={80} isVisible={selected} />
        <div className="flex items-center gap-2 px-4 py-2.5 border-b border-sky-400/40">
          <Globe className="h-4 w-4 text-sky-400 shrink-0" />
          <span className="text-sm font-medium">HTTP Request</span>
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
                name="method"
                validators={{
                  onChange: ({ value }) =>
                    methodSchema.safeParse(value).success ? undefined : 'Invalid method',
                }}
              >
                {(field) => (
                  <Select value={field.state.value} onValueChange={(v) => field.handleChange(v as Method)}>
                    <SelectTrigger className="nodrag h-7 w-[88px] text-xs">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {METHODS.map((m) => (
                        <SelectItem key={m} value={m} className="text-xs">
                          {m}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )}
              </form.Field>

              <form.Field
                name="url"
                validators={{
                  onBlur: ({ value }) => {
                    const r = urlSchema.safeParse(value)
                    return r.success ? undefined : r.error.issues[0]?.message
                  },
                }}
              >
                {(field) => {
                  const invalid = field.state.meta.isTouched && field.state.meta.errors.length > 0
                  return (
                    <div className="flex-1">
                      <Input
                        id={field.name}
                        className="nodrag h-7 text-xs"
                        placeholder="https://api.example.com/v1/items"
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        aria-invalid={invalid}
                      />
                      {invalid && <FieldError errors={field.state.meta.errors as string[]} />}
                    </div>
                  )
                }}
              </form.Field>
            </div>
            <FieldDescription>
              URL/headers/body support <code className="text-[10px]">{'{{…}}'}</code> templates
            </FieldDescription>
          </div>

          <CollapsibleSection label="Headers" open={openHeaders} onToggle={() => setOpenHeaders((v) => !v)}>
            <form.Field
              name="headersText"
              validators={{
                onChange: ({ value }) => {
                  const r = stringRecordJsonSchema.safeParse(value)
                  return r.success ? undefined : r.error.issues[0]?.message
                },
              }}
            >
              {(field) => {
                const invalid = field.state.meta.errors.length > 0
                return (
                  <Field>
                    <Textarea
                      className="nodrag font-mono text-[10px] min-h-[60px] resize-y"
                      rows={3}
                      placeholder='{ "Accept": "application/json" }'
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
          </CollapsibleSection>

          <CollapsibleSection label="Query" open={openQuery} onToggle={() => setOpenQuery((v) => !v)}>
            <form.Field
              name="queryText"
              validators={{
                onChange: ({ value }) => {
                  const r = stringRecordJsonSchema.safeParse(value)
                  return r.success ? undefined : r.error.issues[0]?.message
                },
              }}
            >
              {(field) => {
                const invalid = field.state.meta.errors.length > 0
                return (
                  <Field>
                    <Textarea
                      className="nodrag font-mono text-[10px] min-h-[60px] resize-y"
                      rows={3}
                      placeholder='{ "page": "1", "limit": "20" }'
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
          </CollapsibleSection>

          <form.Subscribe selector={(s) => s.values.bodyType}>
            {(bodyType) => (
              <CollapsibleSection
                label={`Body${bodyType === 'none' ? '' : ` · ${bodyType}`}`}
                open={openBody}
                onToggle={() => setOpenBody((v) => !v)}
              >
                <form.Field name="bodyType">
                  {(field) => (
                    <div className="flex gap-1 mb-1.5">
                      {(['none', 'raw', 'json', 'form'] as BodyType[]).map((t) => (
                        <button
                          key={t}
                          type="button"
                          onClick={() => {
                            field.handleChange(t)
                            // clear stale fields so saved data matches selected type
                            form.setFieldValue('body', '')
                            form.setFieldValue('bodyJsonText', '')
                            form.setFieldValue('bodyFormText', '')
                          }}
                          className={`nodrag rounded px-2 py-0.5 text-[10px] border ${
                            bodyType === t
                              ? 'bg-sky-400/20 border-sky-400 text-foreground'
                              : 'border-border/50 text-muted-foreground hover:text-foreground'
                          }`}
                        >
                          {t}
                        </button>
                      ))}
                    </div>
                  )}
                </form.Field>

                {bodyType === 'raw' && (
                  <form.Field name="body">
                    {(field) => (
                      <Field>
                        <Textarea
                          className="nodrag font-mono text-[10px] min-h-[60px] resize-y"
                          rows={3}
                          placeholder="raw body (string)"
                          value={field.state.value}
                          onChange={(e) => field.handleChange(e.target.value)}
                        />
                      </Field>
                    )}
                  </form.Field>
                )}
                {bodyType === 'json' && (
                  <form.Field
                    name="bodyJsonText"
                    validators={{
                      onChange: ({ value }) => {
                        const r = jsonValueSchema.safeParse(value)
                        return r.success ? undefined : r.error.issues[0]?.message
                      },
                    }}
                  >
                    {(field) => {
                      const invalid = field.state.meta.errors.length > 0
                      return (
                        <Field>
                          <Textarea
                            className="nodrag font-mono text-[10px] min-h-[60px] resize-y"
                            rows={3}
                            placeholder='{ "name": "alice" }'
                            value={field.state.value}
                            onChange={(e) => field.handleChange(e.target.value)}
                            aria-invalid={invalid}
                          />
                          {invalid && <FieldError errors={field.state.meta.errors as string[]} />}
                        </Field>
                      )
                    }}
                  </form.Field>
                )}
                {bodyType === 'form' && (
                  <form.Field
                    name="bodyFormText"
                    validators={{
                      onChange: ({ value }) => {
                        const r = stringRecordJsonSchema.safeParse(value)
                        return r.success ? undefined : r.error.issues[0]?.message
                      },
                    }}
                  >
                    {(field) => {
                      const invalid = field.state.meta.errors.length > 0
                      return (
                        <Field>
                          <Textarea
                            className="nodrag font-mono text-[10px] min-h-[60px] resize-y"
                            rows={3}
                            placeholder='{ "field": "value" }'
                            value={field.state.value}
                            onChange={(e) => field.handleChange(e.target.value)}
                            aria-invalid={invalid}
                          />
                          {invalid && <FieldError errors={field.state.meta.errors as string[]} />}
                        </Field>
                      )
                    }}
                  </form.Field>
                )}
              </CollapsibleSection>
            )}
          </form.Subscribe>

          <CollapsibleSection label="Auth" open={openAuth} onToggle={() => setOpenAuth((v) => !v)}>
            <div className="space-y-1.5">
              <form.Field name="bearer_token">
                {(field) => (
                  <Field>
                    <FieldLabel htmlFor={field.name}>Bearer token</FieldLabel>
                    <Input
                      id={field.name}
                      className="nodrag h-7 text-xs"
                      placeholder="{{params.api_token}}"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                    />
                  </Field>
                )}
              </form.Field>
              <div className="flex gap-1.5">
                <form.Field name="basic_auth_username">
                  {(field) => (
                    <Field className="flex-1">
                      <FieldLabel htmlFor={field.name}>Basic user</FieldLabel>
                      <Input
                        id={field.name}
                        className="nodrag h-7 text-xs"
                        value={field.state.value}
                        onChange={(e) => field.handleChange(e.target.value)}
                      />
                    </Field>
                  )}
                </form.Field>
                <form.Field name="basic_auth_password">
                  {(field) => (
                    <Field className="flex-1">
                      <FieldLabel htmlFor={field.name}>Basic password</FieldLabel>
                      <Input
                        id={field.name}
                        type="password"
                        className="nodrag h-7 text-xs"
                        value={field.state.value}
                        onChange={(e) => field.handleChange(e.target.value)}
                      />
                    </Field>
                  )}
                </form.Field>
              </div>
            </div>
          </CollapsibleSection>

          <CollapsibleSection label="Options" open={openOpts} onToggle={() => setOpenOpts((v) => !v)}>
            <div className="grid grid-cols-2 gap-1.5">
              <form.Field
                name="timeout_seconds"
                validators={{
                  onChange: ({ value }) =>
                    positiveIntSchema.safeParse(value).success ? undefined : 'Must be a positive integer',
                }}
              >
                {(field) => (
                  <NumberFieldInput label="Timeout (s)" field={field} />
                )}
              </form.Field>
              <form.Field
                name="max_redirects"
                validators={{
                  onChange: ({ value }) =>
                    nonNegIntSchema.safeParse(value).success ? undefined : 'Must be ≥ 0',
                }}
              >
                {(field) => <NumberFieldInput label="Max redirects" field={field} />}
              </form.Field>
              <form.Field
                name="max_response_bytes"
                validators={{
                  onChange: ({ value }) =>
                    nonNegIntSchema.safeParse(value).success ? undefined : 'Must be ≥ 0',
                }}
              >
                {(field) => <NumberFieldInput label="Max body bytes" field={field} />}
              </form.Field>
              <form.Field name="user_agent">
                {(field) => (
                  <Field>
                    <FieldLabel htmlFor={field.name}>User-Agent</FieldLabel>
                    <Input
                      id={field.name}
                      className="nodrag h-7 text-xs"
                      placeholder="immaiwin/1.0"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                    />
                  </Field>
                )}
              </form.Field>
            </div>
            <div className="space-y-1 mt-2">
              <form.Field name="follow_redirects">
                {(field) => (
                  <SwitchRow
                    label="Follow redirects"
                    checked={field.state.value}
                    onChange={(v) => field.handleChange(v)}
                  />
                )}
              </form.Field>
              <form.Field name="parse_json">
                {(field) => (
                  <SwitchRow
                    label="Parse response as JSON"
                    checked={field.state.value}
                    onChange={(v) => field.handleChange(v)}
                  />
                )}
              </form.Field>
              <form.Field name="accept_any_status">
                {(field) => (
                  <SwitchRow
                    label="Accept any status (no error on non-2xx)"
                    checked={field.state.value}
                    onChange={(v) => field.handleChange(v)}
                  />
                )}
              </form.Field>
              <form.Field name="tls_insecure_skip_verify">
                {(field) => (
                  <SwitchRow
                    label="TLS skip verify"
                    checked={field.state.value}
                    onChange={(v) => field.handleChange(v)}
                  />
                )}
              </form.Field>
            </div>
          </CollapsibleSection>
        </form>

        <AsToolPanel
          nodeId={id}
          data={data as Record<string, unknown>}
          defaultName="http_request"
          defaultSchema={HTTP_REQUEST_TOOL_SCHEMA}
        />
        <NodeDebugPanel id={id} />
      </div>
      <DynamicHandles nodeId={id} nodeType="http_request" data={data as Record<string, unknown>} />
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
      {open && <div className="px-3 pb-2">{children}</div>}
    </div>
  )
}

interface NumberFieldHandle {
  name: string
  state: { value: number; meta: { errors: unknown[] } }
  handleChange: (v: number) => void
}

function NumberFieldInput({ label, field }: { label: string; field: NumberFieldHandle }) {
  const invalid = field.state.meta.errors.length > 0
  return (
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
        aria-invalid={invalid}
      />
      {invalid && <FieldError errors={field.state.meta.errors as string[]} />}
    </Field>
  )
}

function SwitchRow({
  label,
  checked,
  onChange,
}: {
  label: string
  checked: boolean
  onChange: (v: boolean) => void
}) {
  return (
    <label className="nodrag flex items-center justify-between gap-2 text-[10px]">
      <span>{label}</span>
      <Switch checked={checked} onCheckedChange={onChange} />
    </label>
  )
}

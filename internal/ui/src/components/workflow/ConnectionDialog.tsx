import { useState, useEffect, useCallback, useRef } from 'react'
import { useForm } from '@tanstack/react-form'
import { z } from 'zod'
import { toast } from 'sonner'
import { ChevronDown } from 'lucide-react'
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from '~/components/ui/dialog'
import { Input } from '~/components/ui/input'
import { Button } from '~/components/ui/button'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '~/components/ui/select'
import { Checkbox } from '~/components/ui/checkbox'
import { Separator } from '~/components/ui/separator'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '~/components/ui/field'
import type { Connection, ConnectionType } from './useWorkflowStore'

const API_BASE = import.meta.env['VITE_API_URL'] ?? 'http://localhost:8080'

// ── validators ───────────────────────────────────────────────────────────────

const nameSchema = z.string().min(1, 'Required')
const uriSchema = z.string().min(1, 'Required').url('Must be a valid URI')
const dbNameSchema = z.string().min(1, 'Required')
const hostSchema = z.string().min(1, 'Required')

// ── section helper ───────────────────────────────────────────────────────────

function Section({ title, children, defaultOpen = false }: { title: string; children: React.ReactNode; defaultOpen?: boolean }) {
  const [open, setOpen] = useState(defaultOpen)
  return (
    <div>
      <button
        type="button"
        className="flex items-center gap-1.5 w-full text-xs font-medium text-muted-foreground hover:text-foreground transition-colors py-1"
        onClick={() => setOpen((v) => !v)}
      >
        <ChevronDown className={`h-3 w-3 transition-transform ${open ? '' : '-rotate-90'}`} />
        {title}
      </button>
      {open && <FieldGroup className="pl-1 pt-2">{children}</FieldGroup>}
    </div>
  )
}

// ── component ────────────────────────────────────────────────────────────────

interface Props {
  open: boolean
  onOpenChange(open: boolean): void
  connection: Connection | null
  onSaved(): void
}

export function ConnectionDialog({ open, onOpenChange, connection, onSaved }: Props) {
  const [testing, setTesting] = useState(false)
  const [saved, setSaved] = useState(!!connection)
  const [justCreated, setJustCreated] = useState(false)
  const [connId, setConnId] = useState(() => crypto.randomUUID())

  // Determine the actual ID: editing → existing id, creating → stable generated id
  const id = connection?.id ?? connId

  const form = useForm({
    defaultValues: {
      name: connection?.name ?? '',
      type: (connection?.type ?? 'mongodb') as ConnectionType,
      config: { ...(connection?.config ?? {}) } as Record<string, string>,
    },
    onSubmit: async ({ value }) => {
      try {
        const res = await fetch(`${API_BASE}/api/v1/connections/${id}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name: value.name, type: value.type, config: stripEmpty(value.config) }),
        })
        if (!res.ok) {
          const d = await res.json()
          toast.error(`Save failed: ${d.error}`)
          return
        }
        toast.success('Connection saved')
        setSaved(true)
        if (!connection) setJustCreated(true)
        onOpenChange(false)
        onSaved()
      } catch {
        toast.error('Network error')
      }
    },
  })

  // Reset form when dialog opens with different connection
  useEffect(() => {
    form.reset()
    form.setFieldValue('name', connection?.name ?? '')
    form.setFieldValue('type', (connection?.type ?? 'mongodb') as ConnectionType)
    form.setFieldValue('config', { ...(connection?.config ?? {}) })
    setSaved(!!connection)
    setJustCreated(false)
    // Generate fresh UUID for each new connection dialog open
    if (!connection) setConnId(crypto.randomUUID())
  }, [connection, open]) // eslint-disable-line react-hooks/exhaustive-deps

  async function handleTest() {
    const { type, config } = form.state.values
    setTesting(true)
    try {
      const res = await fetch(`${API_BASE}/api/v1/connections/test`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ type, config: stripEmpty(config) }),
      })
      const result = await res.json()
      if (result.ok) toast.success('Connection successful')
      else toast.error(`Connection failed: ${result.error}`)
    } catch {
      toast.error('Network error')
    } finally {
      setTesting(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg max-h-[85vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{connection ? 'Edit Connection' : 'New Connection'}</DialogTitle>
        </DialogHeader>

        <form onSubmit={(e) => { e.preventDefault(); form.handleSubmit() }}>
          <FieldGroup className="py-2">
            {/* Name */}
            <form.Field
              name="name"
              validators={{
                onBlur: ({ value }) => {
                  const r = nameSchema.safeParse(value)
                  return r.success ? undefined : r.error.issues[0]?.message
                },
              }}
            >
              {(field) => {
                const invalid = field.state.meta.isTouched && field.state.meta.errors.length > 0
                return (
                  <Field>
                    <FieldLabel htmlFor={field.name}>Name</FieldLabel>
                    <Input
                      id={field.name}
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(e) => field.handleChange(e.target.value)}
                      aria-invalid={invalid}
                      placeholder="e.g. prod-mongo"
                      disabled={justCreated}
                    />
                    {invalid && <FieldError errors={field.state.meta.errors as string[]} />}
                  </Field>
                )
              }}
            </form.Field>

            {/* Type */}
            <form.Field name="type">
              {(field) => (
                <Field>
                  <FieldLabel>Type</FieldLabel>
                  <Select value={field.state.value} onValueChange={(v) => field.handleChange(v as ConnectionType)} disabled={justCreated}>
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="mongodb">MongoDB</SelectItem>
                      <SelectItem value="redis">Redis</SelectItem>
                      <SelectItem value="rabbitmq">RabbitMQ</SelectItem>
                      <SelectItem value="anthropic">Anthropic</SelectItem>
                      <SelectItem value="openai">OpenAI</SelectItem>
                      <SelectItem value="ollama">Ollama</SelectItem>
                    </SelectContent>
                  </Select>
                </Field>
              )}
            </form.Field>

            <Separator />

            {/* Type-specific fields */}
            <form.Subscribe selector={(s) => s.values.type}>
              {(type) => (
                <form.Field name="config">
                  {(field) => {
                    const config = field.state.value
                    const setField = (key: string, value: string) => {
                      const next = { ...config }
                      if (value === '') delete next[key]
                      else next[key] = value
                      field.handleChange(next)
                    }
                    if (type === 'mongodb') return <MongoFields config={config} setField={setField} />
                    if (type === 'redis') return <RedisFields config={config} setField={setField} />
                    if (type === 'rabbitmq') return <RabbitMQFields config={config} setField={setField} />
                    if (type === 'anthropic') return <AnthropicFields config={config} setField={setField} />
                    if (type === 'openai') return <OpenAIFields config={config} setField={setField} />
                    return <OllamaFields config={config} setField={setField} />

                  }}
                </form.Field>
              )}
            </form.Subscribe>
          </FieldGroup>

          <DialogFooter className="pt-4">
            <Button type="button" variant="outline" onClick={handleTest} disabled={testing}>
              {testing ? 'Testing…' : 'Test'}
            </Button>
            <form.Subscribe selector={(s) => s.isSubmitting}>
              {(submitting) => (
                <Button type="submit" disabled={submitting}>
                  {submitting ? 'Saving…' : 'Save'}
                </Button>
              )}
            </form.Subscribe>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

// ── helpers ──────────────────────────────────────────────────────────────────

function stripEmpty(config: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [k, v] of Object.entries(config)) {
    if (v !== '') out[k] = v
  }
  return out
}

function ConfigInput({ label, configKey, config, setField, placeholder, type = 'text', description, disabled }: {
  label: string; configKey: string; config: Record<string, string>; setField: (k: string, v: string) => void
  placeholder?: string; type?: string; description?: string; disabled?: boolean
}) {
  return (
    <Field>
      <FieldLabel className="text-xs">{label}</FieldLabel>
      <Input
        type={type}
        value={config[configKey] ?? ''}
        onChange={(e) => setField(configKey, e.target.value)}
        placeholder={placeholder}
        disabled={disabled}
      />
      {description && <FieldDescription>{description}</FieldDescription>}
    </Field>
  )
}

function ConfigCheckbox({ label, configKey, config, setField }: {
  label: string; configKey: string; config: Record<string, string>; setField: (k: string, v: string) => void
}) {
  return (
    <div className="flex items-center gap-2">
      <Checkbox
        id={configKey}
        checked={config[configKey] === 'true'}
        onCheckedChange={(v) => setField(configKey, v ? 'true' : '')}
      />
      <FieldLabel htmlFor={configKey} className="text-xs cursor-pointer">{label}</FieldLabel>
    </div>
  )
}

function ConfigSelect({ label, configKey, config, setField, placeholder, options }: {
  label: string; configKey: string; config: Record<string, string>; setField: (k: string, v: string) => void
  placeholder: string; options: { value: string; label: string }[]
}) {
  return (
    <Field>
      <FieldLabel className="text-xs">{label}</FieldLabel>
      <Select value={config[configKey] ?? ''} onValueChange={(v) => setField(configKey, v === '__none__' ? '' : v)}>
        <SelectTrigger className="text-xs">
          <SelectValue placeholder={placeholder} />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="__none__">{placeholder}</SelectItem>
          {options.map((o) => <SelectItem key={o.value} value={o.value}>{o.label}</SelectItem>)}
        </SelectContent>
      </Select>
    </Field>
  )
}

// ── MongoDB fields ───────────────────────────────────────────────────────────

function MongoFields({ config, setField }: { config: Record<string, string>; setField: (k: string, v: string) => void }) {
  return (
    <FieldGroup>
      <ConfigInput label="URI" configKey="uri" config={config} setField={setField} placeholder="mongodb://localhost:27017" />
      <ConfigInput label="Database" configKey="database" config={config} setField={setField} placeholder="immaiwin" />

      <Section title="Authentication">
        <ConfigInput label="Username" configKey="username" config={config} setField={setField} placeholder="(optional)" />
        <ConfigInput label="Password" configKey="password" config={config} setField={setField} placeholder="(optional)" type="password" />
        <ConfigInput label="Auth Source" configKey="auth_source" config={config} setField={setField} placeholder="admin" />
        <ConfigInput label="Auth Mechanism" configKey="auth_mechanism" config={config} setField={setField} placeholder="SCRAM-SHA-256" />
      </Section>

      <Section title="Connection Pool">
        <ConfigInput label="Max Pool Size" configKey="max_pool_size" config={config} setField={setField} placeholder="100" />
        <ConfigInput label="Min Pool Size" configKey="min_pool_size" config={config} setField={setField} placeholder="0" />
        <ConfigInput label="Max Connecting" configKey="max_connecting" config={config} setField={setField} placeholder="2" />
        <ConfigInput label="Max Conn Idle Time" configKey="max_conn_idle_time" config={config} setField={setField} placeholder="0s (no limit)" description="Go duration: 30s, 5m, 1h" />
      </Section>

      <Section title="Timeouts">
        <ConfigInput label="Connect Timeout" configKey="connect_timeout" config={config} setField={setField} placeholder="30s" description="Go duration" />
        <ConfigInput label="Server Selection Timeout" configKey="server_selection_timeout" config={config} setField={setField} placeholder="30s" />
        <ConfigInput label="Heartbeat Interval" configKey="heartbeat_interval" config={config} setField={setField} placeholder="10s" />
        <ConfigInput label="Timeout (global)" configKey="timeout" config={config} setField={setField} placeholder="0s (disabled)" />
      </Section>

      <Section title="Replica Set & Read/Write">
        <ConfigInput label="Replica Set" configKey="replica_set" config={config} setField={setField} placeholder="(auto-detected)" />
        <ConfigSelect label="Read Preference" configKey="read_preference" config={config} setField={setField}
          placeholder="primary (default)"
          options={[
            { value: 'primaryPreferred', label: 'primaryPreferred' },
            { value: 'secondary', label: 'secondary' },
            { value: 'secondaryPreferred', label: 'secondaryPreferred' },
            { value: 'nearest', label: 'nearest' },
          ]}
        />
        <ConfigInput label="Write Concern (w)" configKey="write_concern_w" config={config} setField={setField} placeholder="majority" />
        <ConfigCheckbox label="Direct Connection" configKey="direct" config={config} setField={setField} />
        <ConfigCheckbox label="Retry Reads" configKey="retry_reads" config={config} setField={setField} />
        <ConfigCheckbox label="Retry Writes" configKey="retry_writes" config={config} setField={setField} />
      </Section>

      <Section title="TLS">
        <ConfigCheckbox label="Enable TLS" configKey="tls" config={config} setField={setField} />
        <ConfigCheckbox label="Insecure (skip cert verify)" configKey="tls_insecure" config={config} setField={setField} />
      </Section>

      <Section title="Compression">
        <ConfigInput label="Compressors" configKey="compressors" config={config} setField={setField} placeholder="snappy,zlib,zstd" description="Comma-separated" />
        <ConfigInput label="Zlib Level" configKey="zlib_level" config={config} setField={setField} placeholder="6 (0-9)" />
        <ConfigInput label="Zstd Level" configKey="zstd_level" config={config} setField={setField} placeholder="6 (1-20)" />
      </Section>

      <Section title="Advanced">
        <ConfigInput label="App Name" configKey="app_name" config={config} setField={setField} placeholder="immaiwin" />
        <ConfigInput label="Local Threshold" configKey="local_threshold" config={config} setField={setField} placeholder="15ms" />
        <ConfigInput label="SRV Max Hosts" configKey="srv_max_hosts" config={config} setField={setField} placeholder="0 (all)" />
        <ConfigCheckbox label="Load Balanced" configKey="load_balanced" config={config} setField={setField} />
      </Section>
    </FieldGroup>
  )
}

// ── Anthropic fields ─────────────────────────────────────────────────────────

function AnthropicFields({ config, setField }: { config: Record<string, string>; setField: (k: string, v: string) => void }) {
  return (
    <FieldGroup>
      <ConfigInput label="API Key" configKey="api_key" config={config} setField={setField} placeholder="sk-ant-…" type="password" />
      <ConfigInput label="Default Model" configKey="default_model" config={config} setField={setField} placeholder="claude-opus-4-7" description="e.g. claude-opus-4-7, claude-sonnet-4-6, claude-haiku-4-5-20251001" />

      <Section title="Advanced">
        <ConfigInput label="Endpoint" configKey="endpoint" config={config} setField={setField} placeholder="https://api.anthropic.com" description="Override base URL (gateways/proxies)" />
        <ConfigInput label="API Version" configKey="version" config={config} setField={setField} placeholder="2023-06-01" />
        <ConfigInput label="Timeout" configKey="timeout" config={config} setField={setField} placeholder="60s" description="Go duration: 30s, 5m, 1h" />
      </Section>
    </FieldGroup>
  )
}

// ── OpenAI fields ────────────────────────────────────────────────────────────

function OpenAIFields({ config, setField }: { config: Record<string, string>; setField: (k: string, v: string) => void }) {
  return (
    <FieldGroup>
      <ConfigInput label="API Key" configKey="api_key" config={config} setField={setField} placeholder="sk-…" type="password" />
      <ConfigInput label="Default Model" configKey="default_model" config={config} setField={setField} placeholder="gpt-4o-mini" description="e.g. gpt-4o, gpt-4o-mini, gpt-4-turbo, gpt-3.5-turbo" />

      <Section title="Advanced">
        <ConfigInput label="Endpoint" configKey="endpoint" config={config} setField={setField} placeholder="https://api.openai.com" description="Override base URL (Azure / proxies)" />
        <ConfigInput label="Organization" configKey="organization" config={config} setField={setField} placeholder="org-…" description="Sent as OpenAI-Organization header" />
        <ConfigInput label="Project" configKey="project" config={config} setField={setField} placeholder="proj_…" description="Sent as OpenAI-Project header" />
        <ConfigInput label="Timeout" configKey="timeout" config={config} setField={setField} placeholder="60s" description="Go duration: 30s, 5m, 1h" />
      </Section>
    </FieldGroup>
  )
}

// ── Ollama fields ────────────────────────────────────────────────────────────

function OllamaFields({ config, setField }: { config: Record<string, string>; setField: (k: string, v: string) => void }) {
  return (
    <FieldGroup>
      <ConfigInput label="Endpoint" configKey="endpoint" config={config} setField={setField} placeholder="http://localhost:11434" description="Local Ollama server URL" />
      <ConfigInput label="Default Model" configKey="default_model" config={config} setField={setField} placeholder="llama3.1" description="Tool-capable models: llama3.1, qwen2.5, mistral-nemo. Tag with :7b/:70b etc." />

      <Section title="Advanced">
        <ConfigInput label="Keep Alive" configKey="keep_alive" config={config} setField={setField} placeholder="5m" description="How long Ollama keeps the model warm in VRAM" />
        <ConfigInput label="Timeout" configKey="timeout" config={config} setField={setField} placeholder="10m" description="HTTP timeout — large models cold-start may exceed 60s" />
      </Section>
    </FieldGroup>
  )
}

// ── RabbitMQ fields ──────────────────────────────────────────────────────────

function RabbitMQFields({ config, setField }: { config: Record<string, string>; setField: (k: string, v: string) => void }) {
  return (
    <FieldGroup>
      <ConfigInput label="Host" configKey="host" config={config} setField={setField} placeholder="localhost" />
      <ConfigInput label="Port" configKey="port" config={config} setField={setField} placeholder="5672" />

      <Section title="Authentication">
        <ConfigInput label="Username" configKey="username" config={config} setField={setField} placeholder="guest" />
        <ConfigInput label="Password" configKey="password" config={config} setField={setField} placeholder="guest" type="password" />
      </Section>

      <Section title="Virtual Host">
        <ConfigInput label="Vhost" configKey="vhost" config={config} setField={setField} placeholder="/" />
      </Section>

      <Section title="TLS">
        <ConfigCheckbox label="Enable TLS" configKey="tls" config={config} setField={setField} />
        <ConfigCheckbox label="Insecure (skip cert verify)" configKey="tls_insecure" config={config} setField={setField} />
      </Section>
    </FieldGroup>
  )
}

// ── Redis fields ─────────────────────────────────────────────────────────────

function RedisFields({ config, setField }: { config: Record<string, string>; setField: (k: string, v: string) => void }) {
  return (
    <FieldGroup>
      <ConfigInput label="Host" configKey="host" config={config} setField={setField} placeholder="localhost" />
      <ConfigInput label="Port" configKey="port" config={config} setField={setField} placeholder="6379" />

      <Section title="Authentication">
        <ConfigInput label="Username" configKey="username" config={config} setField={setField} placeholder="(optional, Redis 6+ ACL)" />
        <ConfigInput label="Password" configKey="password" config={config} setField={setField} placeholder="(optional)" type="password" />
        <ConfigInput label="DB" configKey="db" config={config} setField={setField} placeholder="0" />
      </Section>

      <Section title="Connection Pool">
        <ConfigInput label="Pool Size" configKey="pool_size" config={config} setField={setField} placeholder="10 * GOMAXPROCS" />
        <ConfigInput label="Min Idle Conns" configKey="min_idle_conns" config={config} setField={setField} placeholder="0" />
        <ConfigInput label="Max Idle Conns" configKey="max_idle_conns" config={config} setField={setField} placeholder="0 (no limit)" />
        <ConfigInput label="Max Active Conns" configKey="max_active_conns" config={config} setField={setField} placeholder="0 (no limit)" />
        <ConfigInput label="Pool Timeout" configKey="pool_timeout" config={config} setField={setField} placeholder="ReadTimeout + 1s" description="Go duration" />
        <ConfigInput label="Conn Max Idle Time" configKey="conn_max_idle_time" config={config} setField={setField} placeholder="30m" />
        <ConfigInput label="Conn Max Lifetime" configKey="conn_max_lifetime" config={config} setField={setField} placeholder="0s (disabled)" />
        <ConfigCheckbox label="Pool FIFO" configKey="pool_fifo" config={config} setField={setField} />
      </Section>

      <Section title="Timeouts">
        <ConfigInput label="Dial Timeout" configKey="dial_timeout" config={config} setField={setField} placeholder="5s" description="Go duration" />
        <ConfigInput label="Read Timeout" configKey="read_timeout" config={config} setField={setField} placeholder="3s" />
        <ConfigInput label="Write Timeout" configKey="write_timeout" config={config} setField={setField} placeholder="3s" />
      </Section>

      <Section title="Retries">
        <ConfigInput label="Max Retries" configKey="max_retries" config={config} setField={setField} placeholder="3 (-1 disable)" />
        <ConfigInput label="Min Retry Backoff" configKey="min_retry_backoff" config={config} setField={setField} placeholder="8ms" />
        <ConfigInput label="Max Retry Backoff" configKey="max_retry_backoff" config={config} setField={setField} placeholder="512ms" />
      </Section>

      <Section title="TLS">
        <ConfigCheckbox label="Enable TLS" configKey="tls" config={config} setField={setField} />
        <ConfigCheckbox label="Insecure (skip cert verify)" configKey="tls_insecure" config={config} setField={setField} />
      </Section>

      <Section title="Advanced">
        <ConfigInput label="Client Name" configKey="client_name" config={config} setField={setField} placeholder="(optional)" />
        <ConfigSelect label="Network" configKey="network" config={config} setField={setField}
          placeholder="tcp (default)"
          options={[{ value: 'unix', label: 'unix' }]}
        />
        <ConfigSelect label="Protocol (RESP)" configKey="protocol" config={config} setField={setField}
          placeholder="3 (default)"
          options={[{ value: '2', label: '2' }]}
        />
        <ConfigCheckbox label="Context Timeout Enabled" configKey="context_timeout_enabled" config={config} setField={setField} />
      </Section>
    </FieldGroup>
  )
}

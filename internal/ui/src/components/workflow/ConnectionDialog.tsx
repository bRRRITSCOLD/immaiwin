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
        // Keep dialog open for Schwab OAuth flow; close for others
        if (value.type !== 'schwab') {
          onOpenChange(false)
        }
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
                      <SelectItem value="polymarket">Polymarket</SelectItem>
                      <SelectItem value="schwab">Schwab</SelectItem>
                      <SelectItem value="anthropic">Anthropic</SelectItem>
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
                    if (type === 'schwab') return <SchwabFields config={config} setField={setField} connectionId={id} isSaved={saved} disabled={justCreated} />
                    if (type === 'anthropic') return <AnthropicFields config={config} setField={setField} />
                    return <PolymarketFields config={config} setField={setField} />
                  }}
                </form.Field>
              )}
            </form.Subscribe>
          </FieldGroup>

          <form.Subscribe selector={(s) => s.values.type}>
            {(type) => {
              const showClose = type === 'schwab' && justCreated
              return (
                <DialogFooter className="pt-4">
                  {showClose ? (
                    <Button type="button" onClick={() => onOpenChange(false)}>Close</Button>
                  ) : (
                    <>
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
                    </>
                  )}
                </DialogFooter>
              )
            }}
          </form.Subscribe>
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

// ── Polymarket fields ────────────────────────────────────────────────────────

function PolymarketFields({ config, setField }: { config: Record<string, string>; setField: (k: string, v: string) => void }) {
  return (
    <FieldGroup>
      <ConfigInput label="API Key" configKey="api_key" config={config} setField={setField} placeholder="(optional — falls back to env POLYMARKET_API_KEY)" />
      <ConfigInput label="API Secret" configKey="api_secret" config={config} setField={setField} placeholder="(optional — falls back to env POLYMARKET_API_SECRET)" type="password" />
      <ConfigInput label="API Passphrase" configKey="api_passphrase" config={config} setField={setField} placeholder="(optional — falls back to env POLYMARKET_API_PASSPHRASE)" type="password" />
      <ConfigInput label="Private Key" configKey="private_key" config={config} setField={setField} placeholder="(optional — falls back to env POLYMARKET_PK)" type="password" />

      <Section title="WebSocket Tuning">
        <ConfigCheckbox label="Enable Reconnect" configKey="ws_reconnect" config={config} setField={setField} />
        <ConfigInput label="Reconnect Delay (ms)" configKey="ws_reconnect_delay_ms" config={config} setField={setField} placeholder="2000" />
        <ConfigInput label="Max Reconnect Delay (ms)" configKey="ws_reconnect_max_delay_ms" config={config} setField={setField} placeholder="30000" />
        <ConfigInput label="Backoff Multiplier" configKey="ws_backoff_multiplier" config={config} setField={setField} placeholder="2.0" />
        <ConfigInput label="Max Reconnect Attempts" configKey="ws_reconnect_max" config={config} setField={setField} placeholder="5" />
        <ConfigInput label="Heartbeat Interval (ms)" configKey="ws_heartbeat_interval_ms" config={config} setField={setField} placeholder="10000" />
        <ConfigCheckbox label="Debug Logging" configKey="ws_debug" config={config} setField={setField} />
      </Section>

      <Section title="URL Overrides">
        <ConfigInput label="CLOB WS URL" configKey="clob_ws_url" config={config} setField={setField} placeholder="wss://ws-subscriptions-clob.polymarket.com" />
        <ConfigInput label="CLOB REST URL" configKey="clob_url" config={config} setField={setField} placeholder="https://clob.polymarket.com" />
        <ConfigInput label="Gamma API URL" configKey="gamma_url" config={config} setField={setField} placeholder="https://gamma-api.polymarket.com" />
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

// ── Schwab fields ─────────────────────────────────────────────────────────────

function SchwabFields({ config, setField, connectionId, isSaved, disabled }: {
  config: Record<string, string>; setField: (k: string, v: string) => void
  connectionId: string; isSaved: boolean; disabled?: boolean
}) {
  const [oauthStatus, setOauthStatus] = useState<'unknown' | 'authorized' | 'not_authorized'>('unknown')
  const [oauthUrls, setOauthUrls] = useState<{ authorize_url: string; callback_url: string } | null>(null)
  const pollRef = useRef<ReturnType<typeof setInterval> | undefined>(undefined)

  const fetchOAuthStatus = useCallback(async () => {
    try {
      const res = await fetch(`${API_BASE}/api/v1/connections/${connectionId}/oauth/status`)
      if (!res.ok) return
      const data = await res.json()
      setOauthStatus(data.authorized ? 'authorized' : 'not_authorized')
      if (data.authorized && pollRef.current) {
        clearInterval(pollRef.current)
        pollRef.current = undefined
      }
    } catch { /* ignore */ }
  }, [connectionId])

  const fetchOAuthUrl = useCallback(async () => {
    try {
      const res = await fetch(`${API_BASE}/api/v1/connections/${connectionId}/oauth/url`)
      if (!res.ok) return
      const data = await res.json()
      setOauthUrls(data)
    } catch { /* ignore */ }
  }, [connectionId])

  // Load OAuth info when connection is saved
  useEffect(() => {
    if (!isSaved) return
    fetchOAuthStatus()
    fetchOAuthUrl()
  }, [isSaved, fetchOAuthStatus, fetchOAuthUrl])

  // Cleanup polling on unmount
  useEffect(() => {
    return () => {
      if (pollRef.current) clearInterval(pollRef.current)
    }
  }, [])

  function handleAuthorize() {
    if (!oauthUrls) return
    window.open(oauthUrls.authorize_url, 'schwab_oauth', 'width=600,height=700')
    // Start polling for OAuth completion
    if (pollRef.current) clearInterval(pollRef.current)
    pollRef.current = setInterval(fetchOAuthStatus, 2000)
  }

  return (
    <FieldGroup>
      <ConfigInput label="Client ID" configKey="client_id" config={config} setField={setField} placeholder="Your Schwab app client ID" disabled={disabled} />
      <ConfigInput label="Client Secret" configKey="client_secret" config={config} setField={setField} placeholder="Your Schwab app client secret" type="password" disabled={disabled} />

      {isSaved && (
        <>
          <Separator />
          <div className="space-y-3">
            <p className="text-xs font-medium text-muted-foreground">OAuth Authorization</p>

            {oauthUrls && (
              <Field>
                <FieldLabel className="text-xs">Callback URL</FieldLabel>
                <div className="flex items-center gap-1.5">
                  <Input
                    readOnly
                    value={oauthUrls.callback_url}
                    className="text-xs font-mono"
                    onClick={(e) => (e.target as HTMLInputElement).select()}
                  />
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className="shrink-0"
                    onClick={() => { navigator.clipboard.writeText(oauthUrls.callback_url); toast.success('Copied') }}
                  >
                    Copy
                  </Button>
                </div>
                <FieldDescription>Add this URL to your Schwab Developer Portal app callback URLs</FieldDescription>
              </Field>
            )}

            <div className="flex items-center gap-3">
              <Button type="button" variant="outline" size="sm" onClick={handleAuthorize} disabled={!oauthUrls}>
                Authorize
              </Button>
              {oauthStatus === 'authorized' && (
                <span className="inline-flex items-center gap-1.5 text-xs font-medium text-green-500">
                  <span className="h-2 w-2 rounded-full bg-green-500" /> Authorized
                </span>
              )}
              {oauthStatus === 'not_authorized' && (
                <span className="inline-flex items-center gap-1.5 text-xs font-medium text-red-500">
                  <span className="h-2 w-2 rounded-full bg-red-500" /> Not Authorized
                </span>
              )}
            </div>
          </div>
        </>
      )}
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

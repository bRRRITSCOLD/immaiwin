// /settings — user account management.
//
// Three sections:
//   - Account: email (read-only) + change password
//   - API Keys: list + create + revoke
//   - Linked Accounts: per-provider linked status, link button, unlink button
//
// Auth: AuthGate redirects unauth → /login. Page assumes me is set.

import { createFileRoute, Link } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { useForm } from '@tanstack/react-form'
import { z } from 'zod'
import { toast } from 'sonner'
import { Trash2, Plus, ExternalLink, Copy, Check, AlertTriangle } from 'lucide-react'
import { Button } from '~/components/ui/button'
import { Input } from '~/components/ui/input'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '~/components/ui/field'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '~/components/ui/card'
import { Separator } from '~/components/ui/separator'
import { Badge } from '~/components/ui/badge'
import { TenantSwitcher } from '~/components/TenantSwitcher'
import { api, ApiError, API_BASE } from '~/lib/api'
import { useAuthStore } from '~/lib/auth-store'

export const Route = createFileRoute('/settings')({
  component: SettingsPage,
})

interface APIKey {
  id: string
  name?: string
  key_prefix: string
  created_at: string
  last_used_at?: string
}

function SettingsPage() {
  const me = useAuthStore((s) => s.me)
  const loadMe = useAuthStore((s) => s.loadMe)

  if (!me) return null

  return (
    <div className="min-h-screen bg-background text-foreground">
      <header className="sticky top-0 z-10 border-b bg-background/90 backdrop-blur-sm px-6 py-3 flex items-center gap-4 shrink-0">
        <h1 className="text-lg font-semibold tracking-tight">immaiwin</h1>
        <Separator orientation="vertical" className="h-5" />
        <nav className="flex items-center gap-3 text-sm">
          <Link to="/" className="text-muted-foreground hover:text-foreground transition-colors">Polymarket</Link>
          <Link to="/workflows" className="text-muted-foreground hover:text-foreground transition-colors">Workflows</Link>
          <Link to="/runs" className="text-muted-foreground hover:text-foreground transition-colors">Runs</Link>
          <Link to="/evals" className="text-muted-foreground hover:text-foreground transition-colors">Evals</Link>
          <Link to="/skills" className="text-muted-foreground hover:text-foreground transition-colors">Skills</Link>
          <Link to="/settings" className="text-foreground font-medium">Settings</Link>
        </nav>
        <div className="ml-auto"><TenantSwitcher /></div>
      </header>

      <main className="max-w-3xl mx-auto p-6 space-y-6">
        <h2 className="text-2xl font-semibold">Account settings</h2>

        <AccountSection email={me.user.email} hasPassword={(me.user as { password_hash?: string }).password_hash !== ''} />

        <APIKeysSection />

        <LinkedAccountsSection
          linkedProviders={(me.user.oauth_providers ?? []).map((p) => p.provider)}
          onUpdate={() => void loadMe()}
        />
      </main>
    </div>
  )
}

const passwordSchema = z.string().min(8, 'Must be at least 8 characters')

function AccountSection({ email, hasPassword }: { email: string; hasPassword: boolean }) {
  const form = useForm({
    defaultValues: { current_password: '', new_password: '', confirm: '' },
    onSubmit: async ({ value }) => {
      if (value.new_password !== value.confirm) {
        toast.error('New passwords do not match')
        return
      }
      try {
        await api.post('/api/v1/auth/change_password', {
          current_password: value.current_password,
          new_password: value.new_password,
        })
        toast.success(hasPassword ? 'Password updated' : 'Password set')
        form.reset()
      } catch (err) {
        toast.error(err instanceof ApiError ? err.message : 'Update failed')
      }
    },
  })

  return (
    <Card>
      <CardHeader>
        <CardTitle>Account</CardTitle>
        <CardDescription>Email: <span className="font-mono text-foreground">{email}</span></CardDescription>
      </CardHeader>
      <CardContent>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            void form.handleSubmit()
          }}
        >
          <FieldGroup>
            {hasPassword && (
              <form.Field name="current_password">
                {(field) => (
                  <Field>
                    <FieldLabel htmlFor={field.name}>Current password</FieldLabel>
                    <Input
                      id={field.name}
                      type="password"
                      autoComplete="current-password"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                    />
                  </Field>
                )}
              </form.Field>
            )}
            <form.Field
              name="new_password"
              validators={{ onChange: ({ value }) => passwordSchema.safeParse(value).error?.issues[0]?.message }}
            >
              {(field) => (
                <Field>
                  <FieldLabel htmlFor={field.name}>{hasPassword ? 'New password' : 'Set password'}</FieldLabel>
                  <Input
                    id={field.name}
                    type="password"
                    autoComplete="new-password"
                    value={field.state.value}
                    onChange={(e) => field.handleChange(e.target.value)}
                  />
                  <FieldError errors={field.state.meta.errors as string[]} />
                </Field>
              )}
            </form.Field>
            <form.Field name="confirm">
              {(field) => (
                <Field>
                  <FieldLabel htmlFor={field.name}>Confirm new password</FieldLabel>
                  <Input
                    id={field.name}
                    type="password"
                    autoComplete="new-password"
                    value={field.state.value}
                    onChange={(e) => field.handleChange(e.target.value)}
                  />
                </Field>
              )}
            </form.Field>
            <form.Subscribe selector={(s) => [s.canSubmit, s.isSubmitting] as const}>
              {([canSubmit, isSubmitting]) => (
                <Button type="submit" disabled={!canSubmit || isSubmitting}>
                  {isSubmitting ? 'Saving…' : hasPassword ? 'Change password' : 'Set password'}
                </Button>
              )}
            </form.Subscribe>
            {!hasPassword && (
              <FieldDescription>
                You signed in with an external provider. Setting a password lets you sign in with email + password too.
              </FieldDescription>
            )}
          </FieldGroup>
        </form>
      </CardContent>
    </Card>
  )
}

function APIKeysSection() {
  const [keys, setKeys] = useState<APIKey[]>([])
  const [loading, setLoading] = useState(true)
  const [newKeyValue, setNewKeyValue] = useState<string | null>(null)
  const [creating, setCreating] = useState(false)
  const [copied, setCopied] = useState(false)
  const [name, setName] = useState('')

  useEffect(() => {
    void load()
  }, [])

  async function load() {
    setLoading(true)
    try {
      const res = await api.get<APIKey[]>('/api/v1/api_keys')
      setKeys(res ?? [])
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : 'Load failed')
    } finally {
      setLoading(false)
    }
  }

  async function create() {
    setCreating(true)
    try {
      // Backend returns { key, raw, warning } — raw is the only place the
      // plaintext key surfaces; subsequent List calls expose prefix only.
      const res = await api.post<{ key: APIKey; raw: string; warning: string }>('/api/v1/api_keys', { name })
      setNewKeyValue(res.raw)
      setName('')
      await load()
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : 'Create failed')
    } finally {
      setCreating(false)
    }
  }

  async function revoke(id: string) {
    if (!confirm('Revoke this API key? Programs using it will start to 401.')) return
    try {
      await api.delete(`/api/v1/api_keys/${id}`)
      await load()
      toast.success('Key revoked')
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : 'Revoke failed')
    }
  }

  async function copyKey() {
    if (!newKeyValue) return
    await navigator.clipboard.writeText(newKeyValue)
    setCopied(true)
    setTimeout(() => setCopied(false), 1500)
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>API Keys</CardTitle>
        <CardDescription>Bearer tokens for programmatic access. Treat like passwords.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {newKeyValue && (
          <div className="rounded border border-amber-600/50 bg-amber-950/20 p-3 space-y-2">
            <div className="flex items-center gap-2 text-amber-400 text-sm font-medium">
              <AlertTriangle className="size-4" />
              Copy this key now — it won't be shown again.
            </div>
            <div className="flex items-center gap-2">
              <code className="flex-1 font-mono text-xs bg-background border rounded px-2 py-1 break-all">
                {newKeyValue}
              </code>
              <Button size="sm" variant="outline" onClick={() => void copyKey()}>
                {copied ? <Check className="size-4" /> : <Copy className="size-4" />}
              </Button>
            </div>
            <Button size="sm" variant="ghost" onClick={() => setNewKeyValue(null)}>Dismiss</Button>
          </div>
        )}

        <div className="flex items-end gap-2">
          <div className="flex-1">
            <FieldLabel htmlFor="key-name">New key name</FieldLabel>
            <Input
              id="key-name"
              placeholder="my CI runner"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </div>
          <Button onClick={() => void create()} disabled={creating}>
            <Plus className="size-4 mr-1" />
            {creating ? 'Creating…' : 'Create'}
          </Button>
        </div>

        {loading && <div className="text-sm text-muted-foreground">Loading…</div>}
        {!loading && keys.length === 0 && (
          <div className="text-sm text-muted-foreground">No API keys yet.</div>
        )}
        {!loading && keys.length > 0 && (
          <div className="border rounded divide-y">
            {keys.map((k) => (
              <div key={k.id} className="flex items-center gap-3 p-3 text-sm">
                <div className="flex-1">
                  <div className="font-medium">{k.name || '(unnamed)'}</div>
                  <div className="text-xs text-muted-foreground font-mono">{k.key_prefix}…</div>
                  <div className="text-xs text-muted-foreground">
                    created {new Date(k.created_at).toLocaleDateString()}
                    {k.last_used_at && ` · last used ${new Date(k.last_used_at).toLocaleDateString()}`}
                  </div>
                </div>
                <Button size="sm" variant="ghost" onClick={() => void revoke(k.id)}>
                  <Trash2 className="size-4 text-destructive" />
                </Button>
              </div>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function LinkedAccountsSection({
  linkedProviders,
  onUpdate,
}: {
  linkedProviders: string[]
  onUpdate: () => void
}) {
  const providers: Array<{ id: 'google' | 'github'; label: string }> = [
    { id: 'google', label: 'Google' },
    { id: 'github', label: 'GitHub' },
  ]

  async function unlink(provider: string) {
    if (!confirm(`Unlink ${provider}? You'll no longer be able to sign in with it.`)) return
    try {
      await api.delete(`/api/v1/auth/oauth/${provider}/unlink`)
      toast.success(`${provider} unlinked`)
      onUpdate()
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : 'Unlink failed')
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Linked accounts</CardTitle>
        <CardDescription>OAuth providers you can sign in with.</CardDescription>
      </CardHeader>
      <CardContent>
        <div className="space-y-2">
          {providers.map((p) => {
            const linked = linkedProviders.includes(p.id)
            return (
              <div key={p.id} className="flex items-center gap-3 p-3 border rounded">
                <div className="flex-1">
                  <div className="font-medium">{p.label}</div>
                  <div className="text-xs text-muted-foreground">
                    {linked ? 'Linked — sign in with this provider' : 'Not linked'}
                  </div>
                </div>
                {linked ? (
                  <>
                    <Badge variant="outline">linked</Badge>
                    <Button size="sm" variant="ghost" onClick={() => void unlink(p.id)}>
                      Unlink
                    </Button>
                  </>
                ) : (
                  <a href={`${API_BASE}/auth/oauth/${p.id}/start`}>
                    <Button size="sm" variant="outline">
                      <ExternalLink className="size-4 mr-1" />
                      Link
                    </Button>
                  </a>
                )}
              </div>
            )
          })}
        </div>
      </CardContent>
    </Card>
  )
}

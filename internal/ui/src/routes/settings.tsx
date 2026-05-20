// /settings — user account management.
//
// Three sections:
//   - Account: email (read-only) + change password
//   - API Keys: list + create + revoke
//   - Linked Accounts: per-provider linked status, link button, unlink button
//
// Auth: AuthGate redirects unauth → /login. Page assumes me is set.

import { createFileRoute } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { useForm } from '@tanstack/react-form'
import { z } from 'zod'
import { toast } from 'sonner'
import { Trash2, Plus, ExternalLink, Copy, Check, AlertTriangle, UserMinus, Users, ScrollText, Crown } from 'lucide-react'
import { Button } from '~/components/ui/button'
import { Input } from '~/components/ui/input'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '~/components/ui/field'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '~/components/ui/card'
import { Badge } from '~/components/ui/badge'
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
      <main className="flex-1 min-h-0 overflow-y-auto max-w-3xl w-full mx-auto p-6 space-y-6">
        <h2 className="text-2xl font-semibold">Account settings</h2>

        <AccountSection email={me.user.email} hasPassword={(me.user as { password_hash?: string }).password_hash !== ''} />

        <APIKeysSection />

        <MembersSection currentUserId={me.user.id} />

        <AuditLogSection />

        <LinkedAccountsSection
          linkedProviders={(me.user.oauth_providers ?? []).map((p) => p.provider)}
          onUpdate={() => void loadMe()}
        />
      </main>
  )
}

interface AuditEntry {
  id: string
  ts: string
  tenant_id?: string
  user_id?: string
  actor_email?: string
  action: string
  target?: Record<string, unknown>
  ip?: string
  user_agent?: string
  metadata?: Record<string, unknown>
}

const ACTION_FILTERS = [
  { value: '', label: 'All actions' },
  { value: 'login_success', label: 'Login success' },
  { value: 'login_failure', label: 'Login failure' },
  { value: 'logout', label: 'Logout' },
  { value: 'password_change', label: 'Password change' },
  { value: 'password_reset_requested', label: 'Reset requested' },
  { value: 'password_reset_completed', label: 'Reset completed' },
  { value: 'api_key_created', label: 'API key created' },
  { value: 'api_key_revoked', label: 'API key revoked' },
  { value: 'oauth_linked', label: 'OAuth linked' },
  { value: 'oauth_unlinked', label: 'OAuth unlinked' },
  { value: 'tenant_switch', label: 'Tenant switch' },
  { value: 'invite_created', label: 'Invite created' },
  { value: 'invite_revoked', label: 'Invite revoked' },
  { value: 'invite_accepted', label: 'Invite accepted' },
  { value: 'member_removed', label: 'Member removed' },
  { value: 'tenant_ownership_transferred', label: 'Ownership transferred' },
  { value: 'workflow_duplicated', label: 'Workflow duplicated' },
  { value: 'workflow_enabled', label: 'Workflow enabled' },
  { value: 'workflow_disabled', label: 'Workflow disabled' },
  { value: 'workflow_renamed', label: 'Workflow renamed' },
]

function AuditLogSection() {
  const [entries, setEntries] = useState<AuditEntry[]>([])
  const [loading, setLoading] = useState(true)
  const [forbidden, setForbidden] = useState(false)
  const [actionFilter, setActionFilter] = useState('')

  async function load() {
    setLoading(true)
    try {
      const q = actionFilter ? `?action=${actionFilter}&limit=100` : '?limit=100'
      const res = await api.get<{ entries: AuditEntry[] }>(`/api/v1/audit_log${q}`)
      setEntries(res.entries ?? [])
      setForbidden(false)
    } catch (err) {
      if (err instanceof ApiError && err.status === 403) {
        // Members see their own actions only — for now, we hide the
        // section entirely. Future: surface a /me audit view.
        setForbidden(true)
        setEntries([])
      } else {
        toast.error(err instanceof ApiError ? err.message : 'Load failed')
      }
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
  }, [actionFilter])

  if (forbidden) return null

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <ScrollText className="size-5" />
          Activity log
        </CardTitle>
        <CardDescription>Privileged actions in this tenant. Owner/admin view.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="flex items-center gap-2">
          <FieldLabel htmlFor="audit-filter">Filter</FieldLabel>
          <select
            id="audit-filter"
            className="border rounded px-2 py-1.5 text-sm bg-background"
            value={actionFilter}
            onChange={(e) => setActionFilter(e.target.value)}
          >
            {ACTION_FILTERS.map((a) => (
              <option key={a.value} value={a.value}>{a.label}</option>
            ))}
          </select>
        </div>

        {loading && <div className="text-sm text-muted-foreground">Loading…</div>}

        {!loading && entries.length === 0 && (
          <div className="text-sm text-muted-foreground">No events match this filter.</div>
        )}

        {!loading && entries.length > 0 && (
          <div className="border rounded divide-y max-h-[480px] overflow-auto">
            {entries.map((e) => (
              <div key={e.id} className="p-3 text-xs space-y-1">
                <div className="flex items-center gap-2">
                  <span className="text-muted-foreground">{new Date(e.ts).toLocaleString()}</span>
                  <Badge variant="outline" className="text-xs">{e.action}</Badge>
                  {e.actor_email && <span className="font-mono">{e.actor_email}</span>}
                  {e.ip && <span className="text-muted-foreground ml-auto">{e.ip}</span>}
                </div>
                {(e.target || e.metadata) && (
                  <pre className="text-xs text-muted-foreground bg-muted/30 rounded p-2 overflow-x-auto">
                    {JSON.stringify({ target: e.target, metadata: e.metadata }, null, 0)}
                  </pre>
                )}
              </div>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  )
}

interface Member {
  user_id: string
  email: string
  role: string
  joined_at: string
}

interface PendingInvite {
  id: string
  email: string
  role: string
  token_prefix: string
  created_at: string
  expires_at: string
}

function MembersSection({ currentUserId }: { currentUserId: string }) {
  const [members, setMembers] = useState<Member[]>([])
  const [invites, setInvites] = useState<PendingInvite[]>([])
  const [loading, setLoading] = useState(true)
  const [inviteEmail, setInviteEmail] = useState('')
  const [inviteRole, setInviteRole] = useState<'member' | 'admin'>('member')
  const [creating, setCreating] = useState(false)
  const [lastInviteURL, setLastInviteURL] = useState<string | null>(null)

  async function load() {
    setLoading(true)
    try {
      const [m, inv] = await Promise.all([
        api.get<{ members: Member[] }>('/api/v1/tenants/members'),
        api.get<{ invites: PendingInvite[] }>('/api/v1/tenants/invites'),
      ])
      setMembers(m.members ?? [])
      setInvites(inv.invites ?? [])
    } catch (err) {
      // Non-admin members get 403 on /tenants/invites; degrade to
      // showing the members list only.
      if (err instanceof ApiError && err.status === 403) {
        try {
          const m = await api.get<{ members: Member[] }>('/api/v1/tenants/members')
          setMembers(m.members ?? [])
          setInvites([])
        } catch (err2) {
          toast.error(err2 instanceof ApiError ? err2.message : 'Load failed')
        }
      } else {
        toast.error(err instanceof ApiError ? err.message : 'Load failed')
      }
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
  }, [])

  async function createInvite() {
    if (!inviteEmail.includes('@')) {
      toast.error('Valid email required')
      return
    }
    setCreating(true)
    try {
      const res = await api.post<{ invite: PendingInvite; url: string }>(
        '/api/v1/tenants/invites',
        { email: inviteEmail.trim().toLowerCase(), role: inviteRole },
      )
      setLastInviteURL(res.url)
      setInviteEmail('')
      await load()
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : 'Invite failed')
    } finally {
      setCreating(false)
    }
  }

  async function revokeInvite(id: string) {
    if (!confirm('Revoke invite? The recipient won\'t be able to use the link.')) return
    try {
      await api.delete(`/api/v1/tenants/invites/${id}`)
      toast.success('Invite revoked')
      await load()
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : 'Revoke failed')
    }
  }

  async function removeMember(userID: string) {
    if (!confirm('Remove this member from the tenant?')) return
    try {
      await api.delete(`/api/v1/tenants/members/${userID}`)
      toast.success('Member removed')
      await load()
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : 'Remove failed')
    }
  }

  async function transferOwnership(userID: string, email: string) {
    if (!confirm(
      `Transfer ownership of this tenant to ${email}?\n\n` +
      `You will be demoted to admin. ${email} will gain owner privileges, ` +
      `including the ability to transfer ownership again or remove other admins.`,
    )) return
    try {
      await api.post('/api/v1/tenants/transfer', { to_user_id: userID })
      toast.success(`Ownership transferred to ${email}`)
      await load()
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : 'Transfer failed')
    }
  }

  // Caller is the owner iff their own row in `members` carries the
  // owner role. Drives whether per-row Transfer buttons render.
  const isOwner = members.find((m) => m.user_id === currentUserId)?.role === 'owner'

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Users className="size-5" />
          Team members
        </CardTitle>
        <CardDescription>People with access to this tenant.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {lastInviteURL && (
          <div className="rounded border border-amber-600/50 bg-amber-950/20 p-3 space-y-2">
            <div className="flex items-center gap-2 text-amber-400 text-sm font-medium">
              <AlertTriangle className="size-4" />
              Invite URL — copy now (also sent to recipient's email if SMTP wired):
            </div>
            <code className="block font-mono text-xs bg-background border rounded px-2 py-1 break-all">
              {lastInviteURL}
            </code>
            <Button size="sm" variant="ghost" onClick={() => setLastInviteURL(null)}>Dismiss</Button>
          </div>
        )}

        <div className="grid grid-cols-[1fr_auto_auto] gap-2 items-end">
          <div>
            <FieldLabel htmlFor="invite-email">Invite by email</FieldLabel>
            <Input
              id="invite-email"
              type="email"
              placeholder="teammate@example.com"
              value={inviteEmail}
              onChange={(e) => setInviteEmail(e.target.value)}
            />
          </div>
          <div>
            <FieldLabel htmlFor="invite-role">Role</FieldLabel>
            <select
              id="invite-role"
              className="border rounded px-2 py-2 text-sm bg-background"
              value={inviteRole}
              onChange={(e) => setInviteRole(e.target.value as 'member' | 'admin')}
            >
              <option value="member">member</option>
              <option value="admin">admin</option>
            </select>
          </div>
          <Button onClick={() => void createInvite()} disabled={creating}>
            <Plus className="size-4 mr-1" />
            {creating ? 'Sending…' : 'Invite'}
          </Button>
        </div>

        {loading && <div className="text-sm text-muted-foreground">Loading…</div>}

        {!loading && members.length > 0 && (
          <div className="border rounded divide-y">
            <div className="p-2 text-xs uppercase text-muted-foreground">Members ({members.length})</div>
            {members.map((m) => (
              <div key={m.user_id} className="flex items-center gap-3 p-3 text-sm">
                <div className="flex-1">
                  <div className="font-medium font-mono">{m.email}</div>
                  <div className="text-xs text-muted-foreground">
                    joined {new Date(m.joined_at).toLocaleDateString()}
                  </div>
                </div>
                <Badge variant="outline">{m.role}</Badge>
                {isOwner && m.user_id !== currentUserId && m.role === 'admin' && (
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => void transferOwnership(m.user_id, m.email)}
                    title="Transfer ownership to this admin"
                  >
                    <Crown className="size-4 text-amber-400" />
                  </Button>
                )}
                {m.user_id !== currentUserId && m.role !== 'owner' && (
                  <Button size="sm" variant="ghost" onClick={() => void removeMember(m.user_id)}>
                    <UserMinus className="size-4 text-destructive" />
                  </Button>
                )}
              </div>
            ))}
          </div>
        )}

        {!loading && invites.length > 0 && (
          <div className="border rounded divide-y">
            <div className="p-2 text-xs uppercase text-muted-foreground">Pending invites ({invites.length})</div>
            {invites.map((inv) => (
              <div key={inv.id} className="flex items-center gap-3 p-3 text-sm">
                <div className="flex-1">
                  <div className="font-medium font-mono">{inv.email}</div>
                  <div className="text-xs text-muted-foreground">
                    {inv.token_prefix}… · expires {new Date(inv.expires_at).toLocaleDateString()}
                  </div>
                </div>
                <Badge variant="outline">{inv.role}</Badge>
                <Button size="sm" variant="ghost" onClick={() => void revokeInvite(inv.id)}>
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

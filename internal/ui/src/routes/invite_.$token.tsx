// /invite/:token — accept a tenant invite.
//
// Flow:
//   1. Mount → fetch /api/v1/invites/:token/preview (public).
//   2. Render "you're being invited to <tenant> as <role> by <inviter>".
//   3. If user not authed: prompt to sign in or register, preserving
//      the URL via ?next= so AuthGate's redirect lands them back here.
//   4. If user authed but email doesn't match invite's email: show
//      mismatch error (the invitee_email field is exposed in preview).
//   5. Accept button → POST /accept → redirect to /workflows.

import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { toast } from 'sonner'
import { Building2, AlertTriangle } from 'lucide-react'
import { Button } from '~/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '~/components/ui/card'
import { Badge } from '~/components/ui/badge'
import { api, ApiError } from '~/lib/api'
import { useAuthStore } from '~/lib/auth-store'

export const Route = createFileRoute('/invite_/$token')({
  component: InvitePage,
})

interface Preview {
  tenant_name: string
  tenant_id: string
  role: string
  invitee_email: string
  inviter_email: string
  expires_at: string
}

function InvitePage() {
  const { token } = Route.useParams()
  const navigate = useNavigate()
  const me = useAuthStore((s) => s.me)
  const loadMe = useAuthStore((s) => s.loadMe)

  const [preview, setPreview] = useState<Preview | null>(null)
  const [loadErr, setLoadErr] = useState<string | null>(null)
  const [accepting, setAccepting] = useState(false)

  useEffect(() => {
    if (!token) return
    void (async () => {
      try {
        const p = await api.get<Preview>(`/api/v1/invites/${token}/preview`)
        setPreview(p)
      } catch (err) {
        const msg = err instanceof ApiError ? err.message : 'Failed to load invite'
        setLoadErr(msg)
      }
    })()
  }, [token])

  async function accept() {
    setAccepting(true)
    try {
      await api.post(`/api/v1/invites/${token}/accept`)
      toast.success('Joined tenant')
      // Refresh /me so memberships + active tenant pick up the new
      // row, then land on /workflows under the new tenant context.
      await loadMe()
      void navigate({ to: '/workflows' })
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : 'Accept failed')
    } finally {
      setAccepting(false)
    }
  }

  return (
    <div className="min-h-screen w-full flex items-center justify-center bg-background px-4">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <Building2 className="size-5" />
            Tenant invite
          </CardTitle>
          <CardDescription>Review the invite then accept to join the team.</CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {loadErr && (
            <div className="rounded border border-destructive/50 bg-destructive/10 p-3 text-sm text-destructive flex gap-2">
              <AlertTriangle className="size-4 shrink-0 mt-0.5" />
              <div>
                {loadErr}
                <div className="mt-2">
                  <Link to="/login" className="underline">Back to sign in</Link>
                </div>
              </div>
            </div>
          )}

          {preview && (
            <>
              <div className="space-y-2 text-sm">
                <div>
                  <span className="text-muted-foreground">Tenant:</span>{' '}
                  <span className="font-medium">{preview.tenant_name}</span>
                </div>
                <div>
                  <span className="text-muted-foreground">Role:</span>{' '}
                  <Badge variant="outline">{preview.role}</Badge>
                </div>
                <div>
                  <span className="text-muted-foreground">Invited by:</span>{' '}
                  <span className="font-mono">{preview.inviter_email}</span>
                </div>
                <div>
                  <span className="text-muted-foreground">For email:</span>{' '}
                  <span className="font-mono">{preview.invitee_email}</span>
                </div>
                <div>
                  <span className="text-muted-foreground">Expires:</span>{' '}
                  {new Date(preview.expires_at).toLocaleString()}
                </div>
              </div>

              {!me && (
                <div className="rounded border border-amber-600/50 bg-amber-950/20 p-3 text-sm space-y-2">
                  <div className="flex items-center gap-2 text-amber-400 font-medium">
                    <AlertTriangle className="size-4" />
                    Sign in to accept
                  </div>
                  <div>
                    <Link
                      to="/login"
                      search={{ next: encodeURIComponent(`/invite/${token}`) } as never}
                      className="underline"
                    >
                      Sign in
                    </Link>
                    {' or '}
                    <Link
                      to="/register"
                      search={{ next: encodeURIComponent(`/invite/${token}`) } as never}
                      className="underline"
                    >
                      register
                    </Link>{' '}
                    with <span className="font-mono">{preview.invitee_email}</span> first.
                  </div>
                </div>
              )}

              {me && me.user.email.toLowerCase() !== preview.invitee_email.toLowerCase() && (
                <div className="rounded border border-destructive/50 bg-destructive/10 p-3 text-sm text-destructive">
                  This invite was issued to <span className="font-mono">{preview.invitee_email}</span>,
                  but you're signed in as <span className="font-mono">{me.user.email}</span>.
                  Sign out and register or sign in with the matching email to accept.
                </div>
              )}

              {me && me.user.email.toLowerCase() === preview.invitee_email.toLowerCase() && (
                <Button
                  className="w-full"
                  onClick={() => void accept()}
                  disabled={accepting}
                >
                  {accepting ? 'Joining…' : `Accept and join ${preview.tenant_name}`}
                </Button>
              )}
            </>
          )}

          {!preview && !loadErr && (
            <div className="text-sm text-muted-foreground">Loading invite…</div>
          )}
        </CardContent>
      </Card>
    </div>
  )
}

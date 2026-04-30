import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { useForm } from '@tanstack/react-form'
import { z } from 'zod'
import { toast } from 'sonner'
import { Button } from '~/components/ui/button'
import { Input } from '~/components/ui/input'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '~/components/ui/field'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '~/components/ui/card'
import { Separator } from '~/components/ui/separator'
import { useAuthStore } from '~/lib/auth-store'
import { ApiError } from '~/lib/api'

interface LoginSearch {
  next?: string
}

export const Route = createFileRoute('/login')({
  component: LoginPage,
  validateSearch: (raw: Record<string, unknown>): LoginSearch => ({
    next: typeof raw['next'] === 'string' ? (raw['next'] as string) : undefined,
  }),
})

const emailSchema = z.string().min(1, 'Required').email('Invalid email')
const passwordSchema = z.string().min(8, 'Must be at least 8 characters')

function LoginPage() {
  const navigate = useNavigate()
  const search = Route.useSearch()
  const me = useAuthStore((s) => s.me)
  const login = useAuthStore((s) => s.login)
  const oauthStartUrl = useAuthStore((s) => s.oauthStartUrl)

  // If already authed, bounce to next or /workflows.
  useEffect(() => {
    if (me) {
      const next = search.next ? decodeURIComponent(search.next) : '/workflows'
      void navigate({ to: next as never, replace: true })
    }
  }, [me, search.next, navigate])

  const form = useForm({
    defaultValues: { email: '', password: '' },
    onSubmit: async ({ value }) => {
      try {
        await login(value.email.trim().toLowerCase(), value.password)
        toast.success('Signed in')
      } catch (err) {
        const msg = err instanceof ApiError ? err.message : 'Login failed'
        toast.error(msg)
      }
    },
  })

  return (
    <div className="min-h-screen w-full flex items-center justify-center bg-background px-4">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle>Sign in to immaiwin</CardTitle>
          <CardDescription>Use your email and password, or continue with a provider.</CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <OAuthButtons oauthStartUrl={oauthStartUrl} />

          <div className="relative my-2">
            <Separator />
            <span className="absolute inset-0 -top-2 mx-auto w-fit bg-background px-2 text-xs text-muted-foreground">
              or
            </span>
          </div>

          <form
            onSubmit={(e) => {
              e.preventDefault()
              void form.handleSubmit()
            }}
          >
            <FieldGroup>
              <form.Field
                name="email"
                validators={{ onChange: ({ value }) => emailSchema.safeParse(value).error?.issues[0]?.message }}
              >
                {(field) => (
                  <Field>
                    <FieldLabel htmlFor={field.name}>Email</FieldLabel>
                    <Input
                      id={field.name}
                      type="email"
                      autoComplete="email"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                      onBlur={field.handleBlur}
                    />
                    <FieldError errors={field.state.meta.errors as string[]} />
                  </Field>
                )}
              </form.Field>

              <form.Field
                name="password"
                validators={{ onChange: ({ value }) => passwordSchema.safeParse(value).error?.issues[0]?.message }}
              >
                {(field) => (
                  <Field>
                    <FieldLabel htmlFor={field.name}>Password</FieldLabel>
                    <Input
                      id={field.name}
                      type="password"
                      autoComplete="current-password"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                      onBlur={field.handleBlur}
                    />
                    <FieldError errors={field.state.meta.errors as string[]} />
                  </Field>
                )}
              </form.Field>

              <form.Subscribe
                selector={(s) => [s.canSubmit, s.isSubmitting] as const}
              >
                {([canSubmit, isSubmitting]) => (
                  <Button type="submit" className="w-full" disabled={!canSubmit || isSubmitting}>
                    {isSubmitting ? 'Signing in…' : 'Sign in'}
                  </Button>
                )}
              </form.Subscribe>

              <FieldDescription className="text-center">
                No account?{' '}
                <Link to="/register" className="underline hover:text-foreground">
                  Register
                </Link>
              </FieldDescription>
            </FieldGroup>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}

function OAuthButtons({ oauthStartUrl }: { oauthStartUrl: (p: 'google' | 'github') => string }) {
  const [probing, setProbing] = useState(true)
  const [enabled, setEnabled] = useState<{ google: boolean; github: boolean }>({
    google: false,
    github: false,
  })

  // HEAD-probe each /start; 302 → enabled, 404 → not configured. We
  // can't follow the redirect (would jump to Google), so we use a
  // no-cors HEAD and just check it didn't 404. Fallback: render both
  // optimistically and let the click 404 if unconfigured.
  useEffect(() => {
    let cancelled = false
    async function probe() {
      const check = async (p: 'google' | 'github'): Promise<boolean> => {
        try {
          const res = await fetch(oauthStartUrl(p), {
            method: 'GET',
            redirect: 'manual',
            credentials: 'include',
          })
          // opaqueredirect (status 0) on a 302 is the success signal in
          // browsers when redirect: 'manual'. 404 → disabled.
          if (res.type === 'opaqueredirect') return true
          if (res.status === 404) return false
          return res.status >= 300 && res.status < 400
        } catch {
          return false
        }
      }
      const [g, gh] = await Promise.all([check('google'), check('github')])
      if (!cancelled) {
        setEnabled({ google: g, github: gh })
        setProbing(false)
      }
    }
    void probe()
    return () => {
      cancelled = true
    }
  }, [oauthStartUrl])

  if (probing) {
    return <div className="text-xs text-muted-foreground text-center">checking providers…</div>
  }
  if (!enabled.google && !enabled.github) {
    return (
      <div className="text-xs text-muted-foreground text-center">
        OAuth providers not configured.
      </div>
    )
  }
  return (
    <div className="grid gap-2">
      {enabled.google && (
        <a href={oauthStartUrl('google')}>
          <Button type="button" variant="outline" className="w-full">
            Continue with Google
          </Button>
        </a>
      )}
      {enabled.github && (
        <a href={oauthStartUrl('github')}>
          <Button type="button" variant="outline" className="w-full">
            Continue with GitHub
          </Button>
        </a>
      )}
    </div>
  )
}

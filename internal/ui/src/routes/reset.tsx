// /reset?token=<jwt> — confirm a password reset.
//
// Token comes from the email link's query string. Page accepts a new
// password, POSTs /password_reset/confirm. On success, redirects to
// /login so the user signs in fresh (cookie was never set during reset).

import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { useForm } from '@tanstack/react-form'
import { z } from 'zod'
import { toast } from 'sonner'
import { Button } from '~/components/ui/button'
import { Input } from '~/components/ui/input'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '~/components/ui/field'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '~/components/ui/card'
import { api, ApiError } from '~/lib/api'

interface ResetSearch {
  token?: string
}

export const Route = createFileRoute('/reset')({
  component: ResetPage,
  validateSearch: (raw: Record<string, unknown>): ResetSearch => ({
    token: typeof raw['token'] === 'string' ? (raw['token'] as string) : undefined,
  }),
})

const passwordSchema = z.string().min(8, 'Must be at least 8 characters')

function ResetPage() {
  const navigate = useNavigate()
  const search = Route.useSearch()
  const token = search.token ?? ''

  const form = useForm({
    defaultValues: { new_password: '', confirm: '' },
    onSubmit: async ({ value }) => {
      if (value.new_password !== value.confirm) {
        toast.error('Passwords do not match')
        return
      }
      if (!token) {
        toast.error('Reset link is missing the token. Use the link from your email.')
        return
      }
      try {
        await api.post('/api/v1/auth/password_reset/confirm', {
          token,
          new_password: value.new_password,
        })
        toast.success('Password reset — sign in with your new password')
        void navigate({ to: '/login' })
      } catch (err) {
        toast.error(err instanceof ApiError ? err.message : 'Reset failed — link may be expired')
      }
    },
  })

  return (
    <div className="min-h-screen w-full flex items-center justify-center bg-background px-4">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle>Set a new password</CardTitle>
          <CardDescription>Choose a new password for your burrow account.</CardDescription>
        </CardHeader>
        <CardContent>
          {!token && (
            <div className="mb-4 rounded border border-destructive/50 bg-destructive/10 p-3 text-sm text-destructive">
              Token missing from URL. Use the reset link from your email.
            </div>
          )}
          <form
            onSubmit={(e) => {
              e.preventDefault()
              void form.handleSubmit()
            }}
          >
            <FieldGroup>
              <form.Field
                name="new_password"
                validators={{ onChange: ({ value }) => passwordSchema.safeParse(value).error?.issues[0]?.message }}
              >
                {(field) => (
                  <Field>
                    <FieldLabel htmlFor={field.name}>New password</FieldLabel>
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
                  <Button type="submit" className="w-full" disabled={!canSubmit || isSubmitting || !token}>
                    {isSubmitting ? 'Saving…' : 'Set new password'}
                  </Button>
                )}
              </form.Subscribe>

              <FieldDescription className="text-center">
                <Link to="/login" className="underline hover:text-foreground">
                  Back to sign in
                </Link>
              </FieldDescription>
            </FieldGroup>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}

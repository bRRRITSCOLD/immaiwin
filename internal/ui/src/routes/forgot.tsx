// /forgot — request a password reset link by email.
//
// Always shows the same "if the email is registered, we sent a link"
// message regardless of whether the email exists. Backend mirrors
// this — POST /password_reset/request always returns 200, no enum.

import { createFileRoute, Link } from '@tanstack/react-router'
import { useState } from 'react'
import { useForm } from '@tanstack/react-form'
import { z } from 'zod'
import { toast } from 'sonner'
import { Button } from '~/components/ui/button'
import { Input } from '~/components/ui/input'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '~/components/ui/field'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '~/components/ui/card'
import { api, ApiError } from '~/lib/api'

export const Route = createFileRoute('/forgot')({
  component: ForgotPage,
})

const emailSchema = z.string().min(1, 'Required').email('Invalid email')

function ForgotPage() {
  const [sent, setSent] = useState(false)

  const form = useForm({
    defaultValues: { email: '' },
    onSubmit: async ({ value }) => {
      try {
        await api.post('/api/v1/auth/password_reset/request', {
          email: value.email.trim().toLowerCase(),
        })
        // Always show success regardless — server returns 200 even
        // when the email isn't registered (no enum).
        setSent(true)
      } catch (err) {
        toast.error(err instanceof ApiError ? err.message : 'Request failed')
      }
    },
  })

  if (sent) {
    return (
      <div className="min-h-screen w-full flex items-center justify-center bg-background px-4">
        <Card className="w-full max-w-md">
          <CardHeader>
            <CardTitle>Check your email</CardTitle>
            <CardDescription>
              If that email is registered, we sent a reset link. The link expires in 15 minutes.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <FieldDescription className="text-center">
              <Link to="/login" className="underline hover:text-foreground">
                Back to sign in
              </Link>
            </FieldDescription>
          </CardContent>
        </Card>
      </div>
    )
  }

  return (
    <div className="min-h-screen w-full flex items-center justify-center bg-background px-4">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle>Reset your password</CardTitle>
          <CardDescription>Enter your email and we'll send a reset link.</CardDescription>
        </CardHeader>
        <CardContent>
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

              <form.Subscribe selector={(s) => [s.canSubmit, s.isSubmitting] as const}>
                {([canSubmit, isSubmitting]) => (
                  <Button type="submit" className="w-full" disabled={!canSubmit || isSubmitting}>
                    {isSubmitting ? 'Sending…' : 'Send reset link'}
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

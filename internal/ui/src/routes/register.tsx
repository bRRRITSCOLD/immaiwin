import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { useEffect } from 'react'
import { useForm } from '@tanstack/react-form'
import { z } from 'zod'
import { toast } from 'sonner'
import { Button } from '~/components/ui/button'
import { Input } from '~/components/ui/input'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '~/components/ui/field'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '~/components/ui/card'
import { useAuthStore } from '~/lib/auth-store'
import { ApiError } from '~/lib/api'

interface RegisterSearch {
  next?: string
}

export const Route = createFileRoute('/register')({
  component: RegisterPage,
  validateSearch: (raw: Record<string, unknown>): RegisterSearch => ({
    next: typeof raw['next'] === 'string' ? (raw['next'] as string) : undefined,
  }),
})

const emailSchema = z.string().min(1, 'Required').email('Invalid email')
const passwordSchema = z.string().min(8, 'Must be at least 8 characters')

function RegisterPage() {
  const navigate = useNavigate()
  const search = Route.useSearch()
  const me = useAuthStore((s) => s.me)
  const register = useAuthStore((s) => s.register)

  // After register, honour ?next= so flows like /invite/:token →
  // register-with-matching-email → back-to-invite work without manual
  // re-navigation. Default lands on /workflows.
  useEffect(() => {
    if (me) {
      const next = search.next ? decodeURIComponent(search.next) : '/workflows'
      void navigate({ to: next as never, replace: true })
    }
  }, [me, search.next, navigate])

  const form = useForm({
    defaultValues: { email: '', password: '', confirm: '' },
    onSubmit: async ({ value }) => {
      if (value.password !== value.confirm) {
        toast.error('Passwords do not match')
        return
      }
      try {
        await register(value.email.trim().toLowerCase(), value.password)
        toast.success('Account created')
      } catch (err) {
        const msg = err instanceof ApiError ? err.message : 'Registration failed'
        toast.error(msg)
      }
    },
  })

  return (
    <div className="min-h-screen w-full flex items-center justify-center bg-background px-4">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle>Create your immaiwin account</CardTitle>
          <CardDescription>A personal workspace will be created for you automatically.</CardDescription>
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
                      autoComplete="new-password"
                      value={field.state.value}
                      onChange={(e) => field.handleChange(e.target.value)}
                      onBlur={field.handleBlur}
                    />
                    <FieldError errors={field.state.meta.errors as string[]} />
                  </Field>
                )}
              </form.Field>

              <form.Field
                name="confirm"
                validators={{ onChange: ({ value }) => (value.length === 0 ? 'Required' : undefined) }}
              >
                {(field) => (
                  <Field>
                    <FieldLabel htmlFor={field.name}>Confirm password</FieldLabel>
                    <Input
                      id={field.name}
                      type="password"
                      autoComplete="new-password"
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
                    {isSubmitting ? 'Creating…' : 'Create account'}
                  </Button>
                )}
              </form.Subscribe>

              <FieldDescription className="text-center">
                Already registered?{' '}
                <Link to="/login" className="underline hover:text-foreground">
                  Sign in
                </Link>
              </FieldDescription>
            </FieldGroup>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}

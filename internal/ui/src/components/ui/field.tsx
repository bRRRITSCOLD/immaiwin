import * as React from 'react'
import { cn } from '~/lib/utils'
import { Label } from './label'

function FieldGroup({ className, ...props }: React.HTMLAttributes<HTMLDivElement>) {
  return <div className={cn('space-y-4', className)} {...props} />
}

function Field({ className, ...props }: React.HTMLAttributes<HTMLDivElement>) {
  return <div className={cn('space-y-1.5', className)} {...props} />
}

function FieldLabel(props: React.ComponentProps<typeof Label>) {
  return <Label {...props} />
}

function FieldDescription({ className, ...props }: React.HTMLAttributes<HTMLParagraphElement>) {
  return <p className={cn('text-xs text-muted-foreground', className)} {...props} />
}

function FieldError({ errors }: { errors: string[] }) {
  const msg = errors[0]
  if (!msg) return null
  return <p className="text-xs font-medium text-destructive">{msg}</p>
}

export { Field, FieldGroup, FieldLabel, FieldDescription, FieldError }

// TenantSwitcher — header dropdown showing the active tenant + the
// list of tenants the user is a member of. Click to switch.
//
// Behaviour:
//   - Hidden when the user has 0 memberships (shouldn't happen post-
//     register, defensive).
//   - When only 1 membership: renders as a static label (no dropdown).
//   - On switch: calls /auth/switch_tenant which mints a new JWT cookie
//     w/ the new active tenant; store re-hydrates via /auth/me.

import { ChevronsUpDown, Check, Building2, LogOut } from 'lucide-react'
import { useNavigate } from '@tanstack/react-router'
import { toast } from 'sonner'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '~/components/ui/dropdown-menu'
import { Button } from '~/components/ui/button'
import { useAuthStore } from '~/lib/auth-store'

export function TenantSwitcher() {
  const me = useAuthStore((s) => s.me)
  const switchTenant = useAuthStore((s) => s.switchTenant)
  const logout = useAuthStore((s) => s.logout)
  const navigate = useNavigate()

  if (!me) return null
  const memberships = me.memberships ?? []
  if (memberships.length === 0) return null

  const active = memberships.find((m) => m.tenant.id === me.tenant_id) ?? memberships[0]
  const activeName = active?.tenant?.name ?? me.tenant_id ?? '—'

  async function handleSwitch(tenantId: string) {
    if (tenantId === me?.tenant_id) return
    try {
      await switchTenant(tenantId)
      toast.success('Switched tenant')
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Switch failed')
    }
  }

  async function handleLogout() {
    await logout()
    void navigate({ to: '/login' })
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="sm" className="gap-2 max-w-[260px]">
          <Building2 className="size-4 shrink-0" />
          <span className="truncate text-sm">{activeName}</span>
          <ChevronsUpDown className="size-3.5 shrink-0 opacity-50" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="min-w-[240px]">
        <DropdownMenuLabel className="text-xs text-muted-foreground">
          {me.user.email}
        </DropdownMenuLabel>
        <DropdownMenuSeparator />
        <DropdownMenuLabel className="text-xs text-muted-foreground">Tenants</DropdownMenuLabel>
        {memberships.map((m) => (
          <DropdownMenuItem
            key={m.tenant.id}
            onSelect={() => void handleSwitch(m.tenant.id)}
            className="gap-2"
          >
            <Building2 className="size-4 shrink-0" />
            <span className="truncate flex-1">{m.tenant.name || m.tenant.id}</span>
            <span className="text-xs text-muted-foreground">{m.role}</span>
            {m.tenant.id === me.tenant_id && <Check className="size-4 ml-1" />}
          </DropdownMenuItem>
        ))}
        <DropdownMenuSeparator />
        <DropdownMenuItem onSelect={() => void handleLogout()} className="gap-2 text-destructive">
          <LogOut className="size-4" />
          Log out
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

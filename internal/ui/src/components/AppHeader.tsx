// Shared top-of-page header for authed routes. Rendered once from
// __root.tsx (gated by AuthGate's public-path list) so individual
// routes never re-declare nav markup. Active link styling comes from
// TanStack Router's `activeProps`.
//
// Routes that need extra header content (e.g. /workflows showing the
// active workflow name) pass it via `children` — placed left of the
// TenantSwitcher.

import { Link } from '@tanstack/react-router'
import { Separator } from '~/components/ui/separator'
import { TenantSwitcher } from '~/components/TenantSwitcher'

const NAV = [
  { to: '/workflows', label: 'Workflows' },
  { to: '/runs', label: 'Runs' },
  { to: '/evals', label: 'Evals' },
  { to: '/skills', label: 'Skills' },
  { to: '/admin', label: 'Admin' },
  { to: '/settings', label: 'Settings' },
] as const

const ACTIVE = 'text-foreground font-medium'
const INACTIVE = 'text-muted-foreground hover:text-foreground transition-colors'

export function AppHeader({ children }: { children?: React.ReactNode }) {
  return (
    <header className="sticky top-0 z-10 border-b bg-background/90 backdrop-blur-sm px-6 py-3 flex items-center gap-4 shrink-0">
      <h1 className="text-lg font-semibold tracking-tight">immaiwin</h1>
      <Separator orientation="vertical" className="h-5" />
      <nav className="flex items-center gap-3 text-sm">
        {NAV.map((n) => (
          <Link
            key={n.to}
            to={n.to}
            activeProps={{ className: ACTIVE }}
            inactiveProps={{ className: INACTIVE }}
          >
            {n.label}
          </Link>
        ))}
      </nav>
      <div className="ml-auto flex items-center gap-3">
        {children}
        <TenantSwitcher />
      </div>
    </header>
  )
}

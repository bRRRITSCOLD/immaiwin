// AuthGate — root-level wrapper that:
//   1. Hydrates /auth/me on first mount.
//   2. Renders <Outlet/> through children once loaded.
//   3. Redirects unauthenticated users to /login (except for the
//      public auth routes themselves).
//   4. Shows a thin loader while hydrating so we don't flash the
//      protected UI before deciding.
//
// The list of public paths lives here, not in route metadata, so it's
// trivial to skim what's reachable without auth.

import { useEffect } from 'react'
import { useLocation, useNavigate } from '@tanstack/react-router'
import { useAuthStore } from '~/lib/auth-store'

const PUBLIC_PATHS = new Set<string>(['/login', '/register', '/forgot', '/reset'])

export function AuthGate({ children }: { children: React.ReactNode }) {
  const me = useAuthStore((s) => s.me)
  const loadMe = useAuthStore((s) => s.loadMe)
  const location = useLocation()
  const navigate = useNavigate()
  const isPublic = PUBLIC_PATHS.has(location.pathname)

  // Hydrate once on mount.
  useEffect(() => {
    if (me === undefined) {
      void loadMe()
    }
  }, [me, loadMe])

  // Redirect unauth → /login (preserve return-to as ?next=).
  useEffect(() => {
    if (me === null && !isPublic) {
      const next = encodeURIComponent(location.pathname)
      void navigate({ to: '/login', search: { next } as never })
    }
  }, [me, isPublic, location.pathname, navigate])

  // Initial load — block paint so the protected UI doesn't flash.
  if (me === undefined && !isPublic) {
    return (
      <div className="h-screen w-screen flex items-center justify-center bg-background text-muted-foreground text-sm">
        loading…
      </div>
    )
  }

  // Unauthed on a protected route — render nothing while the redirect
  // useEffect lands. (Returning the loader briefly is fine too.)
  if (me === null && !isPublic) return null

  return <>{children}</>
}

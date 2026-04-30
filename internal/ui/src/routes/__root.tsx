import {
  createRootRoute,
  HeadContent,
  Outlet,
  ScrollRestoration,
  Scripts,
  useLocation,
} from '@tanstack/react-router'
import { Toaster } from '~/components/ui/sonner'
import { TooltipProvider } from '~/components/ui/tooltip'
import { AuthGate, isPublicPath } from '~/components/AuthGate'
import { AppHeader } from '~/components/AppHeader'
import { installCredentialsDefault } from '~/lib/api'
import '../styles.css'

// Patch the global fetch so every request sends cookies. Without this
// the auth cookie is dropped on cross-origin requests and every
// protected route 401s. Side-effect import is intentional.
installCredentialsDefault()

export const Route = createRootRoute({
  head: () => ({
    meta: [
      { charSet: 'utf-8' },
      { name: 'viewport', content: 'width=device-width, initial-scale=1' },
      { title: 'immaiwin' },
    ],
  }),
  component: RootComponent,
})

function RootComponent() {
  return (
    <html lang="en" className="dark">
      <head>
        <HeadContent />
      </head>
      <body>
        <TooltipProvider>
          <AuthGate>
            <Layout />
          </AuthGate>
        </TooltipProvider>
        <Toaster />
        <ScrollRestoration />
        <Scripts />
      </body>
    </html>
  )
}

// Layout decides whether to render the shared AppHeader. Public routes
// (login/register/forgot/reset/invite) own their layout; everything
// else gets the standard sticky header above the route Outlet.
//
// Routes that want extra right-side header content (e.g. /workflows
// showing the active workflow name) render their own AppHeader with
// children — they opt out of the layout-rendered one by setting
// `data-no-app-header` somewhere is overkill. We keep the layout
// header always-on and let routes append via ad-hoc children later
// if needed.
function Layout() {
  const location = useLocation()
  if (isPublicPath(location.pathname)) {
    return <Outlet />
  }
  // h-screen + flex-col so routes can opt into viewport-fit layouts
  // by setting `flex-1 min-h-0` on their own root. Routes that want
  // body scroll add `overflow-y-auto`; routes that want a viewport-
  // fit pane (workflow canvas) add `overflow-hidden flex`. The Layout
  // itself imposes no overflow behavior.
  return (
    <div className="h-screen flex flex-col bg-background text-foreground">
      <AppHeader />
      <Outlet />
    </div>
  )
}

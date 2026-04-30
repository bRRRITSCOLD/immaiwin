import {
  createRootRoute,
  HeadContent,
  Outlet,
  ScrollRestoration,
  Scripts,
} from '@tanstack/react-router'
import { Toaster } from '~/components/ui/sonner'
import { TooltipProvider } from '~/components/ui/tooltip'
import { AuthGate } from '~/components/AuthGate'
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
            <Outlet />
          </AuthGate>
        </TooltipProvider>
        <Toaster />
        <ScrollRestoration />
        <Scripts />
      </body>
    </html>
  )
}

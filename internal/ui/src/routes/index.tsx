// Root path redirects to /workflows. The standalone Polymarket
// dashboard that previously lived here was removed — Polymarket now
// only exists as a workflow integration (trigger nodes + connections).
import { createFileRoute, redirect } from '@tanstack/react-router'

export const Route = createFileRoute('/')({
  beforeLoad: () => {
    throw redirect({ to: '/workflows' })
  },
})

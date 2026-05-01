// /approve — magic-link approval landing page.
//
// Reached from email / Slack notifications dispatched on a pending
// approval gate (Stage 2 OOB approvals). Loaded with two query params:
//
//   /approve?run_id=<rid>&token=<jwt>
//
// The page is unauthenticated — the signed token IS the auth. Submitting
// Approve / Reject POSTs to `/api/v1/workflow_runs/:id/approval/redeem`,
// which validates the token + persists the decision. PR-a ships this
// route + the redeem endpoint; PR-b adds the dispatcher that emits
// the link in the first place.

import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { Button } from '~/components/ui/button'
import { Input } from '~/components/ui/input'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '~/components/ui/card'

export const Route = createFileRoute('/approve')({
  component: ApprovePage,
  validateSearch: (search: Record<string, unknown>) => ({
    run_id: typeof search.run_id === 'string' ? search.run_id : '',
    token: typeof search.token === 'string' ? search.token : '',
  }),
})

type ResultState =
  | { kind: 'idle' }
  | { kind: 'submitting' }
  | { kind: 'success'; decision: 'approve' | 'reject' }
  | { kind: 'error'; status: number; message: string }

function ApprovePage() {
  const { run_id: runID, token } = Route.useSearch()
  const [reason, setReason] = useState('')
  const [state, setState] = useState<ResultState>({ kind: 'idle' })

  async function submit(decision: 'approve' | 'reject') {
    if (!runID || !token) {
      setState({ kind: 'error', status: 400, message: 'Missing run_id or token in the link.' })
      return
    }
    setState({ kind: 'submitting' })
    try {
      // Use a raw fetch — the typed `api.post` helper assumes the
      // session cookie path; this endpoint is unauthenticated and
      // shouldn't have any cookie tossed at it.
      const res = await fetch(`/api/v1/workflow_runs/${encodeURIComponent(runID)}/approval/redeem`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token, decision, reason }),
      })
      if (!res.ok) {
        let msg = `HTTP ${res.status}`
        try {
          const body = await res.json()
          if (body && typeof body.error === 'string') msg = body.error
        } catch {
          // Body wasn't JSON — keep the HTTP status as the message.
        }
        setState({ kind: 'error', status: res.status, message: msg })
        return
      }
      setState({ kind: 'success', decision })
    } catch (err) {
      setState({ kind: 'error', status: 0, message: err instanceof Error ? err.message : 'Network error' })
    }
  }

  if (state.kind === 'success') {
    return (
      <div className="min-h-screen w-full flex items-center justify-center bg-background px-4">
        <Card className="w-full max-w-md">
          <CardHeader>
            <CardTitle>Decision recorded</CardTitle>
            <CardDescription>
              The run has been {state.decision === 'approve' ? 'approved' : 'rejected'} and will resume momentarily.
              You can close this page.
            </CardDescription>
          </CardHeader>
        </Card>
      </div>
    )
  }

  return (
    <div className="min-h-screen w-full flex items-center justify-center bg-background px-4">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle>Workflow approval</CardTitle>
          <CardDescription>
            A workflow run is paused waiting for your decision.
            <span className="block mt-1 text-xs text-muted-foreground font-mono">run: {runID || '(missing)'}</span>
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div>
            <label className="text-sm font-medium block mb-1" htmlFor="approve-reason">Reason (optional)</label>
            <Input
              id="approve-reason"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              placeholder="e.g. signed off by ops"
              disabled={state.kind === 'submitting'}
            />
          </div>
          {state.kind === 'error' && (
            <div className="text-sm text-destructive">
              <strong>Could not record your decision.</strong>
              <span className="block">{state.message}</span>
              {state.status === 410 && (
                <span className="block mt-1 text-xs text-muted-foreground">
                  This link has expired or has already been used.
                </span>
              )}
            </div>
          )}
          <div className="flex gap-2">
            <Button
              variant="default"
              className="flex-1"
              disabled={state.kind === 'submitting' || !runID || !token}
              onClick={() => submit('approve')}
            >
              Approve
            </Button>
            <Button
              variant="destructive"
              className="flex-1"
              disabled={state.kind === 'submitting' || !runID || !token}
              onClick={() => submit('reject')}
            >
              Reject
            </Button>
          </div>
        </CardContent>
      </Card>
    </div>
  )
}

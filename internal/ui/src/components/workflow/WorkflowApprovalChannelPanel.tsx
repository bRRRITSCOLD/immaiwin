// WorkflowApprovalChannelPanel — collapsible per-workflow channel
// picker for OOB approval prompts (Stage 2).
//
// Wires `Workflow.approval_channel.{type, target, from?}` (see Go
// `workflow.ApprovalChannel`). When a `require_node_approval` or
// per-tool `require_approval` gate fires on a server-side run, the
// executor's dispatcher reads this field and posts to the configured
// transport (SMTP / Slack incoming-webhook). Channel = `none` (default)
// means "pause the run but don't notify externally" — the /runs UI
// remains the only resolution path.
//
// Save lives on the existing toolbar Save button. We mutate the active
// workflow doc via `updateActiveApprovalChannel`; the workflow's PUT
// payload picks the field up via the standard spread.

import { useState } from 'react'
import { ChevronDown, ChevronUp, Bell, Save } from 'lucide-react'
import { Button } from '~/components/ui/button'
import { Input } from '~/components/ui/input'
import { useWorkflowStore, type ApprovalChannel } from './useWorkflowStore'

interface Props {
  channel: ApprovalChannel | null | undefined
  onChange(channel: ApprovalChannel | null): void
  onSave(): void
}

const CHANNEL_TYPES: { value: ApprovalChannel['type']; label: string; targetLabel: string; targetPlaceholder: string }[] = [
  { value: 'none', label: 'None (paused, no notification)', targetLabel: '', targetPlaceholder: '' },
  { value: 'smtp', label: 'Email (SMTP)', targetLabel: 'Recipient email', targetPlaceholder: 'ops@example.com' },
  { value: 'slack_webhook', label: 'Slack incoming-webhook (legacy)', targetLabel: 'Webhook URL', targetPlaceholder: 'https://hooks.slack.com/services/...' },
  { value: 'slack_bot', label: 'Slack bot (chat.postMessage)', targetLabel: 'Slack connection', targetPlaceholder: 'select a slack connection' },
]

export function WorkflowApprovalChannelPanel({ channel, onChange, onSave }: Props) {
  const [open, setOpen] = useState(false)
  const connections = useWorkflowStore((s) => s.connections)
  const type = channel?.type ?? 'none'
  const active = type !== 'none'

  function setType(next: ApprovalChannel['type']) {
    if (next === 'none') {
      // Persist as null so the BSON doc stays slim. The dispatcher
      // treats both nil and Type:"none" as no-op.
      onChange(null)
    } else {
      onChange({
        type: next,
        target: channel?.target ?? '',
        from: channel?.from,
        channel: channel?.channel,
      })
    }
  }

  function setTarget(target: string) {
    if (!channel || channel.type === 'none') return
    onChange({ ...channel, target })
  }

  function setFrom(from: string) {
    if (!channel || channel.type === 'none') return
    onChange({ ...channel, from: from || undefined })
  }

  function setChannel(slackChannel: string) {
    if (!channel || channel.type === 'none') return
    onChange({ ...channel, channel: slackChannel || undefined })
  }

  const meta = CHANNEL_TYPES.find((c) => c.value === type) ?? CHANNEL_TYPES[0]
  const slackConnections = connections.filter((c) => c.type === 'slack')

  return (
    <div
      className="rounded-lg border bg-card text-card-foreground shadow-md overflow-hidden"
      style={{ minWidth: 260, maxWidth: 700, width: 320 }}
    >
      <button
        className="flex items-center justify-between w-full px-3 py-2 text-xs font-medium hover:bg-muted/50 transition-colors"
        onClick={() => setOpen((v) => !v)}
      >
        <span className="flex items-center gap-1.5">
          <Bell className="h-3.5 w-3.5" />
          Approval channel {active && <span className="text-amber-600 dark:text-amber-400">·</span>}
        </span>
        {open ? <ChevronUp className="h-3.5 w-3.5" /> : <ChevronDown className="h-3.5 w-3.5" />}
      </button>

      {open && (
        <div className="border-t px-3 py-2 space-y-2">
          <p className="text-[10px] text-muted-foreground">
            Where to send the magic-link prompt when a <code className="text-[10px]">require_node_approval</code>
            {' '}or per-tool <code className="text-[10px]">require_approval</code> gate fires. The /runs page
            still works as a fallback regardless.
          </p>

          <label className="flex items-center justify-between gap-2 text-[11px]">
            <span className="shrink-0 w-[60px]">Type</span>
            <select
              className="h-6 text-[11px] flex-1 px-2 rounded border bg-background"
              value={type}
              onChange={(e) => setType(e.target.value as ApprovalChannel['type'])}
            >
              {CHANNEL_TYPES.map((c) => (
                <option key={c.value} value={c.value}>{c.label}</option>
              ))}
            </select>
          </label>

          {active && type !== 'slack_bot' && (
            <>
              <label className="flex items-center justify-between gap-2 text-[11px]">
                <span className="shrink-0 w-[60px]">{meta?.targetLabel ?? 'Target'}</span>
                <Input
                  className="h-6 text-[11px] flex-1 px-2"
                  value={channel?.target ?? ''}
                  placeholder={meta?.targetPlaceholder}
                  onChange={(e) => setTarget(e.target.value)}
                />
              </label>

              {type === 'smtp' && (
                <label className="flex items-center justify-between gap-2 text-[11px]">
                  <span className="shrink-0 w-[60px]">From</span>
                  <Input
                    className="h-6 text-[11px] flex-1 px-2"
                    value={channel?.from ?? ''}
                    placeholder="(default sender)"
                    onChange={(e) => setFrom(e.target.value)}
                  />
                </label>
              )}
            </>
          )}

          {active && type === 'slack_bot' && (
            <>
              <label className="flex items-center justify-between gap-2 text-[11px]">
                <span className="shrink-0 w-[60px]">Connection</span>
                <select
                  className="h-6 text-[11px] flex-1 px-2 rounded border bg-background"
                  value={channel?.target ?? ''}
                  onChange={(e) => setTarget(e.target.value)}
                >
                  <option value="">— select a Slack connection —</option>
                  {slackConnections.map((c) => (
                    <option key={c.id} value={c.id}>{c.name}</option>
                  ))}
                </select>
              </label>
              {slackConnections.length === 0 && (
                <p className="text-[10px] text-amber-600 dark:text-amber-400">
                  No Slack connections yet. Add one via Connections sidebar (type: Slack).
                </p>
              )}
              <label className="flex items-center justify-between gap-2 text-[11px]">
                <span className="shrink-0 w-[60px]">Channel</span>
                <Input
                  className="h-6 text-[11px] flex-1 px-2"
                  value={channel?.channel ?? ''}
                  placeholder="C01234ABCDE (or DM user ID)"
                  onChange={(e) => setChannel(e.target.value)}
                />
              </label>
              <p className="text-[10px] text-muted-foreground">
                Empty channel falls back to the Slack connection's <code className="text-[10px]">default_channel</code>.
              </p>
            </>
          )}

          <Button
            variant="secondary"
            size="sm"
            className="w-full h-7 text-[11px]"
            onClick={onSave}
          >
            <Save className="h-3 w-3 mr-1" />
            Save approval channel
          </Button>
        </div>
      )}
    </div>
  )
}

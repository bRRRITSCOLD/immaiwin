import { NodeResizer, useReactFlow, type NodeProps } from '@xyflow/react'
import { Play } from 'lucide-react'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '~/components/ui/select'
import { Input } from '~/components/ui/input'
import { Textarea } from '~/components/ui/textarea'
import { Checkbox } from '~/components/ui/checkbox'
import { StepNameInput } from './StepNameInput'
import { DynamicHandles } from './DynamicHandles'
import { ConnectionPicker } from './ConnectionPicker'
import { NodeDebugPanel, BreakpointMarker } from '../RunResultsContext'

const triggerTypes = [
  { value: 'manual', label: 'Manual' },
  { value: 'cron', label: 'Cron Schedule' },
  { value: 'webhook', label: 'Webhook (HTTP)' },
  { value: 'rabbitmq', label: 'RabbitMQ' },
  { value: 'redis_subscribe', label: 'Redis Subscribe' },
  { value: 'websocket', label: 'WebSocket' },
] as const

type TriggerType = (typeof triggerTypes)[number]['value']

const cronFields = [
  // Seconds is optional — leave blank for standard 5-field cron
  // (minute granularity). Fill in for sub-minute scheduling.
  { key: 'cron_sec', label: 'Second', placeholder: '(blank)' },
  { key: 'cron_min', label: 'Minute', placeholder: '*' },
  { key: 'cron_hour', label: 'Hour', placeholder: '*' },
  { key: 'cron_dom', label: 'Day of Month', placeholder: '*' },
  { key: 'cron_mon', label: 'Month', placeholder: '*' },
  { key: 'cron_dow', label: 'Day of Week', placeholder: '*' },
] as const

// parseCronToFields splits a cron string into per-field state. Accepts
// either 5 fields (legacy) or 6 fields (with leading seconds). For 5
// fields, cron_sec stays empty so the rendered form matches the
// minute-granularity input.
function parseCronToFields(cron: string): Record<string, string> {
  const parts = (cron || '').trim().split(/\s+/)
  if (parts.length === 6) {
    return {
      cron_sec: parts[0] || '',
      cron_min: parts[1] || '*',
      cron_hour: parts[2] || '*',
      cron_dom: parts[3] || '*',
      cron_mon: parts[4] || '*',
      cron_dow: parts[5] || '*',
    }
  }
  return {
    cron_sec: '',
    cron_min: parts[0] || '*',
    cron_hour: parts[1] || '*',
    cron_dom: parts[2] || '*',
    cron_mon: parts[3] || '*',
    cron_dow: parts[4] || '*',
  }
}

// buildCronFromData emits a 6-field expression when cron_sec is set,
// otherwise a 5-field expression so existing minute-only schedules
// stay readable. The backend's parser accepts either via
// `cron.SecondOptional`.
function buildCronFromData(data: Record<string, unknown>): string {
  const sec = ((data.cron_sec as string) || '').trim()
  const fields = [
    (data.cron_min as string) || '*',
    (data.cron_hour as string) || '*',
    (data.cron_dom as string) || '*',
    (data.cron_mon as string) || '*',
    (data.cron_dow as string) || '*',
  ]
  if (sec !== '') {
    return [sec, ...fields].join(' ')
  }
  return fields.join(' ')
}

export function TriggerNode({ id, data, selected }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const triggerType = ((data.trigger_type as string) || 'manual') as TriggerType
  const isCron = triggerType === 'cron'
  const isWebhook = triggerType === 'webhook'
  const isRabbitMQ = triggerType === 'rabbitmq'
  const isRedisSubscribe = triggerType === 'redis_subscribe'
  const isWebSocket = triggerType === 'websocket'

  // Sync individual fields from legacy `cron` string on first render
  const hasFields = cronFields.some((f) => data[f.key] != null)
  if (isCron && !hasFields && data.cron) {
    const fields = parseCronToFields(data.cron as string)
    // schedule update for next tick to avoid render-time setState
    queueMicrotask(() => updateNodeData(id, { ...fields, cron: undefined }))
  }

  function updateCronField(key: string, value: string) {
    const updated = { ...data, [key]: value }
    updateNodeData(id, { [key]: value, cron: buildCronFromData(updated) })
  }

  return (
    <div className="relative min-w-[260px] h-full">
      <BreakpointMarker id={id} />
      <div className="overflow-x-hidden rounded-lg border-2 border-blue-500 bg-card text-card-foreground shadow-sm h-full">
        <NodeResizer minWidth={260} minHeight={80} isVisible={selected} />
        <div className="flex items-center gap-2 px-4 py-3 border-b border-blue-500/40">
          <Play className="h-4 w-4 text-blue-500 shrink-0" />
          <span className="text-sm font-medium">Trigger</span>
          {isRabbitMQ && (
            <div className="ml-auto">
              <ConnectionPicker nodeId={id} connectionType="rabbitmq" data={data as Record<string, unknown>} activeColor="text-blue-500" requireExplicit />
            </div>
          )}
          {isRedisSubscribe && (
            <div className="ml-auto">
              <ConnectionPicker nodeId={id} connectionType="redis" data={data as Record<string, unknown>} activeColor="text-blue-500" requireExplicit />
            </div>
          )}
          {isWebSocket && (
            <div className="ml-auto">
              <ConnectionPicker nodeId={id} connectionType="websocket" data={data as Record<string, unknown>} activeColor="text-blue-500" requireExplicit />
            </div>
          )}
        </div>
        <StepNameInput id={id} data={data} />
        <div className="px-4 py-3 space-y-3">
          <div className="space-y-1">
            <label className="text-xs text-muted-foreground">Type</label>
            <Select
              value={triggerType}
              onValueChange={(v) => updateNodeData(id, { trigger_type: v })}
            >
              <SelectTrigger className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {triggerTypes.map((t) => (
                  <SelectItem key={t.value} value={t.value}>
                    {t.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          {isCron && (
            <div className="space-y-2">
              {cronFields.map((f) => (
                <div key={f.key} className="space-y-0.5">
                  <label className="text-xs text-muted-foreground">{f.label}</label>
                  <Input
                    className="font-mono text-sm h-8"
                    placeholder={f.placeholder}
                    value={(data[f.key] as string) ?? '*'}
                    onChange={(e) => updateCronField(f.key, e.target.value)}
                  />
                </div>
              ))}
              <div className="flex items-center gap-2 pt-1">
                <Checkbox
                  id={`${id}-skip-if-running`}
                  checked={data.skip_if_running !== false}
                  onCheckedChange={(v) => updateNodeData(id, { skip_if_running: !!v })}
                />
                <label htmlFor={`${id}-skip-if-running`} className="text-xs text-muted-foreground cursor-pointer">
                  Skip if already running
                </label>
              </div>
            </div>
          )}
          {isWebhook && (
            <WebhookFields id={id} data={data} updateNodeData={updateNodeData} />
          )}
          {isRabbitMQ && (
            <div className="space-y-2">
              <div className="space-y-0.5">
                <label className="text-xs text-muted-foreground">Queue</label>
                <Input
                  className="font-mono text-sm h-8"
                  placeholder="my-queue"
                  value={(data.queue as string) ?? ''}
                  onChange={(e) => updateNodeData(id, { queue: e.target.value })}
                />
              </div>
              <div className="space-y-0.5">
                <label className="text-xs text-muted-foreground">Prefetch Count</label>
                <Input
                  className="font-mono text-sm h-8"
                  type="number"
                  min={1}
                  placeholder="1"
                  value={(data.prefetch as number) ?? 1}
                  onChange={(e) => updateNodeData(id, { prefetch: parseInt(e.target.value) || 1 })}
                />
              </div>
              <div className="flex items-center gap-2 pt-1">
                <Checkbox
                  id={`${id}-auto-ack`}
                  checked={!!data.auto_ack}
                  onCheckedChange={(v) => updateNodeData(id, { auto_ack: !!v })}
                />
                <label htmlFor={`${id}-auto-ack`} className="text-xs text-muted-foreground cursor-pointer">
                  Auto-acknowledge
                </label>
              </div>
            </div>
          )}
          {isWebSocket && (
            <div className="space-y-2">
              <div className="space-y-0.5">
                <label className="text-xs text-muted-foreground">Subscribe payload (optional)</label>
                <Textarea
                  className="nodrag font-mono text-xs min-h-[60px]"
                  placeholder={'JSON sent once on connect\ne.g. {"action":"subscribe","channel":"trades"}'}
                  value={(data.subscribe_payload as string) ?? ''}
                  onChange={(e) => updateNodeData(id, { subscribe_payload: e.target.value })}
                />
                <p className="text-[10px] text-muted-foreground">
                  Provider's subscribe handshake. Skipped when empty.
                </p>
              </div>
              <div className="space-y-0.5">
                <label className="text-xs text-muted-foreground">Event path (optional)</label>
                <input
                  className="nodrag w-full bg-background border border-border rounded px-2 py-1 text-xs font-mono"
                  placeholder="e.g. data.payload"
                  value={(data.event_path as string) ?? ''}
                  onChange={(e) => updateNodeData(id, { event_path: e.target.value })}
                />
                <p className="text-[10px] text-muted-foreground">
                  Dot-path into a JSON frame. Exposed to the workflow as{' '}
                  <code className="text-[10px]">{'{{input.extracted}}'}</code>.
                  Empty = whole frame on{' '}
                  <code className="text-[10px]">{'{{input.json}}'}</code> /{' '}
                  <code className="text-[10px]">{'{{input.raw}}'}</code>.
                </p>
              </div>
              <div className="space-y-0.5">
                <label className="text-xs text-muted-foreground">Heartbeat (seconds)</label>
                <input
                  type="number"
                  min={0}
                  className="nodrag w-full bg-background border border-border rounded px-2 py-1 text-xs font-mono"
                  placeholder="30"
                  value={(data.heartbeat_seconds as number | undefined) ?? ''}
                  onChange={(e) => {
                    const n = parseInt(e.target.value, 10)
                    updateNodeData(id, { heartbeat_seconds: Number.isFinite(n) ? n : undefined })
                  }}
                />
                <p className="text-[10px] text-muted-foreground">
                  Ping interval. 0 disables. Idle servers are detected within ~2× this.
                </p>
              </div>
            </div>
          )}
          {isRedisSubscribe && (
            <div className="space-y-2">
              <div className="space-y-0.5">
                <label className="text-xs text-muted-foreground">Channels</label>
                <Textarea
                  className="nodrag font-mono text-xs min-h-[60px]"
                  placeholder="One channel per line"
                  value={(data.channels as string) ?? ''}
                  onChange={(e) => updateNodeData(id, { channels: e.target.value })}
                />
                <p className="text-[10px] text-muted-foreground">
                  Exact channel match. Each message triggers a workflow run with{' '}
                  <code className="text-[10px]">{'{{input.channel}}'}</code>,{' '}
                  <code className="text-[10px]">{'{{input.payload}}'}</code> available.
                </p>
              </div>
              <div className="space-y-0.5">
                <label className="text-xs text-muted-foreground">Patterns</label>
                <Textarea
                  className="nodrag font-mono text-xs min-h-[60px]"
                  placeholder={'One glob pattern per line\ne.g. burrow:news:*'}
                  value={(data.patterns as string) ?? ''}
                  onChange={(e) => updateNodeData(id, { patterns: e.target.value })}
                />
                <p className="text-[10px] text-muted-foreground">
                  Optional glob patterns (Redis PSUBSCRIBE). Mix freely with channels above.
                </p>
              </div>
            </div>
          )}
        </div>
        <NodeDebugPanel id={id} />
      </div>
      <DynamicHandles nodeId={id} nodeType="trigger" data={data as Record<string, unknown>} />
    </div>
  )
}

// WebhookFields renders the webhook trigger config: a slug (auto-
// suggested when empty), an optional shared secret for HMAC SHA-256
// signature verification, and a copy-friendly URL preview so the
// integrator knows where to POST.
function WebhookFields({
  id,
  data,
  updateNodeData,
}: {
  id: string
  data: Record<string, unknown>
  updateNodeData: (id: string, patch: Record<string, unknown>) => void
}) {
  const slug = (data.webhook_slug as string) ?? ''
  const secret = (data.webhook_secret as string) ?? ''
  // Read API base from Vite env. Fallback to relative URL if missing.
  const apiBase = (import.meta.env['VITE_API_URL'] as string | undefined) ?? ''
  const url = slug ? `${apiBase}/api/v1/webhooks/${slug}` : ''
  return (
    <div className="space-y-2">
      <div className="space-y-0.5">
        <label className="text-xs text-muted-foreground">Slug</label>
        <Input
          className="font-mono text-sm h-8"
          placeholder="my-webhook"
          value={slug}
          onChange={(e) => updateNodeData(id, { webhook_slug: e.target.value.trim() })}
        />
      </div>
      {url && (
        <div className="space-y-0.5">
          <label className="text-xs text-muted-foreground">POST URL</label>
          <div className="flex items-center gap-1">
            <Input className="font-mono text-[11px] h-8 flex-1" readOnly value={url} />
            <button
              type="button"
              className="nodrag text-[10px] px-2 py-1 rounded border border-border hover:bg-muted/50 transition-colors"
              onClick={() => navigator.clipboard.writeText(url)}
              title="Copy URL"
            >
              Copy
            </button>
          </div>
        </div>
      )}
      <div className="space-y-0.5">
        <label className="text-xs text-muted-foreground">Secret (optional)</label>
        <Input
          className="font-mono text-sm h-8"
          type="password"
          placeholder="leave blank for unauthenticated"
          value={secret}
          onChange={(e) => updateNodeData(id, { webhook_secret: e.target.value })}
        />
        {secret && (
          <p className="text-[10px] text-muted-foreground">
            Sender must include <code className="text-[10px]">X-Webhook-Signature: sha256=&lt;hex&gt;</code> over the raw body.
          </p>
        )}
      </div>
    </div>
  )
}

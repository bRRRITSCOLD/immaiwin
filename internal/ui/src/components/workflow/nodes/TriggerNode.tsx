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
  { value: 'rabbitmq', label: 'RabbitMQ' },
  { value: 'redis_subscribe', label: 'Redis Subscribe' },
  { value: 'polymarket_ws', label: 'Polymarket WS' },
  { value: 'schwab_ws', label: 'Schwab WS' },
] as const

type TriggerType = (typeof triggerTypes)[number]['value']

const cronFields = [
  { key: 'cron_min', label: 'Minute', placeholder: '*' },
  { key: 'cron_hour', label: 'Hour', placeholder: '*' },
  { key: 'cron_dom', label: 'Day of Month', placeholder: '*' },
  { key: 'cron_mon', label: 'Month', placeholder: '*' },
  { key: 'cron_dow', label: 'Day of Week', placeholder: '*' },
] as const

function parseCronToFields(cron: string): Record<string, string> {
  const parts = (cron || '').trim().split(/\s+/)
  return {
    cron_min: parts[0] || '*',
    cron_hour: parts[1] || '*',
    cron_dom: parts[2] || '*',
    cron_mon: parts[3] || '*',
    cron_dow: parts[4] || '*',
  }
}

function buildCronFromData(data: Record<string, unknown>): string {
  return [
    (data.cron_min as string) || '*',
    (data.cron_hour as string) || '*',
    (data.cron_dom as string) || '*',
    (data.cron_mon as string) || '*',
    (data.cron_dow as string) || '*',
  ].join(' ')
}

export function TriggerNode({ id, data, selected }: NodeProps) {
  const { updateNodeData } = useReactFlow()
  const triggerType = ((data.trigger_type as string) || 'manual') as TriggerType
  const isCron = triggerType === 'cron'
  const isRabbitMQ = triggerType === 'rabbitmq'
  const isRedisSubscribe = triggerType === 'redis_subscribe'
  const isPolymarketWS = triggerType === 'polymarket_ws'
  const isSchwabWS = triggerType === 'schwab_ws'

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
              <ConnectionPicker nodeId={id} connectionType="rabbitmq" data={data as Record<string, unknown>} activeColor="text-blue-500" />
            </div>
          )}
          {isRedisSubscribe && (
            <div className="ml-auto">
              <ConnectionPicker nodeId={id} connectionType="redis" data={data as Record<string, unknown>} activeColor="text-blue-500" />
            </div>
          )}
          {isPolymarketWS && (
            <div className="ml-auto">
              <ConnectionPicker nodeId={id} connectionType="polymarket" data={data as Record<string, unknown>} activeColor="text-blue-500" />
            </div>
          )}
          {isSchwabWS && (
            <div className="ml-auto">
              <ConnectionPicker nodeId={id} connectionType="schwab" data={data as Record<string, unknown>} activeColor="text-blue-500" />
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
                  placeholder={'One glob pattern per line\ne.g. immaiwin:news:*'}
                  value={(data.patterns as string) ?? ''}
                  onChange={(e) => updateNodeData(id, { patterns: e.target.value })}
                />
                <p className="text-[10px] text-muted-foreground">
                  Optional glob patterns (Redis PSUBSCRIBE). Mix freely with channels above.
                </p>
              </div>
            </div>
          )}
          {isPolymarketWS && (
            <div className="space-y-2">
              <div className="space-y-0.5">
                <label className="text-xs text-muted-foreground">Asset IDs</label>
                <Textarea
                  className="nodrag font-mono text-xs min-h-[60px]"
                  placeholder="One CLOB token ID per line"
                  value={(data.asset_ids as string) ?? ''}
                  onChange={(e) => updateNodeData(id, { asset_ids: e.target.value })}
                />
                <p className="text-[10px] text-muted-foreground">One CLOB token ID per line. Each trade event triggers a workflow run.</p>
              </div>
            </div>
          )}
          {isSchwabWS && (
            <div className="space-y-2">
              <div className="space-y-1">
                <label className="text-xs text-muted-foreground">Stream Type</label>
                <Select
                  value={(data.stream_type as string) || 'options'}
                  onValueChange={(v) => updateNodeData(id, { stream_type: v })}
                >
                  <SelectTrigger className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="options">Options</SelectItem>
                    <SelectItem value="futures">Futures</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-0.5">
                <label className="text-xs text-muted-foreground">Symbols</label>
                <Textarea
                  className="nodrag font-mono text-xs min-h-[60px]"
                  placeholder={
                    (data.stream_type as string) === 'futures'
                      ? 'One contract per line\ne.g. /CLM25'
                      : 'One option key per line\ne.g. SPY_020725C400'
                  }
                  value={(data.symbols as string) ?? ''}
                  onChange={(e) => updateNodeData(id, { symbols: e.target.value })}
                />
                <p className="text-[10px] text-muted-foreground">One symbol per line. Each trade event triggers a workflow run.</p>
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

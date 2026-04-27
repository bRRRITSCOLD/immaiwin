import { useEffect, useRef, useState } from 'react'

const API_BASE = import.meta.env['VITE_API_URL'] ?? 'http://localhost:8080'

interface CapturedMessage {
  id: number
  timestamp: string
  data: unknown
  raw: string
}

interface WSPreviewPanelProps {
  workflowId: string
  onRunWithInput: (input: unknown) => Promise<void>
  onClose: () => void
}

let msgId = 0

export function WSPreviewPanel({ workflowId, onRunWithInput, onClose }: WSPreviewPanelProps) {
  const [status, setStatus] = useState<'connecting' | 'connected' | 'disconnected' | 'error'>('connecting')
  const [messages, setMessages] = useState<CapturedMessage[]>([])
  const [paused, setPaused] = useState(false)
  const [runningId, setRunningId] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)
  const pausedRef = useRef(false)

  useEffect(() => {
    pausedRef.current = paused
  }, [paused])

  useEffect(() => {
    const es = new EventSource(`${API_BASE}/api/v1/workflows/${workflowId}/ws-preview`)

    es.addEventListener('status', () => {
      setStatus('connected')
      setError(null)
    })

    es.addEventListener('message', (e) => {
      if (pausedRef.current) return
      try {
        const parsed = JSON.parse(e.data)
        const msg: CapturedMessage = {
          id: ++msgId,
          timestamp: new Date().toLocaleTimeString(),
          data: parsed,
          raw: e.data,
        }
        setMessages((prev) => [msg, ...prev].slice(0, 100))
      } catch {
        // ignore malformed
      }
    })

    es.addEventListener('error', (e) => {
      if (e instanceof MessageEvent && e.data) {
        try {
          const d = JSON.parse(e.data)
          setError(d.error)
        } catch {
          setError('Connection error')
        }
      }
      setStatus('error')
    })

    es.onopen = () => {
      setStatus('connected')
      setError(null)
    }

    es.onerror = () => {
      if (es.readyState === EventSource.CLOSED) {
        setStatus('disconnected')
      }
    }

    return () => es.close()
  }, [workflowId])

  async function handleRun(msg: CapturedMessage) {
    setRunningId(msg.id)
    try {
      await onRunWithInput(msg.data)
    } finally {
      setRunningId(null)
    }
  }

  const statusColor = {
    connecting: 'bg-yellow-500',
    connected: 'bg-green-500',
    disconnected: 'bg-gray-500',
    error: 'bg-red-500',
  }[status]

  return (
    <div className="border-t border-border bg-background flex flex-col max-h-64">
      {/* Header */}
      <div className="flex items-center gap-3 px-3 py-2 border-b border-border shrink-0">
        <div className="flex items-center gap-2 text-xs">
          <span className={`inline-block w-2 h-2 rounded-full ${statusColor}`} />
          <span className="text-muted-foreground capitalize">{status}</span>
        </div>
        {error && <span className="text-xs text-red-400 truncate max-w-xs">{error}</span>}
        <span className="text-xs text-muted-foreground">{messages.length} msgs</span>
        <div className="ml-auto flex gap-2">
          <button
            onClick={() => setPaused((v) => !v)}
            className="text-xs rounded px-2 py-1 bg-muted text-muted-foreground hover:bg-muted/80 transition-colors"
          >
            {paused ? 'Resume' : 'Pause'}
          </button>
          <button
            onClick={() => setMessages([])}
            className="text-xs rounded px-2 py-1 bg-muted text-muted-foreground hover:bg-muted/80 transition-colors"
          >
            Clear
          </button>
          <button
            onClick={onClose}
            className="text-xs rounded px-2 py-1 bg-muted text-muted-foreground hover:bg-muted/80 transition-colors"
          >
            Close
          </button>
        </div>
      </div>
      {/* Messages */}
      <div className="overflow-y-auto flex-1 text-xs font-mono">
        {messages.length === 0 && (
          <div className="px-3 py-4 text-center text-muted-foreground">
            {status === 'connecting' ? 'Connecting...' : 'Waiting for messages...'}
          </div>
        )}
        {messages.map((msg) => (
          <div
            key={msg.id}
            className="flex items-center gap-2 px-3 py-1.5 border-b border-border/50 hover:bg-muted/30"
          >
            <span className="text-muted-foreground shrink-0 w-16">{msg.timestamp}</span>
            <span className="truncate flex-1 text-foreground">{msg.raw.slice(0, 200)}</span>
            <button
              onClick={() => handleRun(msg)}
              disabled={runningId !== null}
              className="shrink-0 rounded px-2 py-0.5 text-xs bg-green-700 text-white hover:bg-green-800 disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
            >
              {runningId === msg.id ? 'Running...' : 'Run'}
            </button>
          </div>
        ))}
      </div>
    </div>
  )
}

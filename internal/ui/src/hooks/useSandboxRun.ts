import { useCallback, useRef, useState } from 'react'
import { buildWSURL } from '~/lib/api'

export type RunStatus = 'idle' | 'connecting' | 'running' | 'done' | 'error'

export interface RunOutputLine {
  stream: 'stdout' | 'stderr'
  data: string
  timestamp: number
}

interface WsMessage {
  type: string
  stream?: string
  data?: string
  exit_code?: number
  duration?: string
  error?: string
}

export interface SandboxRunSession {
  status: RunStatus
  output: RunOutputLine[]
  exitCode: number | null
  duration: string | null
  error: string | null
  run(language: string, code: string, input: unknown, context: Record<string, unknown>, image?: string, packages?: string, network?: boolean): void
  reset(): void
}

export function useSandboxRun(): SandboxRunSession {
  const [status, setStatus] = useState<RunStatus>('idle')
  const [output, setOutput] = useState<RunOutputLine[]>([])
  const [exitCode, setExitCode] = useState<number | null>(null)
  const [duration, setDuration] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const wsRef = useRef<WebSocket | null>(null)

  const handleMessage = useCallback((event: MessageEvent) => {
    const msg: WsMessage = JSON.parse(event.data)

    switch (msg.type) {
      case 'output':
        setOutput((prev) => [
          ...prev,
          {
            stream: (msg.stream as 'stdout' | 'stderr') ?? 'stdout',
            data: msg.data ?? '',
            timestamp: Date.now(),
          },
        ])
        break

      case 'done':
        setExitCode(msg.exit_code ?? 0)
        setDuration(msg.duration ?? null)
        setStatus('done')
        break

      case 'error':
        setError(msg.error ?? 'Unknown error')
        setStatus('error')
        break
    }
  }, [])

  const run = useCallback(
    (language: string, code: string, input: unknown, context: Record<string, unknown>, image?: string, packages?: string, network?: boolean) => {
      // Reset
      setStatus('connecting')
      setOutput([])
      setExitCode(null)
      setDuration(null)
      setError(null)

      if (wsRef.current) {
        wsRef.current.close()
      }

      void (async () => {
        let wsUrl: string
        try {
          wsUrl = await buildWSURL('/api/v1/sandbox/run')
        } catch {
          setError('failed to mint ws token (auth required)')
          setStatus('error')
          return
        }

        const ws = new WebSocket(wsUrl)
        wsRef.current = ws

        ws.onopen = () => {
          setStatus('running')
          ws.send(
            JSON.stringify({
              type: 'run',
              language,
              code,
              input,
              context,
              ...(image ? { image } : {}),
              ...(packages ? { packages } : {}),
              ...(network ? { network: true } : {}),
            }),
          )
        }

        ws.onmessage = handleMessage

        ws.onerror = () => {
          setError('WebSocket connection error')
          setStatus('error')
        }

        ws.onclose = () => {
          wsRef.current = null
        }
      })()
    },
    [handleMessage],
  )

  const reset = useCallback(() => {
    if (wsRef.current) {
      wsRef.current.close()
      wsRef.current = null
    }
    setStatus('idle')
    setOutput([])
    setExitCode(null)
    setDuration(null)
    setError(null)
  }, [])

  return {
    status,
    output,
    exitCode,
    duration,
    error,
    run,
    reset,
  }
}

import { useCallback, useRef, useState } from 'react'
import { buildWSURL } from '~/lib/api'

export type DebugStatus = 'idle' | 'connecting' | 'running' | 'paused' | 'terminated'

export interface DebugVariable {
  name: string
  value: string
  type?: string
  variablesReference?: number
  objectId?: string
}

export interface DebugStackFrame {
  id?: number
  name: string
  line: number
  column: number
  source?: { path: string; name?: string }
}

export interface DebugOutputLine {
  stream: 'stdout' | 'stderr'
  data: string
  timestamp: number
}

interface WsMessage {
  type: string
  session_id?: string
  reason?: string
  line?: number
  variables?: DebugVariable[]
  callStack?: DebugStackFrame[]
  stream?: string
  data?: string
  error?: string
  objectId?: string
}

export interface DebugSession {
  status: DebugStatus
  sessionId: string | null
  currentLine: number | null
  variables: DebugVariable[]
  callStack: DebugStackFrame[]
  output: DebugOutputLine[]
  result: string | null
  error: string | null

  start(language: string, code: string, input: unknown, context: Record<string, unknown>, breakpoints: { line: number; condition?: string }[], image?: string, packages?: string): void
  setBreakpoints(breakpoints: { line: number; condition?: string }[]): void
  continue_(): void
  stepOver(): void
  stepIn(): void
  stepOut(): void
  evaluate(expression: string): void
  expandedVars: Record<string, DebugVariable[]>
  expandVar(objectId: string): void
  collapseVar(objectId: string): void
  disconnect(): void
}

export function useDebugSession(): DebugSession {
  const [status, setStatus] = useState<DebugStatus>('idle')
  const [sessionId, setSessionId] = useState<string | null>(null)
  const [currentLine, setCurrentLine] = useState<number | null>(null)
  const [variables, setVariables] = useState<DebugVariable[]>([])
  const [callStack, setCallStack] = useState<DebugStackFrame[]>([])
  const [output, setOutput] = useState<DebugOutputLine[]>([])
  const [result, setResult] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [expandedVars, setExpandedVars] = useState<Record<string, DebugVariable[]>>({})
  const wsRef = useRef<WebSocket | null>(null)

  const send = useCallback((msg: Record<string, unknown>) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify(msg))
    }
  }, [])

  const handleMessage = useCallback((event: MessageEvent) => {
    const msg: WsMessage = JSON.parse(event.data)

    switch (msg.type) {
      case 'started':
        setSessionId(msg.session_id ?? null)
        setStatus('connecting')
        break

      case 'connected':
        setStatus('running')
        break

      case 'stopped':
        setStatus('paused')
        setCurrentLine(msg.line || null) // null when paused outside user code (e.g. runner.js debugger;)
        if (msg.variables) setVariables(msg.variables)
        if (msg.callStack) setCallStack(msg.callStack)
        setExpandedVars({}) // clear expansions on new pause
        // Show exception in console + error bar when paused on exception
        if (msg.reason === 'exception' && msg.data) {
          setError(msg.data)
          setOutput(prev => [...prev, {
            stream: 'stderr',
            data: `Exception: ${msg.data}\n`,
            timestamp: Date.now(),
          }])
        }
        break

      case 'variables':
        if (msg.variables) setVariables(msg.variables)
        break

      case 'continued':
        setStatus('running')
        setCurrentLine(null)
        break

      case 'output':
        setOutput(prev => [...prev, {
          stream: (msg.stream as 'stdout' | 'stderr') ?? 'stdout',
          data: msg.data ?? '',
          timestamp: Date.now(),
        }])
        break

      case 'result':
        setResult(msg.data ?? null)
        break

      case 'expand':
        if (msg.objectId && msg.variables) {
          setExpandedVars(prev => ({ ...prev, [msg.objectId!]: msg.variables! }))
        }
        break

      case 'evaluate':
        setOutput(prev => [...prev, {
          stream: 'stdout',
          data: `=> ${msg.data}\n`,
          timestamp: Date.now(),
        }])
        break

      case 'terminated':
        setStatus('terminated')
        break

      case 'error':
        setError(msg.error ?? 'Unknown error')
        break
    }
  }, [])

  const start = useCallback((
    language: string,
    code: string,
    input: unknown,
    context: Record<string, unknown>,
    breakpoints: { line: number; condition?: string }[],
    image?: string,
    packages?: string,
  ) => {
    // Reset state
    setStatus('connecting')
    setSessionId(null)
    setCurrentLine(null)
    setVariables([])
    setCallStack([])
    setOutput([])
    setResult(null)
    setExpandedVars({})
    setError(null)

    // Close existing WS
    if (wsRef.current) {
      wsRef.current.close()
    }

    void (async () => {
      let wsUrl: string
      try {
        wsUrl = await buildWSURL('/api/v1/sandbox/debug')
      } catch {
        setError('failed to mint ws token (auth required)')
        setStatus('terminated')
        return
      }

      const ws = new WebSocket(wsUrl)
      wsRef.current = ws

      ws.onopen = () => {
        ws.send(JSON.stringify({
          type: 'start',
          language,
          code,
          input,
          context,
          breakpoints,
          ...(image ? { image } : {}),
          ...(packages ? { packages } : {}),
        }))
      }

      ws.onmessage = handleMessage

      ws.onerror = () => {
        setError('WebSocket connection error')
        setStatus('terminated')
      }

      ws.onclose = () => {
        if (status !== 'terminated') {
          setStatus('terminated')
        }
      }
    })()
  }, [handleMessage, status])

  const setBreakpoints = useCallback((bps: { line: number; condition?: string }[]) => {
    send({ type: 'set_breakpoints', breakpoints: bps })
  }, [send])

  const continue_ = useCallback(() => {
    send({ type: 'continue' })
    setStatus('running')
    setCurrentLine(null)
  }, [send])

  const stepOver = useCallback(() => {
    send({ type: 'step_over' })
    setStatus('running')
  }, [send])

  const stepIn = useCallback(() => {
    send({ type: 'step_in' })
    setStatus('running')
  }, [send])

  const stepOut = useCallback(() => {
    send({ type: 'step_out' })
    setStatus('running')
  }, [send])

  const evaluate = useCallback((expression: string) => {
    send({ type: 'evaluate', expression })
  }, [send])

  const expandVar = useCallback((objectId: string) => {
    send({ type: 'expand', objectId })
  }, [send])

  const collapseVar = useCallback((objectId: string) => {
    setExpandedVars(prev => {
      const next = { ...prev }
      delete next[objectId]
      return next
    })
  }, [])

  const disconnect = useCallback(() => {
    send({ type: 'disconnect' })
    if (wsRef.current) {
      wsRef.current.close()
      wsRef.current = null
    }
    setStatus('terminated')
  }, [send])

  return {
    status,
    sessionId,
    currentLine,
    variables,
    callStack,
    output,
    result,
    error,
    expandedVars,
    expandVar,
    collapseVar,
    start,
    setBreakpoints,
    continue_,
    stepOver,
    stepIn,
    stepOut,
    evaluate,
    disconnect,
  }
}

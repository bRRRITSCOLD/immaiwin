import { useCallback, useEffect, useRef, useState } from 'react'
import Editor, { type OnMount } from '@monaco-editor/react'
import type * as Monaco from 'monaco-editor'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from '~/components/ui/dialog'
import {
  Bug,
  Play,
  Square,
  SkipForward,
  ArrowDownToLine,
  ArrowUpFromLine,
  StepForward,
  Terminal,
} from 'lucide-react'
import { useDebugSession, type DebugVariable, type DebugStackFrame } from '~/hooks/useDebugSession'
import { useSandboxRun } from '~/hooks/useSandboxRun'

export type DialogMode = 'run' | 'debug'

const monacoLanguageMap: Record<string, string> = {
  javascript: 'javascript',
  python: 'python',
}

interface SandboxDebugDialogProps {
  open: boolean
  onOpenChange(open: boolean): void
  language: string
  code: string
  onCodeChange(code: string): void
  initialMode?: DialogMode
  customImage?: string
  packages?: string
}

export function SandboxDebugDialog({
  open,
  onOpenChange,
  language,
  code,
  onCodeChange,
  initialMode = 'debug',
  customImage,
  packages,
}: SandboxDebugDialogProps) {
  const [mode, setMode] = useState<DialogMode>(initialMode)
  const modeRef = useRef<DialogMode>(mode)
  modeRef.current = mode
  const debug = useDebugSession()
  const runner = useSandboxRun()
  const editorRef = useRef<Monaco.editor.IStandaloneCodeEditor | null>(null)
  const monacoRef = useRef<typeof Monaco | null>(null)
  const [breakpoints, setBreakpoints] = useState<number[]>([])
  const [evalInput, setEvalInput] = useState('')
  const decorationsRef = useRef<string[]>([])
  const [mockInput, setMockInput] = useState('{}')
  const [mockContext, setMockContext] = useState('{}')
  const [mockExpanded, setMockExpanded] = useState(true)

  // Sync mode when dialog reopens with different initialMode
  useEffect(() => {
    if (open) setMode(initialMode)
  }, [open, initialMode])

  // Whether either session is active
  const debugActive = debug.status !== 'idle' && debug.status !== 'terminated'
  const runActive = runner.status === 'connecting' || runner.status === 'running'
  const isActive = mode === 'debug' ? debugActive : runActive

  // Can switch mode only when nothing active
  const canSwitchMode = !debugActive && !runActive

  // Update breakpoint decorations in Monaco (debug mode only)
  const updateDecorations = useCallback(() => {
    const editor = editorRef.current
    if (!editor) return

    if (mode !== 'debug') {
      decorationsRef.current = editor.deltaDecorations(decorationsRef.current, [])
      return
    }

    const newDecorations: Monaco.editor.IModelDeltaDecoration[] = breakpoints.map((line) => ({
      range: { startLineNumber: line, startColumn: 1, endLineNumber: line, endColumn: 1 },
      options: {
        isWholeLine: false,
        glyphMarginClassName: 'debug-breakpoint-glyph',
        glyphMarginHoverMessage: { value: 'Breakpoint' },
      },
    }))

    if (debug.currentLine) {
      newDecorations.push({
        range: {
          startLineNumber: debug.currentLine,
          startColumn: 1,
          endLineNumber: debug.currentLine,
          endColumn: 1,
        },
        options: {
          isWholeLine: true,
          className: 'debug-current-line',
          glyphMarginClassName: debug.currentLine && breakpoints.includes(debug.currentLine)
            ? 'debug-breakpoint-glyph'
            : 'debug-current-line-glyph',
        },
      })
    }

    decorationsRef.current = editor.deltaDecorations(decorationsRef.current, newDecorations)
  }, [breakpoints, debug.currentLine, mode])

  useEffect(() => {
    updateDecorations()
  }, [updateDecorations])

  useEffect(() => {
    if (debug.currentLine && editorRef.current) {
      editorRef.current.revealLineInCenter(debug.currentLine)
    }
  }, [debug.currentLine])

  const handleEditorMount: OnMount = (editor, monaco) => {
    editorRef.current = editor
    monacoRef.current = monaco

    editor.updateOptions({ glyphMargin: true })

    editor.onMouseDown((e) => {
      if (modeRef.current !== 'debug') return
      if (e.target.type === monaco.editor.MouseTargetType.GUTTER_GLYPH_MARGIN) {
        const line = e.target.position?.lineNumber
        if (line) {
          setBreakpoints((prev) => {
            const next = prev.includes(line) ? prev.filter((l) => l !== line) : [...prev, line]
            if (debug.status === 'paused' || debug.status === 'running') {
              debug.setBreakpoints(next.map((l) => ({ line: l })))
            }
            return next
          })
        }
      }
    })

    const styleEl = document.getElementById('debug-styles') || document.createElement('style')
    styleEl.id = 'debug-styles'
    styleEl.textContent = `
      .debug-breakpoint-glyph {
        background: #e51400;
        border-radius: 50%;
        width: 10px !important;
        height: 10px !important;
        margin-left: 4px;
        margin-top: 4px;
      }
      .debug-current-line {
        background: rgba(255, 255, 0, 0.15) !important;
      }
      .debug-current-line-glyph {
        background: #ffcc00;
        clip-path: polygon(0% 20%, 60% 20%, 60% 0%, 100% 50%, 60% 100%, 60% 80%, 0% 80%);
        width: 14px !important;
        height: 14px !important;
        margin-left: 2px;
        margin-top: 2px;
      }
    `
    if (!document.getElementById('debug-styles')) {
      document.head.appendChild(styleEl)
    }
  }

  function parseMockData() {
    let parsedInput: unknown = null
    let parsedContext: Record<string, unknown> = {}
    try { parsedInput = JSON.parse(mockInput) } catch { /* use null */ }
    try { parsedContext = JSON.parse(mockContext) } catch { /* use {} */ }
    return { parsedInput, parsedContext }
  }

  function handleDebugStart() {
    const { parsedInput, parsedContext } = parseMockData()
    debug.start(language, code, parsedInput, parsedContext, breakpoints.map((l) => ({ line: l })), customImage, packages)
  }

  function handleRunStart() {
    const { parsedInput, parsedContext } = parseMockData()
    runner.run(language, code, parsedInput, parsedContext, customImage, packages)
  }

  function handleEval(e: React.FormEvent) {
    e.preventDefault()
    if (evalInput.trim()) {
      debug.evaluate(evalInput.trim())
      setEvalInput('')
    }
  }

  function handleDisconnect() {
    debug.disconnect()
  }

  function handleRunStop() {
    runner.reset()
  }

  const isPaused = debug.status === 'paused'
  const monacoLang = monacoLanguageMap[language] ?? 'javascript'

  // Combine output for current mode
  const currentOutput = mode === 'debug' ? debug.output : runner.output
  const currentError = mode === 'debug' ? debug.error : runner.error

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="!max-w-[95vw] w-[95vw] h-[90vh] flex flex-col !p-0 !gap-0">
        <DialogHeader className="px-4 py-3 border-b border-border flex-none">
          <DialogTitle className="flex items-center gap-2">
            {mode === 'debug' ? (
              <Bug className="h-4 w-4 text-red-400" />
            ) : (
              <Terminal className="h-4 w-4 text-green-400" />
            )}
            {mode === 'debug' ? 'Debug' : 'Run'} — {language}

            {/* Mode toggle */}
            <div className="ml-4 flex rounded-md border border-border overflow-hidden text-xs">
              <button
                onClick={() => canSwitchMode && setMode('run')}
                className={`px-3 py-1 flex items-center gap-1 transition-colors ${
                  mode === 'run'
                    ? 'bg-green-600/20 text-green-400'
                    : 'text-muted-foreground hover:text-foreground'
                } ${!canSwitchMode ? 'opacity-50 cursor-not-allowed' : ''}`}
              >
                <Terminal className="h-3 w-3" /> Run
              </button>
              <button
                onClick={() => canSwitchMode && setMode('debug')}
                className={`px-3 py-1 flex items-center gap-1 border-l border-border transition-colors ${
                  mode === 'debug'
                    ? 'bg-red-600/20 text-red-400'
                    : 'text-muted-foreground hover:text-foreground'
                } ${!canSwitchMode ? 'opacity-50 cursor-not-allowed' : ''}`}
              >
                <Bug className="h-3 w-3" /> Debug
              </button>
            </div>
          </DialogTitle>
        </DialogHeader>

        {/* Toolbar */}
        <div className="flex items-center gap-1.5 px-4 py-2 border-b border-border bg-muted/30 flex-none">
          {mode === 'run' ? (
            /* ——— Run mode toolbar ——— */
            <>
              {!runActive ? (
                <button
                  onClick={handleRunStart}
                  disabled={runner.status === 'connecting'}
                  className="flex items-center gap-1.5 px-3 py-1.5 rounded bg-green-600 hover:bg-green-700 text-white text-xs font-medium transition-colors"
                >
                  <Play className="h-3.5 w-3.5" /> Run
                </button>
              ) : (
                <button
                  onClick={handleRunStop}
                  className="p-1.5 rounded hover:bg-muted transition-colors"
                  title="Stop"
                >
                  <Square className="h-4 w-4 text-red-400" />
                </button>
              )}
              <div className="ml-2 flex items-center gap-1.5">
                <span
                  className={`inline-block h-2 w-2 rounded-full ${
                    runner.status === 'running'
                      ? 'bg-green-400 animate-pulse'
                      : runner.status === 'connecting'
                        ? 'bg-blue-400 animate-pulse'
                        : runner.status === 'done'
                          ? 'bg-green-400'
                          : runner.status === 'error'
                            ? 'bg-red-400'
                            : 'bg-gray-400'
                  }`}
                />
                <span className="text-xs text-muted-foreground">{runner.status}</span>
                {runner.exitCode !== null && (
                  <span className={`text-xs ${runner.exitCode === 0 ? 'text-green-400' : 'text-red-400'}`}>
                    exit {runner.exitCode}
                  </span>
                )}
                {runner.duration && (
                  <span className="text-xs text-muted-foreground">{runner.duration}</span>
                )}
              </div>
              {(runner.status === 'done' || runner.status === 'error') && (
                <button
                  onClick={() => runner.reset()}
                  className="ml-2 px-2 py-1 rounded text-xs text-muted-foreground hover:text-foreground hover:bg-muted transition-colors"
                >
                  Clear
                </button>
              )}
            </>
          ) : (
            /* ——— Debug mode toolbar ——— */
            <>
              {!debugActive ? (
                <button
                  onClick={handleDebugStart}
                  className="flex items-center gap-1.5 px-3 py-1.5 rounded bg-green-600 hover:bg-green-700 text-white text-xs font-medium transition-colors"
                >
                  <Play className="h-3.5 w-3.5" /> Debug
                </button>
              ) : (
                <>
                  <button
                    onClick={() => debug.continue_()}
                    disabled={!isPaused}
                    className="p-1.5 rounded hover:bg-muted disabled:opacity-30 transition-colors"
                    title="Continue (F5)"
                  >
                    <Play className="h-4 w-4 text-green-400" />
                  </button>
                  <button
                    onClick={() => debug.stepOver()}
                    disabled={!isPaused}
                    className="p-1.5 rounded hover:bg-muted disabled:opacity-30 transition-colors"
                    title="Step Over (F10)"
                  >
                    <StepForward className="h-4 w-4 text-blue-400" />
                  </button>
                  <button
                    onClick={() => debug.stepIn()}
                    disabled={!isPaused}
                    className="p-1.5 rounded hover:bg-muted disabled:opacity-30 transition-colors"
                    title="Step In (F11)"
                  >
                    <ArrowDownToLine className="h-4 w-4 text-blue-400" />
                  </button>
                  <button
                    onClick={() => debug.stepOut()}
                    disabled={!isPaused}
                    className="p-1.5 rounded hover:bg-muted disabled:opacity-30 transition-colors"
                    title="Step Out (Shift+F11)"
                  >
                    <ArrowUpFromLine className="h-4 w-4 text-blue-400" />
                  </button>
                  <button
                    onClick={handleDisconnect}
                    className="p-1.5 rounded hover:bg-muted transition-colors"
                    title="Stop"
                  >
                    <Square className="h-4 w-4 text-red-400" />
                  </button>
                  <div className="ml-2 flex items-center gap-1.5">
                    <span
                      className={`inline-block h-2 w-2 rounded-full ${
                        debug.status === 'paused'
                          ? 'bg-yellow-400'
                          : debug.status === 'running'
                            ? 'bg-green-400 animate-pulse'
                            : debug.status === 'connecting'
                              ? 'bg-blue-400 animate-pulse'
                              : 'bg-gray-400'
                      }`}
                    />
                    <span className="text-xs text-muted-foreground">{debug.status}</span>
                    {debug.currentLine && (
                      <span className="text-xs text-muted-foreground">
                        — line {debug.currentLine}
                      </span>
                    )}
                  </div>
                </>
              )}
            </>
          )}

          {currentError && (
            <span className="ml-auto text-xs text-red-400 truncate max-w-[300px]">{currentError}</span>
          )}

          <SkipForward className="h-0 w-0" /> {/* preload icon */}
        </div>

        {/* Mock Data panel — visible when idle */}
        {!isActive && (
          <div className="border-b border-border bg-muted/10">
            <button
              onClick={() => setMockExpanded((v) => !v)}
              className="w-full px-4 py-1.5 text-xs font-medium text-muted-foreground hover:text-foreground flex items-center gap-1 transition-colors"
            >
              <span className={`transition-transform ${mockExpanded ? 'rotate-90' : ''}`}>▶</span>
              Mock Data (input &amp; context)
            </button>
            {mockExpanded && (
              <div className="flex gap-4 px-4 pb-3">
                <div className="flex-1">
                  <label className="block text-xs text-muted-foreground mb-1">input (JSON)</label>
                  <textarea
                    className="w-full h-20 bg-background border border-border rounded px-2 py-1.5 text-xs font-mono resize-y"
                    value={mockInput}
                    onChange={(e) => setMockInput(e.target.value)}
                    placeholder='{"data": [1, 2, 3]}'
                    spellCheck={false}
                  />
                </div>
                <div className="flex-1">
                  <label className="block text-xs text-muted-foreground mb-1">context (JSON)</label>
                  <textarea
                    className="w-full h-20 bg-background border border-border rounded px-2 py-1.5 text-xs font-mono resize-y"
                    value={mockContext}
                    onChange={(e) => setMockContext(e.target.value)}
                    placeholder='{"prevStep": {"output": ...}}'
                    spellCheck={false}
                  />
                </div>
              </div>
            )}
          </div>
        )}

        {/* Main content: editor + side panels */}
        <div className="flex flex-1 min-h-0">
          {/* Left: Monaco editor */}
          <div className="flex-1 min-w-0">
            <Editor
              height="100%"
              language={monacoLang}
              theme="vs-dark"
              value={code}
              onChange={(v) => onCodeChange(v ?? '')}
              onMount={handleEditorMount}
              options={{
                minimap: { enabled: false },
                fontSize: 14,
                wordWrap: 'on',
                scrollBeyondLastLine: false,
                tabSize: 2,
                automaticLayout: true,
                glyphMargin: mode === 'debug',
                readOnly: isActive,
              }}
            />
          </div>

          {/* Right: panels differ by mode */}
          <div className="w-[340px] border-l border-border flex flex-col bg-background">
            {mode === 'debug' ? (
              /* ——— Debug panels: Output + Variables + Call Stack + Console ——— */
              <>
                <div className="h-[120px] flex flex-col border-b border-border">
                  <div className="px-3 py-1.5 text-xs font-medium text-muted-foreground bg-muted/30 border-b border-border">
                    Output
                  </div>
                  <div className="flex-1 overflow-y-auto p-2 font-mono text-xs">
                    {debug.result == null ? (
                      <p className="text-muted-foreground/50 italic">
                        {debug.status === 'terminated' ? 'No output' : 'Waiting for script to complete...'}
                      </p>
                    ) : (
                      <pre className="whitespace-pre-wrap break-all text-foreground">{debug.result}</pre>
                    )}
                  </div>
                </div>

                <div className="flex-1 min-h-0 flex flex-col border-b border-border">
                  <div className="px-3 py-1.5 text-xs font-medium text-muted-foreground bg-muted/30 border-b border-border">
                    Variables
                  </div>
                  <div className="flex-1 overflow-y-auto p-2">
                    {debug.variables.length === 0 ? (
                      <p className="text-xs text-muted-foreground/50 italic">
                        {isPaused ? 'No variables in scope' : 'Pause to inspect variables'}
                      </p>
                    ) : (
                      <VariablesTree
                        variables={debug.variables}
                        expandedVars={debug.expandedVars}
                        onExpand={debug.expandVar}
                        onCollapse={debug.collapseVar}
                      />
                    )}
                  </div>
                </div>

                <div className="h-[140px] flex flex-col border-b border-border">
                  <div className="px-3 py-1.5 text-xs font-medium text-muted-foreground bg-muted/30 border-b border-border">
                    Call Stack
                  </div>
                  <div className="flex-1 overflow-y-auto p-2">
                    {debug.callStack.length === 0 ? (
                      <p className="text-xs text-muted-foreground/50 italic">No call stack</p>
                    ) : (
                      debug.callStack.map((frame, i) => (
                        <div
                          key={i}
                          className={`text-xs px-2 py-1 rounded cursor-pointer hover:bg-muted/50 ${i === 0 ? 'bg-muted/30' : ''}`}
                          onClick={() => {
                            if (frame.line && editorRef.current) {
                              editorRef.current.revealLineInCenter(frame.line)
                            }
                          }}
                        >
                          <span className="text-foreground">{frame.name}</span>
                          <span className="text-muted-foreground ml-1">:{frame.line}</span>
                        </div>
                      ))
                    )}
                  </div>
                </div>

                <div className="flex-1 min-h-0 flex flex-col">
                  <div className="px-3 py-1.5 text-xs font-medium text-muted-foreground bg-muted/30 border-b border-border">
                    Console
                  </div>
                  <OutputPanel output={debug.output} />
                  <form onSubmit={handleEval} className="flex border-t border-border">
                    <span className="px-2 py-1.5 text-xs text-muted-foreground">&gt;</span>
                    <input
                      type="text"
                      className="flex-1 bg-transparent text-xs outline-none py-1.5 pr-2 font-mono"
                      placeholder="Evaluate expression..."
                      value={evalInput}
                      onChange={(e) => setEvalInput(e.target.value)}
                      disabled={!isPaused}
                    />
                  </form>
                </div>
              </>
            ) : (
              /* ——— Run mode: output-only panel ——— */
              <div className="flex-1 min-h-0 flex flex-col">
                <div className="px-3 py-1.5 text-xs font-medium text-muted-foreground bg-muted/30 border-b border-border">
                  Output
                </div>
                <OutputPanel output={currentOutput} />
              </div>
            )}
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}

function VariablesTree({
  variables,
  expandedVars,
  onExpand,
  onCollapse,
  depth = 0,
}: {
  variables: DebugVariable[]
  expandedVars: Record<string, DebugVariable[]>
  onExpand: (objectId: string) => void
  onCollapse: (objectId: string) => void
  depth?: number
}) {
  return (
    <div className="space-y-0.5">
      {variables.map((v, i) => {
        const expandable = !!v.objectId
        const isExpanded = v.objectId ? v.objectId in expandedVars : false
        const children = v.objectId ? expandedVars[v.objectId] : undefined

        return (
          <div key={`${v.name}-${i}`}>
            <div
              className={`flex items-baseline gap-2 text-xs font-mono px-1 py-0.5 rounded hover:bg-muted/30 ${expandable ? 'cursor-pointer' : ''}`}
              style={{ paddingLeft: `${depth * 12 + 4}px` }}
              onClick={() => {
                if (!expandable || !v.objectId) return
                if (isExpanded) {
                  onCollapse(v.objectId)
                } else {
                  onExpand(v.objectId)
                }
              }}
            >
              {expandable && (
                <span className={`text-muted-foreground text-[10px] select-none transition-transform ${isExpanded ? 'rotate-90' : ''}`}>
                  ▶
                </span>
              )}
              <span className="text-blue-400 shrink-0">{v.name}</span>
              <span className="text-muted-foreground">=</span>
              <span className="text-foreground truncate">{v.value}</span>
              {v.type && <span className="text-muted-foreground/50 text-[10px] ml-auto shrink-0">{v.type}</span>}
            </div>
            {isExpanded && children && (
              <VariablesTree
                variables={children}
                expandedVars={expandedVars}
                onExpand={onExpand}
                onCollapse={onCollapse}
                depth={depth + 1}
              />
            )}
          </div>
        )
      })}
    </div>
  )
}

function OutputPanel({ output }: { output: { stream: string; data: string; timestamp: number }[] }) {
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (ref.current) {
      ref.current.scrollTop = ref.current.scrollHeight
    }
  }, [output])

  return (
    <div ref={ref} className="flex-1 overflow-y-auto p-2 font-mono text-xs">
      {output.length === 0 ? (
        <p className="text-muted-foreground/50 italic">No output yet</p>
      ) : (
        output.map((line, i) => (
          <div
            key={i}
            className={`whitespace-pre-wrap break-all ${
              line.stream === 'stderr' ? 'text-red-400' : 'text-foreground'
            }`}
          >
            {line.data}
          </div>
        ))
      )}
    </div>
  )
}

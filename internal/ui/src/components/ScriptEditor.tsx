import Editor, { type BeforeMount } from '@monaco-editor/react'
import type * as Monaco from 'monaco-editor'

let completionsRegistered = false

const GOJA_COMPLETIONS: Omit<Monaco.languages.CompletionItem, 'range'>[] = [
  {
    label: 'parse',
    kind: 1 /* Function */,
    insertText: [
      'function parse(raw) {',
      '\t// raw: string (HTML or XML from the feed)',
      '\t// return an array of article objects',
      '\treturn []',
      '}',
    ].join('\n'),
    insertTextRules: 4 /* InsertAsSnippet */,
    documentation: 'Entry point. Must be defined at top level. Receives raw feed string, returns article array.',
    detail: 'function parse(raw: string): Article[]',
  },
  {
    label: '$',
    kind: 1,
    insertText: '$(${1:html})',
    insertTextRules: 4,
    documentation: 'jQuery-like HTML selection. Returns a Selection with .find(), .first(), .eq(), .text(), .attr(), .html(), .length, .each().',
    detail: '$(html: string): Selection',
  },
  {
    label: 'parseRSS',
    kind: 1,
    insertText: 'parseRSS(${1:xmlStr})',
    insertTextRules: 4,
    documentation: 'Parses RSS/Atom XML string. Returns array of item objects with fields: title, link, guid, description, pubDate, author, category.',
    detail: 'parseRSS(xmlStr: string): RSSItem[]',
  },
  {
    label: 'now',
    kind: 1,
    insertText: 'now()',
    insertTextRules: 4,
    documentation: 'Returns current UTC timestamp as ISO-8601 string.',
    detail: 'now(): string',
  },
  {
    label: 'parseDate',
    kind: 1,
    insertText: 'parseDate(${1:str})',
    insertTextRules: 4,
    documentation: 'Parses a date string (RFC1123, RFC1123Z, "2006-01-02 15:04:05", or ISO-8601). Returns ISO-8601 UTC string, or "" on failure.',
    detail: 'parseDate(str: string): string',
  },
]

const handleBeforeMount: BeforeMount = (monaco) => {
  if (completionsRegistered) return
  completionsRegistered = true

  monaco.languages.registerCompletionItemProvider('javascript', {
    provideCompletionItems(model: Monaco.editor.ITextModel, position: Monaco.Position) {
      const word = model.getWordUntilPosition(position)
      const range = {
        startLineNumber: position.lineNumber,
        endLineNumber: position.lineNumber,
        startColumn: word.startColumn,
        endColumn: word.endColumn,
      }
      return {
        suggestions: GOJA_COMPLETIONS.map((c) => ({ ...c, range })),
      }
    },
  })
}

interface Props {
  value: string
  onChange: (v: string) => void
  height?: number | string
}

export function ScriptEditor({ value, onChange, height = 360 }: Props) {
  return (
    <Editor
      height={height}
      language="javascript"
      theme="vs-dark"
      value={value}
      onChange={(v) => onChange(v ?? '')}
      beforeMount={handleBeforeMount}
      options={{
        minimap: { enabled: false },
        fontSize: 13,
        wordWrap: 'on',
        scrollBeyondLastLine: false,
        tabSize: 2,
        automaticLayout: true,
      }}
    />
  )
}

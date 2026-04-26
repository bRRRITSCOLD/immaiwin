import Editor, { type BeforeMount } from '@monaco-editor/react'
import type * as Monaco from 'monaco-editor'

// Type declarations injected into Monaco so the user gets full IntelliSense
// for all goja runtime bindings.
const GOJA_LIB = `
declare interface Selection {
  find(selector: string): Selection;
  first(): Selection;
  eq(index: number): Selection;
  text(): string;
  attr(name: string): string | undefined;
  html(): string;
  length: number;
  each(fn: (index: number, el: Selection) => void): void;
}

declare interface RSSItem {
  title: string;
  link: string;
  guid: string;
  description: string;
  pubDate: string;
  author: string;
  categories: string[];
}

declare interface Article {
  url: string;
  title: string;
  body?: string;
  raw_html?: string;
  raw_xml?: string;
  scraped_at?: string;
  metadata?: Record<string, unknown>;
}

/**
 * jQuery-like HTML selection. Wraps the raw HTML string in a document and
 * returns a Selection with .find(), .text(), .attr(), .each(), etc.
 */
declare function $(html: string): Selection;

/**
 * Parses an RSS/Atom XML string. Returns an array of item objects.
 * Fields: title, link, guid, description, pubDate, author, categories[]
 */
declare function parseRSS(xmlStr: string): RSSItem[];

/** Returns the current UTC timestamp as an ISO-8601 string. */
declare function now(): string;

/**
 * Parses a date string (RFC1123, RFC1123Z, "2006-01-02 15:04:05", or ISO-8601).
 * Returns an ISO-8601 UTC string, or "" on failure.
 */
declare function parseDate(str: string): string;

declare interface HttpResponse {
  ok: boolean;
  status: number;
  body: string;
}

/**
 * Performs a synchronous HTTP GET request.
 * Returns {ok, status, body}. Never throws — check ok/status for errors.
 */
declare function httpGet(url: string): HttpResponse;
`

// Snippet completions shown in addition to the IntelliSense from GOJA_LIB.
const SNIPPETS: Omit<Monaco.languages.CompletionItem, 'range'>[] = [
  {
    label: 'parse (function)',
    kind: 1 /* Function */,
    insertText: [
      'function parse(raw: string): Article[] {',
      '\t// raw: RSS/XML or HTML string from the feed',
      '\treturn []',
      '}',
    ].join('\n'),
    insertTextRules: 4 /* InsertAsSnippet */,
    documentation: 'Entry point. Must be defined at top level.',
    detail: 'function parse(raw: string): Article[]',
  },
]

let editorConfigured = false

const handleBeforeMount: BeforeMount = (monaco) => {
  if (editorConfigured) return
  editorConfigured = true

  // Inject goja global type declarations for both TS and JS editors.
  monaco.languages.typescript.typescriptDefaults.addExtraLib(GOJA_LIB, 'goja-globals.d.ts')
  monaco.languages.typescript.javascriptDefaults.addExtraLib(GOJA_LIB, 'goja-globals.d.ts')

  // Relax TS strict mode — users write scripts, not production apps.
  monaco.languages.typescript.typescriptDefaults.setCompilerOptions({
    target: monaco.languages.typescript.ScriptTarget.ES2015,
    allowNonTsExtensions: true,
    strict: false,
    noImplicitAny: false,
  })

  // Snippet completions for both languages.
  const registerSnippets = (langId: string) => {
    monaco.languages.registerCompletionItemProvider(langId, {
      provideCompletionItems(model: Monaco.editor.ITextModel, position: Monaco.Position) {
        const word = model.getWordUntilPosition(position)
        const range = {
          startLineNumber: position.lineNumber,
          endLineNumber: position.lineNumber,
          startColumn: word.startColumn,
          endColumn: word.endColumn,
        }
        return { suggestions: SNIPPETS.map((s) => ({ ...s, range })) }
      },
    })
  }
  registerSnippets('typescript')
  registerSnippets('javascript')
}

interface Props {
  value: string
  onChange: (v: string) => void
  height?: number | string
  language?: 'typescript' | 'javascript'
}

export function ScriptEditor({ value, onChange, height = 360, language = 'typescript' }: Props) {
  return (
    <Editor
      height={height}
      language={language}
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

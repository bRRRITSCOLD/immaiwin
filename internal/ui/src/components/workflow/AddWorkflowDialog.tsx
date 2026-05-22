import { useState, useCallback, useEffect } from 'react'
import { useForm } from '@tanstack/react-form'
import { useDropzone } from 'react-dropzone'
import { z } from 'zod'
import { toast } from 'sonner'
import { Button } from '~/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from '~/components/ui/dialog'
import { Input } from '~/components/ui/input'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '~/components/ui/field'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '~/components/ui/tabs'
import { Plus, Upload, FileUp, Copy } from 'lucide-react'
import type { Workflow } from './useWorkflowStore'
import { api, ApiError } from '~/lib/api'

const nameSchema = z.string().min(1, 'Required').max(100, 'Too long')

interface WorkflowBundle {
  version: number
  workflow: Workflow
}

interface Props {
  onCreated: () => void
}

export function AddWorkflowDialog({ onCreated }: Props) {
  const [open, setOpen] = useState(false)
  const [tab, setTab] = useState<string>('create')

  // ── Create form ────────────────────────────────────────────────────────────
  const createForm = useForm({
    defaultValues: { name: '' },
    onSubmit: async ({ value }) => {
      const id = crypto.randomUUID()
      try {
        await api.put(`/api/v1/workflows/${id}`, {
          name: value.name,
          config: {},
          nodes: [
            {
              id: 'trigger-1',
              type: 'trigger',
              position: { x: 250, y: 50 },
              data: { trigger_type: 'manual' },
            },
          ],
          edges: [],
        })
        toast.success(`Workflow "${value.name}" created`)
        handleClose()
        onCreated()
      } catch (err) {
        toast.error(err instanceof ApiError ? err.message : 'Network error')
      }
    },
  })

  // ── Template state ─────────────────────────────────────────────────────────
  interface TemplateMeta {
    slug: string
    name: string
    description?: string
    category?: string
    workflow: Workflow
  }
  const [templates, setTemplates] = useState<TemplateMeta[]>([])
  const [tmplLoading, setTmplLoading] = useState(false)
  const [tmplForking, setTmplForking] = useState(false)

  useEffect(() => {
    if (tab !== 'template' || templates.length > 0) return
    setTmplLoading(true)
    api.get<{ templates: TemplateMeta[] }>('/api/v1/workflow_templates')
      .then((d) => setTemplates(d.templates ?? []))
      .catch(() => toast.error('Failed to load templates'))
      .finally(() => setTmplLoading(false))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tab])

  async function forkTemplate(tmpl: TemplateMeta) {
    setTmplForking(true)
    try {
      // Server-side fork — bypasses the UpsertWorkflow connection
      // validator because templates ship with empty connection_id
      // placeholders by design. The user wires the real connection
      // on the canvas; the NEXT save runs through the normal guard.
      await api.post(`/api/v1/workflow_templates/${tmpl.slug}/fork`, {})
      toast.success(`Forked "${tmpl.name}"`)
      handleClose()
      onCreated()
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : 'Network error')
    } finally {
      setTmplForking(false)
    }
  }

  // ── Import state ───────────────────────────────────────────────────────────
  const [bundle, setBundle] = useState<WorkflowBundle | null>(null)
  const [fileName, setFileName] = useState('')
  const [importName, setImportName] = useState('')
  const [importError, setImportError] = useState('')
  const [importing, setImporting] = useState(false)

  const processFile = useCallback((file: File) => {
    setImportError('')
    setBundle(null)
    setFileName(file.name)
    const reader = new FileReader()
    reader.onload = () => {
      try {
        const data = JSON.parse(reader.result as string) as WorkflowBundle
        if (!data.version || !data.workflow) {
          setImportError('Invalid bundle: missing version or workflow')
          return
        }
        setBundle(data)
        setImportName(data.workflow.name)
      } catch {
        setImportError('Invalid JSON file')
      }
    }
    reader.readAsText(file)
  }, [])

  const { getRootProps, getInputProps, isDragActive } = useDropzone({
    onDrop: (accepted) => {
      if (accepted[0]) processFile(accepted[0])
    },
    accept: { 'application/json': ['.json'] },
    multiple: false,
  })

  async function handleImport() {
    if (!bundle) return
    setImporting(true)
    try {
      const wfId = crypto.randomUUID()
      const wf = bundle.workflow
      await api.put(`/api/v1/workflows/${wfId}`, {
        name: importName || wf.name,
        config: wf.config ?? {},
        nodes: wf.nodes ?? [],
        edges: wf.edges ?? [],
      })

      toast.success(`Imported "${importName}"`)
      handleClose()
      onCreated()
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : 'Network error')
    } finally {
      setImporting(false)
    }
  }

  function handleClose() {
    createForm.reset()
    setBundle(null)
    setFileName('')
    setImportName('')
    setImportError('')
    setTab('create')
    setOpen(false)
  }

  return (
    <Dialog open={open} onOpenChange={(o) => { if (!o) handleClose(); else setOpen(true) }}>
      <DialogTrigger asChild>
        <button className="flex items-center gap-1.5 w-full px-2 py-1.5 rounded-md border border-dashed border-border text-xs text-muted-foreground hover:bg-accent transition-colors">
          <Plus className="h-3 w-3" />
          Add Workflow
        </button>
      </DialogTrigger>

      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>New Workflow</DialogTitle>
        </DialogHeader>

        <Tabs value={tab} onValueChange={setTab}>
          <TabsList className="w-full">
            <TabsTrigger value="create" className="flex-1">Blank</TabsTrigger>
            <TabsTrigger value="template" className="flex-1">From Template</TabsTrigger>
            <TabsTrigger value="import" className="flex-1">Import</TabsTrigger>
          </TabsList>

          {/* ── Create tab ────────────────────────────────────────────── */}
          <TabsContent value="create">
            <form
              onSubmit={(e) => {
                e.preventDefault()
                createForm.handleSubmit()
              }}
            >
              <FieldGroup className="py-2">
                <createForm.Field
                  name="name"
                  validators={{
                    onBlur: ({ value }) => {
                      const r = nameSchema.safeParse(value)
                      return r.success ? undefined : r.error.issues[0]?.message
                    },
                  }}
                >
                  {(field) => {
                    const invalid = field.state.meta.isTouched && field.state.meta.errors.length > 0
                    return (
                      <Field>
                        <FieldLabel htmlFor={field.name}>Name</FieldLabel>
                        <Input
                          id={field.name}
                          name={field.name}
                          value={field.state.value}
                          onBlur={field.handleBlur}
                          onChange={(e) => field.handleChange(e.target.value)}
                          aria-invalid={invalid}
                          placeholder="e.g. Bloomberg News Scraper"
                        />
                        <FieldDescription>
                          Display name for this workflow.
                        </FieldDescription>
                        {invalid && <FieldError errors={field.state.meta.errors as string[]} />}
                      </Field>
                    )
                  }}
                </createForm.Field>
              </FieldGroup>

              <DialogFooter className="pt-4">
                <Button type="button" variant="ghost" onClick={handleClose}>
                  Cancel
                </Button>
                <createForm.Subscribe selector={(s) => s.isSubmitting}>
                  {(submitting) => (
                    <Button type="submit" disabled={submitting}>
                      {submitting ? 'Creating…' : 'Create'}
                    </Button>
                  )}
                </createForm.Subscribe>
              </DialogFooter>
            </form>
          </TabsContent>

          {/* ── Template tab ──────────────────────────────────────────── */}
          <TabsContent value="template">
            <div className="py-2 max-h-[400px] overflow-y-auto space-y-2">
              {tmplLoading && (
                <p className="text-xs text-muted-foreground italic py-4 text-center">Loading templates…</p>
              )}
              {!tmplLoading && templates.length === 0 && (
                <p className="text-xs text-muted-foreground italic py-4 text-center">No templates available</p>
              )}
              {templates.map((t) => {
                const nodeCount = t.workflow.nodes?.length ?? 0
                const edgeCount = t.workflow.edges?.length ?? 0
                return (
                  <button
                    key={t.slug}
                    type="button"
                    disabled={tmplForking}
                    onClick={() => forkTemplate(t)}
                    className="w-full text-left rounded border border-border hover:border-primary hover:bg-accent/40 transition-colors p-3 disabled:opacity-50"
                  >
                    <div className="flex items-center justify-between gap-2">
                      <div className="flex items-center gap-2 min-w-0">
                        <span className="font-medium text-sm truncate">{t.name}</span>
                        {t.category && (
                          <span className="text-[9px] uppercase tracking-wider text-muted-foreground bg-muted px-1.5 py-0.5 rounded">
                            {t.category}
                          </span>
                        )}
                      </div>
                      <Copy className="h-3.5 w-3.5 text-muted-foreground shrink-0" />
                    </div>
                    {t.description && (
                      <p className="text-xs text-muted-foreground mt-1 leading-snug">
                        {t.description}
                      </p>
                    )}
                    <p className="text-[10px] text-muted-foreground/70 mt-1">
                      {nodeCount} node{nodeCount === 1 ? '' : 's'} · {edgeCount} edge{edgeCount === 1 ? '' : 's'}
                    </p>
                  </button>
                )
              })}
            </div>
            <DialogFooter className="pt-2">
              <Button type="button" variant="ghost" onClick={handleClose}>
                Cancel
              </Button>
            </DialogFooter>
          </TabsContent>

          {/* ── Import tab ────────────────────────────────────────────── */}
          <TabsContent value="import">
            <FieldGroup className="py-2">
              <Field>
                <FieldLabel>Bundle File</FieldLabel>
                <div
                  {...getRootProps()}
                  className={`flex flex-col items-center justify-center gap-2 rounded-md border-2 border-dashed px-4 py-6 text-center cursor-pointer transition-colors ${
                    isDragActive
                      ? 'border-primary bg-primary/5'
                      : 'border-border hover:border-muted-foreground/50 hover:bg-accent/50'
                  }`}
                >
                  <input {...getInputProps()} />
                  <FileUp className={`h-8 w-8 ${isDragActive ? 'text-primary' : 'text-muted-foreground'}`} />
                  {fileName ? (
                    <p className="text-xs text-foreground font-medium">{fileName}</p>
                  ) : isDragActive ? (
                    <p className="text-xs text-primary font-medium">Drop file here</p>
                  ) : (
                    <p className="text-xs text-muted-foreground">
                      Drag & drop a <code className="bg-muted px-1 rounded text-[10px]">.workflow.json</code> file, or click to browse
                    </p>
                  )}
                </div>
                {importError && <FieldError errors={[importError]} />}
              </Field>

              {bundle && (
                <>
                  <Field>
                    <FieldLabel>Workflow Name</FieldLabel>
                    <Input
                      value={importName}
                      onChange={(e) => setImportName(e.target.value)}
                      placeholder={bundle.workflow.name}
                    />
                    <FieldDescription>
                      Override name for target environment.
                    </FieldDescription>
                  </Field>

                  <div className="text-xs text-muted-foreground space-y-1 rounded-md border p-3">
                    <p><span className="font-medium text-foreground">{bundle.workflow.nodes?.length ?? 0}</span> nodes, <span className="font-medium text-foreground">{bundle.workflow.edges?.length ?? 0}</span> edges</p>
                    {Object.keys(bundle.workflow.config ?? {}).length > 0 && (
                      <p><span className="font-medium text-foreground">{Object.keys(bundle.workflow.config).length}</span> config keys: {Object.keys(bundle.workflow.config).join(', ')}</p>
                    )}
                    <p className="text-[10px]">Connections not included — set up manually in target environment.</p>
                  </div>
                </>
              )}
            </FieldGroup>

            <DialogFooter className="pt-4">
              <Button type="button" variant="ghost" onClick={handleClose}>
                Cancel
              </Button>
              <Button onClick={handleImport} disabled={!bundle || importing}>
                <Upload className="h-3.5 w-3.5 mr-1.5" />
                {importing ? 'Importing…' : 'Import'}
              </Button>
            </DialogFooter>
          </TabsContent>
        </Tabs>
      </DialogContent>
    </Dialog>
  )
}

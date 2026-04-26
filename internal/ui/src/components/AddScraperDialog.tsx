import { useState } from 'react'
import { useForm } from '@tanstack/react-form'
import { z } from 'zod'
import { toast } from 'sonner'
import { Button } from '~/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from '~/components/ui/dialog'
import { Input } from '~/components/ui/input'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '~/components/ui/field'
import { ScriptEditor } from '~/components/ScriptEditor'

// ── templates ────────────────────────────────────────────────────────────────

const RSS_TEMPLATE = `// parse(raw) receives raw RSS/XML string.
// Helpers: parseRSS(xmlStr), now(), parseDate(str), $(html)
function parse(raw) {
  var items = parseRSS(raw)
  return items.map(function(item) {
    return {
      url: item.link || item.guid || '',
      title: item.title || '',
      body: item.description || '',
      scraped_at: item.pubDate ? parseDate(item.pubDate) : now()
    }
  })
}`

const HTML_TEMPLATE = `// parse(raw) receives raw HTML string.
// Helpers: $(html), now(), parseRSS(xmlStr), parseDate(str)
function parse(raw) {
  var articles = []
  $(raw).find('article').each(function(i, el) {
    var title = el.find('h2, h3').first().text().trim()
    if (!title) return
    var a = el.find('a').first()
    var link = a.length ? (a.attr('href') || '') : ''
    if (!link) return
    if (link.indexOf('/') === 0) link = 'https://example.com' + link
    if (link.indexOf('http') !== 0) return
    articles.push({ url: link, title: title, scraped_at: now() })
  })
  return articles
}`

type FeedType = 'rss' | 'html'
const TEMPLATES: Record<FeedType, string> = { rss: RSS_TEMPLATE, html: HTML_TEMPLATE }

// ── validators ───────────────────────────────────────────────────────────────

const sourceSchema = z
  .string()
  .min(1, 'Required')
  .regex(/^[a-z0-9-]+$/, 'Lowercase letters, numbers, hyphens only')

const feedURLSchema = z.string().min(1, 'Required').url('Must be a valid URL')

const scriptSchema = z.string().min(1, 'Script required')

// ── component ─────────────────────────────────────────────────────────────────

interface Props {
  apiBase: string
  onCreated: () => void
}

export function AddScraperDialog({ apiBase, onCreated }: Props) {
  const [open, setOpen] = useState(false)
  const [feedType, setFeedType] = useState<FeedType>('rss')
  const [validating, setValidating] = useState(false)

  const form = useForm({
    defaultValues: {
      source: '',
      feed_url: '',
      script: RSS_TEMPLATE,
    },
    onSubmit: async ({ value }) => {
      try {
        const res = await fetch(`${apiBase}/api/v1/news/scrapers/${encodeURIComponent(value.source)}`, {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ feed_url: value.feed_url, script: value.script }),
        })
        const data = await res.json()
        if (!res.ok) {
          toast.error(data.error ?? 'Failed to create scraper')
          return
        }
        toast.success(`Scraper "${value.source}" created`)
        handleClose()
        onCreated()
      } catch {
        toast.error('Network error')
      }
    },
  })

  function handleClose() {
    form.reset()
    setFeedType('rss')
    setOpen(false)
  }

  function handleFeedTypeChange(newType: FeedType) {
    const currentScript = form.state.values.script
    // swap template only if script is still the current template (unmodified)
    const isDefault = currentScript === TEMPLATES[feedType]
    setFeedType(newType)
    if (isDefault) {
      void form.setFieldValue('script', TEMPLATES[newType])
    }
  }

  async function validate() {
    const script = form.state.values.script
    setValidating(true)
    try {
      const res = await fetch(`${apiBase}/api/v1/news/scrapers/validate`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ script }),
      })
      const data = await res.json()
      if (!res.ok) toast.error(data.error ?? 'Invalid script')
      else toast.success('Script valid')
    } catch {
      toast.error('Network error')
    } finally {
      setValidating(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={(o) => { if (!o) handleClose(); else setOpen(true) }}>
      <DialogTrigger asChild>
        <Button variant="outline" size="sm">Add Scraper</Button>
      </DialogTrigger>

      <DialogContent className="sm:max-w-3xl max-h-[90vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>Add New Scraper</DialogTitle>
          <DialogDescription>
            Define a new news source with a custom parser script.
          </DialogDescription>
        </DialogHeader>

        <form
          onSubmit={(e) => {
            e.preventDefault()
            form.handleSubmit()
          }}
        >
          <FieldGroup className="py-2">
            {/* Source ID */}
            <form.Field
              name="source"
              validators={{
                onBlur: ({ value }) => {
                  const r = sourceSchema.safeParse(value)
                  return r.success ? undefined : r.error.issues[0]?.message
                },
              }}
            >
              {(field) => {
                const invalid = field.state.meta.isTouched && field.state.meta.errors.length > 0
                return (
                  <Field>
                    <FieldLabel htmlFor={field.name}>Source ID</FieldLabel>
                    <Input
                      id={field.name}
                      name={field.name}
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(e) => field.handleChange(e.target.value)}
                      aria-invalid={invalid}
                      placeholder="e.g. reuters"
                    />
                    <FieldDescription>
                      Unique slug — lowercase letters, numbers, hyphens.
                    </FieldDescription>
                    {invalid && <FieldError errors={field.state.meta.errors as string[]} />}
                  </Field>
                )
              }}
            </form.Field>

            {/* Feed URL */}
            <form.Field
              name="feed_url"
              validators={{
                onBlur: ({ value }) => {
                  const r = feedURLSchema.safeParse(value)
                  return r.success ? undefined : r.error.issues[0]?.message
                },
              }}
            >
              {(field) => {
                const invalid = field.state.meta.isTouched && field.state.meta.errors.length > 0
                return (
                  <Field>
                    <FieldLabel htmlFor={field.name}>Feed URL</FieldLabel>
                    <Input
                      id={field.name}
                      name={field.name}
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(e) => field.handleChange(e.target.value)}
                      aria-invalid={invalid}
                      placeholder="https://example.com/rss.xml"
                    />
                    <FieldDescription>
                      RSS/Atom feed or HTML page URL to scrape.
                    </FieldDescription>
                    {invalid && <FieldError errors={field.state.meta.errors as string[]} />}
                  </Field>
                )
              }}
            </form.Field>

            {/* Feed type toggle → loads template */}
            <Field>
              <FieldLabel>Feed Type</FieldLabel>
              <div className="flex gap-2">
                <Button
                  type="button"
                  size="sm"
                  variant={feedType === 'rss' ? 'default' : 'outline'}
                  onClick={() => handleFeedTypeChange('rss')}
                >
                  RSS / XML
                </Button>
                <Button
                  type="button"
                  size="sm"
                  variant={feedType === 'html' ? 'default' : 'outline'}
                  onClick={() => handleFeedTypeChange('html')}
                >
                  HTML
                </Button>
              </div>
              <FieldDescription>
                Selects the starter template. Swap only resets script if you haven't edited it yet.
              </FieldDescription>
            </Field>

            {/* Script editor */}
            <form.Field
              name="script"
              validators={{
                onBlur: ({ value }) => {
                  const r = scriptSchema.safeParse(value)
                  return r.success ? undefined : r.error.issues[0]?.message
                },
              }}
            >
              {(field) => {
                const invalid = field.state.meta.isTouched && field.state.meta.errors.length > 0
                return (
                  <Field>
                    <div className="flex items-center justify-between">
                      <FieldLabel>
                        Parser Script
                        <span className="ml-1.5 text-muted-foreground/60 font-normal text-xs">
                          — define <code className="bg-muted px-1 rounded">function parse(raw)</code>
                        </span>
                      </FieldLabel>
                      <Button
                        type="button"
                        size="sm"
                        variant="outline"
                        onClick={validate}
                        disabled={validating}
                        className="h-6 px-2 text-xs"
                      >
                        {validating ? 'Validating…' : 'Validate'}
                      </Button>
                    </div>
                    <div className="rounded-md overflow-hidden border">
                      <ScriptEditor
                        value={field.state.value}
                        onChange={(v) => field.handleChange(v)}
                        height={280}
                      />
                    </div>
                    {invalid && <FieldError errors={field.state.meta.errors as string[]} />}
                  </Field>
                )
              }}
            </form.Field>
          </FieldGroup>

          <DialogFooter className="pt-4">
            <Button type="button" variant="ghost" onClick={handleClose}>
              Cancel
            </Button>
            <form.Subscribe selector={(s) => s.isSubmitting}>
              {(submitting) => (
                <Button type="submit" disabled={submitting}>
                  {submitting ? 'Adding…' : 'Add Scraper'}
                </Button>
              )}
            </form.Subscribe>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

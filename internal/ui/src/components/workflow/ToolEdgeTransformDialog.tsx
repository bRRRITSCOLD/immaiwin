import { useEffect, useRef, useState } from 'react'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '~/components/ui/dialog'
import { Button } from '~/components/ui/button'
import { Textarea } from '~/components/ui/textarea'
import { handleJsonTextareaTab } from './nodes/jsonTextareaTab'

interface Props {
  open: boolean
  onOpenChange: (open: boolean) => void
  initialTransform: unknown
  onSave: (transform: unknown | null) => void
}

/**
 * ToolEdgeTransformDialog edits the `output_transform` field on an
 * agent tool edge. The transform is a JSON template — same engine as
 * Return / Transform nodes — applied to the tool's output before the
 * agent receives it as tool_result. Raw output still lands on the
 * node's StepResult so debug surfaces aren't lossy.
 *
 * Empty / cleared text clears the transform entirely (edge data field
 * removed via null sentinel up to the parent).
 */
export function ToolEdgeTransformDialog({ open, onOpenChange, initialTransform, onSave }: Props) {
  const initialText =
    initialTransform === undefined || initialTransform === null
      ? ''
      : typeof initialTransform === 'string'
        ? initialTransform
        : JSON.stringify(initialTransform, null, 2)

  const [text, setText] = useState(initialText)
  const lastInitialRef = useRef(initialText)
  const [parseError, setParseError] = useState<string | null>(null)

  // Reset local state when the dialog opens against a different edge
  // / different initial value.
  useEffect(() => {
    if (initialText !== lastInitialRef.current) {
      setText(initialText)
      lastInitialRef.current = initialText
      setParseError(null)
    }
  }, [initialText])

  const handleSave = () => {
    const trimmed = text.trim()
    if (trimmed === '') {
      onSave(null)
      onOpenChange(false)
      return
    }
    try {
      const parsed = JSON.parse(trimmed)
      setParseError(null)
      onSave(parsed)
      onOpenChange(false)
    } catch (err) {
      setParseError((err as Error).message)
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>Tool output transform</DialogTitle>
          <DialogDescription>
            Reshape the tool's output before the agent receives it as tool_result. JSON, template-resolved at runtime
            against the tool's raw output as <code>{`{{input.<field>}}`}</code>, plus the workflow's <code>{`{{context.X}}`}</code> /{' '}
            <code>{`{{config.X}}`}</code> / <code>{`{{run_input.X}}`}</code>. Empty = raw output passes through (no transform).
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-2">
          <Textarea
            className="font-mono text-xs min-h-[200px] resize-y"
            rows={12}
            placeholder='{ "id": "{{input.id}}", "name": "{{input.name}}" }'
            value={text}
            onKeyDown={(e) => handleJsonTextareaTab(e, text, setText)}
            onChange={(e) => {
              setText(e.target.value)
              setParseError(null)
            }}
          />
          {parseError && <p className="text-xs text-red-500">{parseError}</p>}
          <p className="text-xs text-muted-foreground/70">
            Debug panel still shows the raw tool output — only the LLM-facing tool_result is reshaped.
          </p>
        </div>
        <DialogFooter>
          <Button variant="ghost" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button onClick={handleSave}>Save</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

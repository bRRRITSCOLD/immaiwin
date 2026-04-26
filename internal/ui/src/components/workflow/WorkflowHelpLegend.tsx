import { useState } from 'react'
import { HelpCircle, ChevronDown, ChevronUp } from 'lucide-react'

export function WorkflowHelpLegend() {
  const [open, setOpen] = useState(false)

  return (
    <div
      className="rounded-lg border bg-card text-card-foreground shadow-md overflow-hidden"
      style={{ minWidth: 220, maxWidth: 460, width: 'max-content' }}
    >
      <button
        className="flex items-center gap-2 w-full px-3 py-2 text-xs font-medium hover:bg-muted/50 transition-colors"
        onClick={() => setOpen((v) => !v)}
      >
        <HelpCircle className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
        <span>Template Reference</span>
        {open ? <ChevronUp className="h-3.5 w-3.5 ml-auto" /> : <ChevronDown className="h-3.5 w-3.5 ml-auto" />}
      </button>

      {open && (
        <div className="border-t px-3 py-2 space-y-2 text-[10px]">
          <section>
            <p className="text-muted-foreground font-semibold uppercase tracking-wide mb-1">Fields (use &#123;&#123;…&#125;&#125;)</p>
            <table className="w-full border-separate" style={{ borderSpacing: '0 2px' }}>
              <tbody>
                <Row code="{{context.stepName.input.field}}" desc="named step's input" />
                <Row code="{{context.stepName.output.field}}" desc="named step's output" />
                <Row code="{{context.stepName.item.field}}" desc="for_each current element (body only)" dimDesc />
                <Row code="{{params.key}}" desc="workflow parameter" />
              </tbody>
            </table>
          </section>

          <section>
            <p className="text-muted-foreground font-semibold uppercase tracking-wide mb-1">JS Scripts (no &#123;&#123;&#125;&#125;)</p>
            <table className="w-full border-separate" style={{ borderSpacing: '0 2px' }}>
              <tbody>
                <Row code="context.stepName.input" desc="named step input" />
                <Row code="context.stepName.output" desc="named step output" />
                <Row code="context.stepName.item" desc="for_each element (body only)" dimDesc />
                <Row code="params.key" desc="workflow parameter" />
                <Row code="$(html)" desc="jQuery-like HTML selector" />
                <Row code="parseRSS(xml)" desc="parse RSS/Atom feed" />
                <Row code="now()" desc="current UTC timestamp" />
                <Row code="parseDate(str)" desc="parse date string → ISO" />
              </tbody>
            </table>
          </section>
        </div>
      )}
    </div>
  )
}

function Row({ code, desc, dimDesc }: { code: string; desc: string; dimDesc?: boolean }) {
  return (
    <tr>
      <td className="pr-3 align-top">
        <code className="text-[10px] text-foreground whitespace-nowrap">{code}</code>
      </td>
      <td className={`align-top ${dimDesc ? 'text-muted-foreground/60' : 'text-muted-foreground'}`}>{desc}</td>
    </tr>
  )
}

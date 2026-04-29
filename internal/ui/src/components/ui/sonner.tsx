import { Toaster as Sonner, type ToasterProps } from 'sonner'

// Toaster wraps sonner with the project palette. richColors=true enables
// type-aware backgrounds — toast.error() lands red, toast.success() lands
// green — and we override the CSS vars so success/error read as on-brand
// shades instead of sonner's defaults. Black-on-black was the prior look
// because the base bg used --popover (matches the dark canvas) and there
// was no per-type color, so the user couldn't tell success from error at
// a glance.
function Toaster({ ...props }: ToasterProps) {
  return (
    <Sonner
      theme="dark"
      richColors
      closeButton
      position="bottom-right"
      className="toaster group"
      style={
        {
          // Default (info / loading / generic) toasts. Same as before.
          '--normal-bg': 'var(--popover)',
          '--normal-text': 'var(--popover-foreground)',
          '--normal-border': 'var(--border)',

          // Success — green-700 background, white text, green-300 border.
          // High contrast against the dark canvas; mirrors the Run button.
          '--success-bg': '#15803d',
          '--success-text': '#ffffff',
          '--success-border': '#86efac',

          // Error — red-700 background, white text, red-300 border. Same
          // colour the Cancel button uses, so error toasts read as
          // "destructive" without ambiguity.
          '--error-bg': '#b91c1c',
          '--error-text': '#ffffff',
          '--error-border': '#fca5a5',

          // Warning + info — amber + sky for the rare uses.
          '--warning-bg': '#b45309',
          '--warning-text': '#ffffff',
          '--warning-border': '#fcd34d',
          '--info-bg': '#0369a1',
          '--info-text': '#ffffff',
          '--info-border': '#7dd3fc',
        } as React.CSSProperties
      }
      {...props}
    />
  )
}

export { Toaster }

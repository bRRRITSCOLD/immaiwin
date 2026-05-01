# Branding

Asset directory for the burrow brand. Hand-written geometric SVGs shipped as v0; commission a designer when polish matters more than parity.

## Name

**Burrow** — workflow + AI agent platform. Gophers burrow tunnels; workflows tunnel through chained nodes. Pun does double duty (Go community mascot tie-in + literal workflow metaphor).

## Mascot

A gopher (the Go mascot, [Renee French's design lineage](https://go.dev/blog/gopher)). Two mark variants:

- **Wordmark** (full logo, used in README + docs header): cross-section of a tunnel network with a gopher visible at one node. The graph-as-burrow metaphor — literal node-network = literal burrow-network.
- **Icon mark** (favicon, app shortcut, small surfaces): a gopher peeking out of a stylized "B"-shaped burrow hole. Minimalist, recognizable at 16×16.

## Color

Earthy palette to lean into the burrow / dirt / Go-mascot heritage:

- **Burrow brown** — primary tunnel/dirt tone (~`#8B5E34`)
- **Gopher cyan** — accent, matches the official Go gopher (~`#00ADD8`)
- **Soil dark** — background on dark-mode UI (~`#1A0F0A`)
- **Tunnel cream** — light-mode background highlight (~`#F4E9D8`)

Final hex values to be set by designer; the tokens above are placeholders.

## Files

```
branding/
├── README.md                 ← this file
├── wordmark.svg              ← full logo for README header + docs (light bg)
├── wordmark-dark.svg         ← dark-mode variant (light text on dark bg)
├── icon.svg                  ← square mark for favicon + app shortcut
├── icon-16.png               ← TODO (designer): favicon raster fallback
├── icon-512.png              ← TODO (designer): PWA / app shortcut
├── social-card.png           ← TODO (designer): OpenGraph 1200×630
└── tokens.json               ← TODO (designer): brand colors as design tokens
```

The SVGs are hand-written geometric placeholders meant to ship the wordmark + favicon-class icon without blocking on a designer. Raster (`icon-*.png`, `social-card.png`) and the formal token file remain designer tasks — flag in any rebrand pass.

## Usage

README header uses both wordmark variants via `<picture>` so light + dark themes resolve to the matching variant:

```markdown
<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="branding/wordmark-dark.svg">
    <img src="branding/wordmark.svg" alt="burrow" height="80">
  </picture>
</p>
```

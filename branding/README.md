# Branding

Asset directory for the burrow brand. Empty placeholders today — drop finals here when a designer ships them.

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

## Files (when ready)

```
branding/
├── README.md                 ← this file
├── wordmark.svg              ← full logo for README header + docs
├── wordmark-dark.svg         ← dark-mode variant
├── icon.svg                  ← square mark for favicon + app shortcut
├── icon-16.png               ← favicon raster fallback
├── icon-512.png              ← PWA / app shortcut
├── social-card.png           ← OpenGraph 1200×630
└── tokens.json               ← brand colors as design tokens
```

## Usage

Until the SVGs land, the README leans on the **burrow** wordmark via plain text. After designer drop:

```markdown
<p align="center">
  <img src="branding/wordmark.svg" alt="burrow" height="80" />
</p>
```

## History

Renamed from `immaiwin` (a riff on the JG Wentworth jingle, "it's my money and I want it now"). The original project was a Polymarket / Schwab / news-scraper toolkit; over time the workflow + AI agent layer outgrew the finance domain, so the project pivoted and dropped the name. See PR #38 for the domain-code rip and PR-where-this-ships for the rename.

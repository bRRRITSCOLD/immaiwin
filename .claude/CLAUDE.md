# Claude

## Rules
### Guidelines
* Be as concise as possible when thinking and planning, do not use unneeded fluff or filler. However, if the fluff or filler is needed for a better result please still include it.
* Project Structure
    ```text
    ├── cmd/                # External facing commands
    │   ├── api/
    │   ├── ui/
    │   └── worker/
    ├── internal/           # Internal modules (not exposed to user)
    │   ├── api/
    │   ├── config/
    │   ├── futures/
    │   ├── mongodb/
    │   ├── news/
    │   ├── options/
    │   ├── polymarket/
    │   ├── rediss/
    │   ├── schwab/
    │   ├── trade/
    │   ├── ui/
    │   ├── watchlist/
    │   └── worker/
    ├── tools/              # Dev dependencies and tools
    ├── scripts/            # Dev scripts
    │   └── test/
    ├── .private/           # Private docs/info (git-ignored)
    │   └── certs/
    └── .claude/            # Information for Claude AI agent
    ```
* Coming Soon!

### Coding
[CODING.md](./rules/CODING.md)

### Testing
[TESTING.md](./rules/TESTING.md)

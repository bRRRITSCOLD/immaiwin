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
    │   ├── api/            # HTTP server, routes, handlers
    │   ├── auth/           # JWT issue/parse, request-ctx user/tenant
    │   ├── config/         # Environment configuration
    │   ├── email/          # Transactional email sender (log + SMTP)
    │   ├── llm/            # LLM provider registry (Anthropic / OpenAI / Ollama)
    │   ├── mongodb/        # MongoDB repositories
    │   ├── rediss/         # Redis client wrapper + broadcaster
    │   ├── sandbox/        # Container sandbox engine (Docker / k3s + gVisor)
    │   ├── skills/         # Skill registry + local-fs source for AI agent tool catalogs
    │   ├── ui/             # Frontend (React + TanStack Start)
    │   ├── worker/         # Background worker implementations
    │   └── workflow/       # Workflow engine (executor, types, connections)
    ├── tools/              # Dev dependencies and tools
    ├── scripts/            # Dev scripts
    │   └── test/
    ├── skills/             # Bundled skill definitions
    ├── examples/           # Reference deployments (k3s)
    ├── .private/           # Private docs/info (git-ignored)
    │   └── certs/
    └── .claude/            # Information for Claude AI agent
    ```
* Coming Soon!

### Coding
[CODING.md](./rules/CODING.md)

### Testing
[TESTING.md](./rules/TESTING.md)

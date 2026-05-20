# Burrow Roadmap

> Living doc — what's shipped, what's in flight, and where we're headed.
> Public-facing companion to the README's [What makes this different](README.md#what-makes-this-different).

Burrow is **sandboxed automation + AI agents + durable runs on your own infra**.
The differentiator is the built-in `code_execute` tool (gVisor-isolated) with
DAP/CDP debug for every workflow, plus multi-tenancy and durable execution
baked into the engine instead of bolted on after.

---

## Shipped

### Platform
- Multi-tenant auth (email/password, OAuth Google + GitHub, API keys), invites, ownership transfer, RBAC (owner/admin/member)
- Audit log — append-only ledger of privileged actions
- Run metrics + worker-health dashboards (`/admin`)
- Reaper worker (sweeps stuck `running` / `paused` / `pending_approval` runs)
- Transactional email (SMTP — Mailpit dev sink or any standard relay)
- CI gate (vet / lint / `test -race` / typecheck) on every push + PR, branch protection on `main`

### Workflow engine
- Visual canvas (React Flow) with 8 node types — `trigger`, `http_request`, `sandbox_script`, `ai_agent`, `for_each`, `mongo_request`, `redis_request`, `notify`
- Live run streaming over WebSocket — trace timeline, per-node status, cost badges
- Webhook trigger (HMAC-SHA-256 signed)
- Cron, Manual, RabbitMQ, Redis-subscribe, **WebSocket** triggers (all migrated to durable lease workers). The WebSocket trigger is provider-agnostic — operator-supplied bearer/header/query auth, exponential reconnect with ping/pong heartbeat, `event_path` dot-extract on each frame
- Universal `on_error: stop | continue` on every fallible node + the agent + the for_each
  - `continue` is **dual-edge** (fires error AND success edges)
  - Approval-gate rejection is a hard veto — policy can't downgrade it
- `for_each` loop with `items` selector (`{{input.docs}}`), node-level loop-abort policy, loop-iteration trace badges, plain-body `iter K/M` sync across the body subgraph
- Cancel propagates into long internal loops (heartbeat → run-ctx cancel)
- Approval gates — per-node + per-tool, durable across worker restarts, OOB via Slack / email magic link
- Durable execution: lease workers, checkpoint-per-step resume, boot-time stuck-run sweep, no-orphans on api restart
- **Workflow enable/disable toggle** — `PATCH /api/v1/workflows/:id/enabled` flips a first-class state; disabled workflows drop from every trigger worker's sync-tick active set (cron / RabbitMQ / Redis-subscribe) while staying fully editable + manually runnable. Backfilled on boot so existing rows default to enabled.
- **Workflow rename** — `PATCH /api/v1/workflows/:id/name` rewrites the display name without round-tripping the whole graph. Inline double-click / pencil-button edit in the sidebar; audit-logged.

### AI agent
- ReAct loop with Anthropic, OpenAI, Ollama providers behind a single `Provider` interface
- Built-in `code_execute` tool (always-on when sandbox wired) — the unfair-advantage surface
- Edge-bound tool nodes (`as_tool`) — any node becomes an agent tool by toggling a switch
- Tool input validated against JSON-Schema before dispatch (LLM gets a structured violation message, no handler runs)
- Skills system — manifest + Local-FS source + Mongo registry + loader; bundled skills shipped (e.g. `weather-formatter`)
- Chat memory (Mongo, tenant-scoped, durable)
- Per-run + per-day cost caps; cost surfaced in the Runs tab
- `as_tool` full isolation — only the agent's `tool` edge wires into a tool node (the engine bypasses its BFS edges anyway)
- **Tool authorization policy** — per-agent ACL via `data.allowed_tools: string[]`. Filter is applied uniformly to built-ins, skill tools, and edge-bound `as_tool` targets AFTER the catalog is built. LLM never sees denied tools; defense-in-depth Execute path returns unknown-tool for hallucinated calls so a denied handler can never fire. Empty list = open (back-compat). Per-workflow rollup is "every agent's list" — finer than the original "per-workflow" framing.
- **Canvas Continue + breakpoints control channel** — REST endpoints (`POST /workflow_runs/:id/continue`, `PUT /workflow_runs/:id/breakpoints`) publish `RunControlMessage`s on `burrow:run_control:<runID>`. Worker bridge subscribes during the run-pass and routes inbound messages into the executor's in-process `continueCh` + `SetBreakpoints` primitives. Restores the lease-era debug UX (mid-run breakpoint add/remove, Continue button releases a pre-exec pause) without breaking the canvas-WS-as-pure-subscriber contract. Tenant-scoped; terminal runs refuse with 400.
- Approval-gate resume is **replay-only** — no extra `Chat` call on resume. The trailing assistant tool_use is replayed from `PausedAgent.Messages`, dispatch continues from the saved `PartialNextIndex`, and the only post-resume LLM call is the final-answer prompt that observes the tool_result. Total Chat per gate = pre-gate + final-answer (locked by integration tests asserting the exact Chat-call count).

### Security
- `mongo_request` / `redis_request` and `rabbitmq` / `redis_subscribe` triggers **require** an explicit user connection — the platform's own Mongo/Redis (workflow records, audit log, lease, chat memory) are not reachable from a user workflow
- Server-side `UpsertWorkflow` connection validation — half-wired workflows are rejected at save with `400 {missing[…]}` (defense-in-depth on top of the worker refusal)
- HTTP SSRF dial-guard — refuses loopback / link-local (including cloud metadata `169.254.169.254`) / RFC1918 / CGNAT / multicast / unspecified / IPv6 ULA / IPv6 link-local; per-node `allowed_hosts` opt-in; **DNS-rebind safe** (check fires at dial time, after resolution)
- Approval rejection IGNORES `on_error` (security invariant) on both inline and durable-resume paths

### Sandbox + infra
- Multi-language execution — JS, Python, Go, Rust, PHP
- Two backends: Docker (single-node dev) + Kubernetes API (single-node k3s today; portable to upstream Kubernetes — see `examples/k3s/MIGRATION.md`)
- gVisor (`runsc`) syscall-level isolation via k3s `RuntimeClass`
- Custom sandbox images (`data.custom_image` per node)
- Per-node package list (`data.packages`) for installable runtimes
- Streaming sandbox WS endpoint (`/api/v1/sandbox/run`) with stdout / stderr / output frames
- Interactive debug — DAP for Python, CDP for JavaScript

---

## Up next (short-term)

### Composability
- **Sub-workflow as a tool** — agent calls another workflow by id, with recursion-depth + cycle guards. The highest-value composability lever still unbuilt.
- **Output-schema enforcement** on agent results (the provider `tool_choice: "required"` trick on existing `output_schema` field)
- **Parallel agent inside `for_each`** — opt-in, bounded parallelism (default sequential to protect token budgets)
- **Multi-agent workflows** — agent-to-agent handoff
- **Transform node** — drop-in node in front of any tool to reshape its output before it hits the agent context (token-saving)

### Engine
- **Per-iter agent checkpoint** — worker death mid-ReAct resumes at the same iteration instead of restarting the loop
- **Per-tool gate Skip button** (vs Reject) — Skip = soft, agent observes the rejection and pivots; Reject = hard veto
- **Distributed concurrency control per workflow** — `concurrency: N` across the worker pool
- **Per-skill `on_error`** — follow-up to the universal node policy

### Security + governance
- **Per-tenant sandbox isolation** — namespaces, RBAC, secret scoping; tenant workflows can't see each other's sandbox pods
- **Per-tool cost weights / budget alerts**
- **Provider fallback** — Anthropic 5xx → OpenAI
- **Per-tenant cost cap** (per-run + per-day already exist)

### Sandbox + infra
- **Container pool warm starts** — keep a warm pool of language-runtime pods so cold start is sub-100ms (versus the current per-run create + image-pull + entrypoint)
- **Image-layer caching for `packages`** — skip the per-run pip / npm / cargo install when the `packages` list is the same as a previously-cached layer
- **Multi-node Kubernetes validation** — exercise the k3s backend on real multi-node clusters; verify HPA, pod priority, ResourceQuotas, anti-affinity behave as expected

### Standard integrations + connectors
- **Slack OOB approvals — full ladder** (bot token → Block Kit interactive buttons → per-tenant OAuth install → Slack Connect)
- **Connector nodes** — Gmail, Google Docs, Google Sheets, S3, SQS, SNS, Postgres, MySQL, CockroachDB, CouchDB, DynamoDB, Slack send

### UX + dev surface
- **Per-node debug JSON viewer** — current workflow context, inputs, outputs, item per iteration
- **Save / Revert** — modal on Run when unsaved changes exist; Revert undoes unsaved edits
- **Right-click → node palette** — place node at cursor location
- **Auto-cancel on page navigate-away / refresh** (with the refresh case opt-in)
- **Pulsing approval indicator** — gate-pending visibility, so the user notices the workflow is waiting on them
- **Scroll-wheel scope** — scroll wheel zooms the canvas only when the cursor is on the canvas, not inside a node body
- **Per-tool reordering** on AI Agent nodes
- **Audit-log coverage** for run-status events (run / approve / reject)
- **Replay run diff** — compare past runs side-by-side
- **Workflow canvas Save validation → Zod** (cleanup of the current ad-hoc validators)
- **k3s / k8s control panel inside `/admin`**

### Observability + ops
- **CI: E2E Playwright tier** — alongside unit + integration
- **Refactor to use `enviro-go` everywhere** (env config consistency)
- **Webhook trigger horizontal scale-out** — separate worker plane so spiky webhook traffic doesn't starve other triggers

---

## Aspirational

- **Chat-platform triggers** — Slack / Discord / Telegram conversational agent loops on top of existing memory + agent
- **Agent eval / replay harness** — fixture-based regression suite, CI-failable on agent-behaviour drift
- **Tool synthesis** — agent authors and registers a new sandbox-tool mid-run when it identifies a capability gap (versioned, sandboxed, reviewable)
- **Shell-command custom-image nodes** — run a single shell command instead of user code

---

## Process / debt

- Re-walk the durable-execution smoke checklist (items #3–#9 from the Phase 3 plan), or formally retire those scenarios
- PLAN doc-drift sweep — re-tick checkboxes for items shipped during the lease + on_error sprints (OpenAI/Ollama providers, cost in Runs, cancellation-through-tool-calls, etc.)
- Audit-log coverage gap — per-skill / per-tool actions


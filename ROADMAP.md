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
- Cron, Manual, RabbitMQ, Redis-subscribe triggers (all migrated to durable lease workers)
- Universal `on_error: stop | continue` on every fallible node + the agent + the for_each
  - `continue` is **dual-edge** (fires error AND success edges)
  - Approval-gate rejection is a hard veto — policy can't downgrade it
- `for_each` loop with `items` selector (`{{input.docs}}`), node-level loop-abort policy, loop-iteration trace badges, plain-body `iter K/M` sync across the body subgraph
- Cancel propagates into long internal loops (heartbeat → run-ctx cancel)
- Approval gates — per-node + per-tool, durable across worker restarts, OOB via Slack / email magic link
- Durable execution: lease workers, checkpoint-per-step resume, boot-time stuck-run sweep, no-orphans on api restart

### AI agent
- ReAct loop with Anthropic, OpenAI, Ollama providers behind a single `Provider` interface
- Built-in `code_execute` tool (always-on when sandbox wired) — the unfair-advantage surface
- Edge-bound tool nodes (`as_tool`) — any node becomes an agent tool by toggling a switch
- Tool input validated against JSON-Schema before dispatch (LLM gets a structured violation message, no handler runs)
- Skills system — manifest + Local-FS source + Mongo registry + loader; bundled skills shipped (e.g. `weather-formatter`)
- Chat memory (Mongo, tenant-scoped, durable)
- Per-run + per-day cost caps; cost surfaced in the Runs tab
- `as_tool` full isolation — only the agent's `tool` edge wires into a tool node (the engine bypasses its BFS edges anyway)

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

## In progress

- Container pool warm starts (sub-100ms cold)
- Image-layer caching for `packages` to skip per-run installs
- Multi-node Kubernetes validation (HPA, pod priority, ResourceQuotas)
- Per-tenant isolation in the sandbox (namespaces, RBAC, secret scoping)

---

## Up next (short-term)

### Composability
- **Sub-workflow as a tool** — agent calls another workflow by id, with recursion-depth + cycle guards. The highest-value composability lever still unbuilt.
- **Output-schema enforcement** on agent results (the provider `tool_choice: "required"` trick on existing `output_schema` field)
- **Parallel agent inside `for_each`** — opt-in, bounded parallelism (default sequential to protect token budgets)
- **Multi-agent workflows** — agent-to-agent handoff
- **Transform node** — drop-in node in front of any tool to reshape its output before it hits the agent context (token-saving)

### Engine
- **Workflow enable/disable toggle** — first-class `enabled` state on every workflow (today the only way to stop one firing is to delete it, which is destructive). Disabled workflows drop from cron / RabbitMQ / Redis-subscribe / WebSocket worker sync-tick active sets; trigger events get dropped (cron) or stay queued (RMQ) per source semantics; the manual `Run` button stays usable so authors can still test in isolation. Backfill all existing workflows to `enabled: true`. ~1 day.
- **Approval-gate resume cost optimisation** — every approval gate today pays one extra `Chat` call on resume, because the executor pops the trailing assistant tool_call before persisting (most providers reject a re-saved un-answered tool_call) and the resumed agent re-prompts the model fresh. With long prompts or many gates per run, the per-resume token cost adds up. Two mitigation paths:
  1. **Per-provider request-id idempotency** to dedupe the resume `Chat` (where the provider supports it — Anthropic / OpenAI).
  2. **Stateful resume mode** that replays the LLM's previous response from the saved trace instead of re-prompting (cheaper but constrained by tool-call protocol invariants).
  High-leverage cost win for any user running approval-gated agent workflows at volume.
- **Per-iter agent checkpoint** — worker death mid-ReAct resumes at the same iteration instead of restarting the loop
- **Per-tool gate Skip button** (vs Reject) — Skip = soft, agent observes the rejection and pivots; Reject = hard veto
- **Canvas Continue + breakpoints control channel** — restore the lease-era debug UX (set / clear breakpoints mid-run, Continue button releases a pause)
- **Distributed concurrency control per workflow** — `concurrency: N` across the worker pool
- **Per-skill `on_error`** — follow-up to the universal node policy

### Security + governance
- **Tool authorization policy** — per-workflow ACL ("this agent may call HTTP but not Mongo/Redis")
- **Per-tool cost weights / budget alerts**
- **Provider fallback** — Anthropic 5xx → OpenAI
- **Per-tenant cost cap** (per-run + per-day already exist)

### Standard integrations + connectors
- **Generic WebSocket trigger** — provider-agnostic, user-supplied bearer token (no per-provider OAuth maze)
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


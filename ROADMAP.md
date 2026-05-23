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
- Visual canvas (React Flow) with 10 node types — `trigger`, `http_request`, `sandbox_script`, `ai_agent`, `for_each`, `mongo_request`, `redis_request`, `notify`, `sub_workflow`, `return`
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
- **Sub-workflow as an agent tool** — `sub_workflow` node bound to another workflow id, dispatched by the AI agent via `as_tool`. Tenant-scoped (cross-tenant dispatch refused at save AND run); cycle detection via ctx-stashed ancestor chain; depth cap (`maxSubWorkflowDepth = 5`) prevents runaway recursion. Structural refusals (cycle / tenant / depth) terminate the agent loop regardless of the tool target's `on_error` policy — LLM cannot recover, so retry would only burn tokens.
- **`return` node + workflow OutputSchema** — explicit single-payload contract for sub-workflow consumers (Temporal/Airflow-style return). Payload templates resolve via the standard template engine (`{{context.X}}`, `{{input.X}}`, `{{run_input.X}}`) including nested maps/arrays + BSON-decoded run-state. At most one return node per workflow (enforced at save). No return node = `null` output (explicit "no contract" signal, not implicit last-step).
- **Workflow `input_schema` + pre-flight Run dialog** — workflows declare the shape of their run input via typed `SchemaEntry[]` (string / number / boolean / enum) OR raw JSON Schema (escape hatch for nested objects + arrays). Engine validates on every dispatch (manual, trigger, sub_workflow) and surfaces `ErrInputValidation`. The canvas Run button opens a typed form OR raw-JSON editor seeded by recursing the schema's `properties` / `items` and honoring `default` annotations.
- **`{{run_input.X}}` template namespace + sandbox global** — the workflow's initial trigger payload is reachable from nodes at any depth in the BFS, independent of predecessor edges. Distinct from `{{input.X}}` (predecessor output). Top-level `run_input` global is also exposed in all 5 sandbox runtimes, parallel to existing `input` / `config`.
- **`params` → `config` rename, end-to-end** — engine struct field, BSON tag, sandbox payload, all 5 lang entrypoints (JS / Python / Go / Rust / PHP), bundled skill source, UI types + visible labels, bundled workflow templates. Single name across the stack; pre-launch so no back-compat shim.
- **Per-node `output_transform`** — every actionable node has a collapsible `Output transform` editor in its config panel. JSON template, same engine as the return node — `{{input.X}}` resolves against the node's RAW output; `{{context.X}}`, `{{config.X}}`, `{{run_input.X}}` standard. Engine applies the template after the node's handler returns; the StepResult + step_done event keep the RAW output on `.Output` and the reshape on `.TransformedOutput`, so the canvas debug panel renders both panes (raw + transformed). Downstream BFS edges + agent-tool dispatches see the transformed value. Replaces the earlier standalone `transform` node and the edge-level tool-output transform — one mental model for "reshape what this node hands downstream".
- **Agent-as-tool (Multi-agent A)** — `ai_agent` nodes opt-in as agent tools via the standard `as_tool.enabled` toggle (same panel mongo / http / sandbox nodes use). Parent agent dispatches a child agent directly through its `tool` edge — child runs its own ReAct loop with the parent's tool args as initial input and returns its final answer as tool_result. A sub-agent keeps its own `tool` source handles, so a specialist can call its own tools (http / mongo / sandbox / further sub-agents). Ancestor-chain ctx guard refuses agent cycles (parent → child → parent) + caps nesting depth at 5; structural refusals route through `isInfraToolError` (`ai_agent:` prefix) so they always terminate the parent loop regardless of `on_error: continue`. No sub_workflow wrapper needed for the common "delegate to a specialist" pattern.
- **Node duplicate** — Cmd/Ctrl+D on a selected node (or right-click → Duplicate) clones with fresh ID + handles + no edges. Multiple selection works; clones land selected so a series of presses stamps copies.
- **Wider canvas zoom range** — minZoom 0.1, maxZoom 4 (vs React Flow defaults 0.5 / 2). Lets large multi-agent graphs fit on a single screen.

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
- **Debug-pause durability across worker restarts** — the executor checkpoints `execution_state` at every pre-exec breakpoint wait (BFS and as_tool target). On worker death, `ClaimLease` requires `execution_state != nil` for lapsed-lease branch (orphan-no-checkpoint runs are terminated by the worker's periodic sweep, which publishes a synthetic `run_done`). The api's run/stream WS sends keepalive pings every 20s so an idle pause doesn't drop the socket. Workers emit periodic `worker_heartbeat` events on the per-run channel so the canvas distinguishes a legit quiet pause from a dead worker (15s threshold). Live UI surfaces a "Worker quiet" banner when the gap exceeds the threshold, and the per-node yellow paused marker is preserved across worker death + non-cancel terminal events; one node pulses at a time. As_tool target breakpoints persist `PausedAgent` mid-loop so reclaim resumes at exactly the inner tool call — no re-running the agent loop, no extra LLM call.
- Approval-gate resume is **replay-only** — no extra `Chat` call on resume. The trailing assistant tool_use is replayed from `PausedAgent.Messages`, dispatch continues from the saved `PartialNextIndex`, and the only post-resume LLM call is the final-answer prompt that observes the tool_result. Total Chat per gate = pre-gate + final-answer (locked by integration tests asserting the exact Chat-call count).
- **Provider-side output-schema enforcement** — when an agent declares an `output_schema`, the loop sends `tool_choice: "any"` (Anthropic) / `"required"` (OpenAI) on every iter so the model cannot emit free-text answers. The synthetic `submit_final_answer` tool stays the exit channel. Existing post-call rejection-retry remains as a defensive backstop for providers (e.g. ollama) that don't honor tool_choice. Cuts the wasted Chat round trip when the model would otherwise emit a free-text answer that gets rejected.

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
- **Container pool warm starts** — boot-time async warm + per-Acquire liveness check + 24h `activeDeadline` on warm pods + tight post-attach termination poll (10ms init / 50ms cap). Pool key is `(lang, image-tag, network, mem, cpu)`, so package-using runs hit the warm pool too.
- **Image-layer caching for `packages`** — docker `BuildOrReuse` (sha256-tagged image, reused on next run) + k3s variant that pushes the built image to the configured registry so containerd can pull. Pushed-tag cache prevents redundant pushes across the pool.

---

## Up next (short-term)

### Composability
- **Multi-agent workflows (C)** — orchestrator pattern: coordinator agent + parallel fan-out primitive over specialist sub-agents (now that A is shipped). Depends on bounded-parallelism semantics shared with the for_each work below.
- **Parallel agent inside `for_each`** — opt-in, bounded parallelism (default sequential to protect token budgets)
- **`ReturnNode` payload autofill from `output_schema_json`** — symmetry with the run-input pre-flight dialog (stub from schema, honor `default`, recurse nested)

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
- **Handler-function sandbox runtimes** — rewrite the per-language entrypoints (JS / Python / Go / Rust / PHP) so user scripts export a `handler(input, ctx)` function instead of running top-level statements. Runtime imports the user module, invokes the handler, treats the return value as node output, and treats a thrown exception / non-nil error as the node error (no more stdout-parsing or sentinel framing). Benefits: clean input/output contract, structured errors with stack traces, easier to layer middleware (timeouts, tracing, ctx cancellation, secrets injection) inside the runtime without touching user code. Migration is a breaking change to the `sandbox_script` node — needs a versioned schema field so old graphs keep working.

---

## Process / debt

- Re-walk the durable-execution smoke checklist (items #3–#9 from the Phase 3 plan), or formally retire those scenarios
- PLAN doc-drift sweep — re-tick checkboxes for items shipped during the lease + on_error sprints (OpenAI/Ollama providers, cost in Runs, cancellation-through-tool-calls, etc.)
- Audit-log coverage gap — per-skill / per-tool actions


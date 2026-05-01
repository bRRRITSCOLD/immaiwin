<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="branding/wordmark-dark.svg">
    <img src="branding/wordmark.svg" alt="burrow" height="80">
  </picture>
</p>

**Visual workflow automation with sandboxed multi-language code execution and interactive debugging.**

Think **n8n** meets **Replit** — a canvas-based workflow builder where every node can run real code (JavaScript, Python, Go, Rust, PHP) inside isolated Docker containers with gVisor-grade security. Step through your code with breakpoints. Chain data transformations across languages. Connect to any WebSocket, REST API, or message queue. All self-hosted, all open source.

The long game: add AI agents that can write and execute code as part of their reasoning — like [OpenClaw](https://github.com/openclaw) but inherently safe, because every script runs in a sandboxed container with syscall interception, no network by default, and hard resource limits. The agent can't escape the sandbox.

---

## Table of contents

- [What makes this different](#what-makes-this-different)
- [Architecture](#architecture)
- [Core features](#core-features)
  - [Visual workflow builder](#visual-workflow-builder)
  - [Multi-language sandbox execution](#multi-language-sandbox-execution)
  - [Interactive debugging (DAP/CDP)](#interactive-debugging-dapcdp)
  - [Security model](#security-model)
  - [Workflow triggers](#workflow-triggers)
  - [Multi-tenancy + auth](#multi-tenancy--auth)
  - [Admin dashboard](#admin-dashboard)
- [Tech stack](#tech-stack)
- [Prerequisites](#prerequisites)
- [Quick start](#quick-start)
  - [1. Clone and configure](#1-clone-and-configure)
  - [2. Start infrastructure](#2-start-infrastructure)
  - [3. Build sandbox images](#3-build-sandbox-images)
  - [3b. (Optional) k3s + gVisor backend](#3b-optional-k3s--gvisor-backend)
  - [4. Setup and run](#4-setup-and-run)
  - [5. Start workers (optional)](#5-start-workers-optional)
  - [6. Start / tear down a session](#6-start--tear-down-a-session)
- [Project structure](#project-structure)
- [API endpoints](#api-endpoints)
  - [Workflows](#workflows)
  - [Sandbox](#sandbox)
  - [Connections](#connections-data-sources-llm-providers)
  - [Auth + multi-tenancy](#auth--multi-tenancy)
  - [Admin](#admin)
  - [Workflow runs](#workflow-runs)
  - [Webhooks](#webhooks)
- [Roadmap](#roadmap)
- [Development](#development)
- [License](#license)

### Other docs

| Doc | What |
|---|---|
| [TESTING.md](TESTING.md) | Coverage catalog — every unit / integration / e2e test in the repo, plus open coverage gaps. Updated on every test-touching PR. |
| [.claude/CLAUDE.md](.claude/CLAUDE.md) | Project rules entry point for the Claude AI agent. Links to coding + testing rules. |
| [.claude/rules/CODING.md](.claude/rules/CODING.md) | Coding standards (DRY, UI library, env-var loading, audit-logging vigilance). |
| [.claude/rules/TESTING.md](.claude/rules/TESTING.md) | Test authoring rules — naming convention, descriptor requirement, suite-template snippets per tier. |
| [examples/k3s/README.md](examples/k3s/README.md) | Single-node k3s + gVisor reference deployment for the sandbox runtime. |
| [examples/k3s/MIGRATION.md](examples/k3s/MIGRATION.md) | Porting the k3s deployment to upstream Kubernetes (kubeadm / EKS / GKE / AKS). |
| [internal/ui/README.md](internal/ui/README.md) | Frontend package readme (placeholder; defers to root for instructions). |

---

## What makes this different

| Feature | n8n | Zapier | Make.com | **burrow** |
|---------|-----|--------|----------|-------------|
| Visual workflow builder | Yes | Limited | Yes | **Yes** (React Flow canvas) |
| Code execution | JS only | No | No | **5 languages** (JS, Python, Go, Rust, PHP) |
| Sandboxed isolation | No | N/A | N/A | **Docker + gVisor** (syscall-level) |
| Interactive debugging | No | No | No | **DAP/CDP debugger** (breakpoints, step, inspect) |
| AI agent nodes | No | No | No | **Native** (Anthropic / OpenAI / Ollama, edge-bound tool nodes, sandboxed `code_execute`) |
| Multi-tenancy + auth | No | N/A | N/A | **Built-in** (tenants, invites, RBAC, JWT, OAuth login, audit log) |
| Self-hosted | Yes | No | No | **Yes** |

---

## Architecture

```
┌──────────────────────────────────────────────────────────┐
│  Frontend (React + TanStack Start)                        │
│  ┌────────────┐  ┌────────────┐  ┌─────────────────────┐ │
│  │ Workflow    │  │ Monaco     │  │ Debug Panel         │ │
│  │ Canvas     │  │ Code Editor│  │ (Variables, Stack,   │ │
│  │ (React     │  │ (multi-    │  │  Breakpoints, REPL) │ │
│  │  Flow)     │  │  lang)     │  │                     │ │
│  └─────┬──────┘  └─────┬──────┘  └──────────┬──────────┘ │
└────────┼───────────────┼────────────────────┼────────────┘
         │ HTTP/SSE      │ HTTP               │ WebSocket
         ▼               ▼                    ▼
┌──────────────────────────────────────────────────────────┐
│  API Server (Go + Gin)                                    │
│  ┌──────────────────────────────────────────────────┐     │
│  │  Sandbox Manager                                  │     │
│  │  - Container lifecycle (create/start/stop/kill)   │     │
│  │  - stdin JSON → stdout JSON protocol              │     │
│  │  - DAP proxy (Python/debugpy over TCP)            │     │
│  │  - CDP proxy (JS/Node --inspect over WebSocket)   │     │
│  │  - Port allocator for debug sessions              │     │
│  │  - Resource limits (CPU, memory, PIDs, timeout)   │     │
│  └──────────────┬───────────────────────────────────┘     │
│  ┌──────────────┴───────────────────────────────────┐     │
│  │  Workflow Executor                                │     │
│  │  - BFS node traversal with step context           │     │
│  │  - Named steps → cross-node data access           │     │
│  │  - ForEach with cloned context per iteration      │     │
│  └──────────────────────────────────────────────────┘     │
└─────────────────┬────────────────────────────────────────┘
                  │ Docker API
                  ▼
┌──────────────────────────────────────────────────────────┐
│  Container Runtime (Docker / gVisor)                      │
│  ┌────────────────────────────────────────────────────┐   │
│  │  Sandbox Container                                  │   │
│  │  - Language runtime (Node/Python/Go/Rust/PHP)       │   │
│  │  - Non-root user (uid 65534)                        │   │
│  │  - No network by default (opt-in bridge)            │   │
│  │  - cgroup limits: CPU 0.5, MEM 128MB, PIDs 256      │   │
│  │  - Optional: DAP server (debugpy :5678)             │   │
│  │  - Optional: CDP server (node --inspect :9229)      │   │
│  └────────────────────────────────────────────────────┘   │
└──────────────────────────────────────────────────────────┘
```

---

## Core Features

### Visual Workflow Builder

Canvas-based editor powered by React Flow. Drag nodes, connect edges, run entire workflows with one click.

**8 node types:**

| Node | Purpose |
|------|---------|
| `trigger` | Start workflow from WebSocket events, cron, or RabbitMQ |
| `http_request` | Make HTTP requests with full Go http.Client parity (method, headers, query, body, auth, redirects, TLS, JSON parse) |
| `sandbox_script` | Run code in isolated container (5 languages, Docker or k3s+gVisor backend) |
| `ai_agent` | Reason-act-observe LLM loop (Anthropic / OpenAI / Ollama) with edge-bound tool nodes + built-in `code_execute` |
| `for_each` | Loop over arrays with isolated context per iteration |
| `mongo_request` | Generic MongoDB ops: `find`, `find_one_and_update`, `find_one_and_replace`, `insert_one`, `insert_many`, `update_many`, `delete_one`, `delete_many`, `aggregate`, `count_documents`, `distinct`, `cursor_fetch` (server-side cursor pagination) |
| `redis_request` | Generic Redis ops: `publish`, strings (`get`/`set`/`del`/`incr`/`decr`/`expire`/`ttl`/`exists`/`keys`/`mget`/`mset`), hashes (`hget`/`hset`/`hgetall`/`hdel`), lists (`lpush`/`rpush`/`lpop`/`rpop`/`lrange`/`llen`), sets (`sadd`/`srem`/`smembers`/`sismember`), sorted sets (`zadd`/`zrem`/`zrange`/`zscore`/`zincrby`), streams producer (`xadd`/`xrange`/`xlen`). Subscribe-side will become a trigger node. |
| `notify` | Send notifications |

Every node supports named steps — downstream nodes access upstream results via `context.stepName.output`.

### Multi-Language Sandbox Execution

Write code in any supported language. All use the same protocol: `output(value)` to produce a result.

**JavaScript:**
```javascript
const result = input.data.map(x => x * 2);
output({ doubled: result });
```

**Python:**
```python
import json
result = [x * 2 for x in input["data"]]
output({"doubled": result})
```

**Go:**
```go
result := make([]int, len(input["data"].([]any)))
for i, v := range input["data"].([]any) {
    result[i] = int(v.(float64)) * 2
}
output(map[string]any{"doubled": result})
```

**Rust:**
```rust
let data: Vec<f64> = serde_json::from_value(input["data"].clone()).unwrap();
let doubled: Vec<f64> = data.iter().map(|x| x * 2.0).collect();
output(json!({"doubled": doubled}));
```

**PHP:**
```php
$result = array_map(fn($x) => $x * 2, $input["data"]);
output(["doubled" => $result]);
```

### Interactive Debugging (DAP/CDP)

Set breakpoints in the Monaco editor, click Debug, and step through your code running inside a Docker container.

- **Breakpoint gutter** — click to toggle breakpoints
- **Variables panel** — inspect all in-scope variables at each pause
- **Call stack** — see the full execution path
- **Step controls** — Continue, Step Over, Step In, Step Out
- **Debug console** — evaluate expressions in the paused context
- **Supported languages:** JavaScript (Chrome DevTools Protocol) and Python (Debug Adapter Protocol via debugpy)

Under the hood:
- JS: `node --inspect-brk` → CDP WebSocket → `Runtime.getProperties` for variable inspection
- Python: `debugpy.listen()` → DAP TCP → Content-Length framed messages
- Go proxy translates between browser WebSocket and container debug protocol

### Security Model

Every sandbox container runs with defense-in-depth:

| Layer | Protection |
|-------|-----------|
| **Container isolation** | Separate PID/network/mount namespaces |
| **gVisor** (`runsc`) | User-space kernel — intercepts all syscalls; opt-in via the k3s backend (`SANDBOX_BACKEND=k3s` + `SANDBOX_K3S_RUNTIMECLASS=gvisor`) |
| **Non-root** | Runs as uid 65534 (nobody) |
| **No network** | `--network=none` by default, opt-in per node |
| **Resource limits** | CPU 0.5 cores, 128MB RAM, 256 PIDs, 30s timeout |
| **Read-only code** | User script mounted, no host filesystem access |
| **No secrets** | Only explicit params injected, never host env |

This is what makes AI agent integration safe — an agent can write and execute arbitrary code, but it runs in a sandbox that can't access the host, can't make network calls (unless explicitly allowed), and gets killed after 30 seconds.

### Workflow Triggers

Five ways to kick off workflows:

- **Manual** — run on-demand from the UI
- **Webhook (HTTP)** — POST to `/api/v1/webhooks/<slug>` with optional HMAC signature verification
- **Cron** — schedule workflows on any interval (`workflow-cron` worker)
- **RabbitMQ** — trigger from message queue events (`workflow-rabbitmq` worker)
- **Redis Subscribe** — trigger from Redis pub/sub channels and patterns (`workflow-redis-subscribe` worker)

### Multi-Tenancy + Auth

Every per-user resource (workflows, runs, connections, evals, chat memory, audit) carries a `tenant_id`. Stores filter by ctx tenant.

- **Email + password** registration with bcrypt hashes
- **OAuth login** (Google + GitHub)
- **JWT cookies** (httpOnly, sliding TTL) + **API keys** for programmatic access
- **Tenants** — every user gets a personal tenant on signup; can be invited to additional tenants
- **Roles** — `owner`, `admin`, `member`. Owner-only ops gated; admin-or-owner ops gated; pure reads open to all members
- **Invites** — email-bound, single-use, time-bound tokens; SMTP delivery via Mailpit (dev) or any standard relay (prod)
- **Ownership transfer** — owner can hand off to any admin; demoted to admin atomically
- **Audit log** — append-only ledger of privileged actions (login, logout, password change/reset, OAuth link, API key create/revoke, invite/membership ops, ownership transfer)

### Admin Dashboard

`/admin` (owner/admin only) shows:

- **Run metrics** — total runs + cost, by-status breakdown, top workflows by run count over selectable window (24h / 7d / 30d)
- **Worker health** — live heartbeats, tick counts, last error per registered worker (heartbeat ticker writes every 30s; reaper sweeps stuck runs)

---

## Tech Stack

| Component | Technology |
|-----------|-----------|
| **API** | Go 1.22+, Gin, gorilla/websocket |
| **Frontend** | React 19, TanStack Start, TanStack Form + zod, React Flow, Monaco Editor, Tailwind CSS, shadcn/ui |
| **Database** | MongoDB (documents, configs, workflows) |
| **Cache/PubSub** | Redis (real-time streaming, inter-service messaging) |
| **Message Queue** | RabbitMQ (workflow triggers, event-driven execution) |
| **Containers** | Docker API (default) or k3s + gVisor (opt-in via `SANDBOX_BACKEND=k3s`) |
| **Sandbox Runtimes** | Node.js 20, Python 3.12, Go 1.22, Rust 1.86, PHP 8.3 |
| **Debug Protocols** | DAP (Python/debugpy), CDP (JS/Node --inspect) |
| **LLM Providers** | Anthropic, OpenAI, Ollama (configurable per agent node via Connection) |

---

## Prerequisites

- **Go** 1.22+
- **Node.js** 20+ with `pnpm`
- **Docker** (Docker Desktop or Docker Engine)
- **Make**

## Quick Start

### 1. Clone and configure

```bash
git clone https://github.com/bRRRITSCOLD/burrow.git
cd burrow
cp .env.example .env
# Edit .env with your settings
```

### 2. Start infrastructure

```bash
make docker-compose-up
```

Starts MongoDB, Redis, and RabbitMQ.

### 3. Build sandbox images

```bash
make sandbox-images           # 5 runtime images (JS, Python, Go, Rust, PHP)
make sandbox-images-debug     # 2 debug images (JS, Python — required for interactive debugging)

# k3s backend only — push to the local registry that the cluster pulls from:
make sandbox-images-push      # build + push all of the above (default REGISTRY=localhost:5000)
make sandbox-images-push REGISTRY=registry.local:5000
```

### 3b. (Optional) k3s + gVisor backend

Skip this for the default Docker backend. To run sandboxes in a single-node
k3s cluster with gVisor (`runsc`) syscall-level isolation:

```bash
sudo ./examples/k3s/setup.sh   # installs runsc, k3s, registry; idempotent
make sandbox-images-push       # publishes images to localhost:5000

# in .env:
# SANDBOX_BACKEND=k3s
# SANDBOX_K3S_RUNTIMECLASS=gvisor
# SANDBOX_IMAGE_REGISTRY=localhost:5000
```

See `examples/k3s/README.md` for prerequisites and `examples/k3s/MIGRATION.md`
for porting to upstream Kubernetes (kubeadm / EKS / GKE / AKS).

### 4. Setup and run

```bash
make setup      # Install Go tools, git hooks, frontend deps
make api        # Start API server (https://localhost:8080)
make dev-ui     # Start frontend (http://localhost:3000)
```

### 5. Start workers (optional)

```bash
make list-workers                              # See all available workers
make worker NAME=workflow-cron                 # Cron-triggered workflows
make worker NAME=workflow-rabbitmq             # RabbitMQ-triggered workflows
make worker NAME=workflow-redis-subscribe      # Redis pub/sub-triggered workflows
make worker NAME=reaper                        # Sweeps stuck workflow runs (running/paused/pending_approval)
```

### 6. Start / tear down a session

Inverse pair. Soft start in the morning, soft stop at night — state is
preserved on disk between sessions.

```bash
# Soft start — start k3s, start the local registry, bring up docker compose.
# Idempotent; skips anything already running.
make dev-startup

# First-time bring-up from a clean machine (runs examples/k3s/setup.sh,
# builds + pushes sandbox images, brings up docker compose).
make dev-startup-fresh
```

```bash
# Soft stop — bring down docker compose, k3s, and the local registry.
# Cluster + volume state preserved on disk; bring it back with `make dev-startup`.
make dev-teardown

# Cluster stays up; just delete sandbox pods (override ns/kubeconfig if needed).
make dev-teardown-sandbox
make dev-teardown-sandbox SANDBOX_NS=foo KUBECONFIG_K3S=~/.kube/config

# DESTRUCTIVE — uninstalls k3s (wipes /var/lib/rancher/k3s),
# removes the registry container, removes gVisor (runsc) via apt.
# 5-second Ctrl-C window before the destructive steps fire.
make dev-teardown-full
```

What each touches:

| Component | Source | Stopped by |
|-----------|--------|------------|
| `docker compose` stack (Mongo, Redis, RabbitMQ, …) | `make docker-compose-up` | `dev-teardown` (`docker-compose-down`) |
| Local Docker registry (`registry:2` on `:5000`) | `examples/k3s/setup.sh` | `dev-teardown` (`docker stop registry`); removed by `dev-teardown-full` |
| `k3s.service` (single-node cluster) | `examples/k3s/setup.sh` (calls upstream `get.k3s.io`) | `dev-teardown` (`systemctl stop k3s`); uninstalled by `dev-teardown-full` |
| Sandbox pods inside `burrow-sandbox` namespace | API server creates them at runtime | `dev-teardown-sandbox`, or wiped with the cluster by `dev-teardown-full` |
| gVisor (`runsc` shim) | `examples/k3s/setup.sh` (apt) | `dev-teardown-full` only |

---

## Project Structure

```
├── cmd/
│   ├── api/                    # API server entrypoint
│   ├── ui/                     # UI build entrypoint
│   └── worker/                 # Worker entrypoint (registry-based)
├── internal/
│   ├── api/                    # HTTP server, routes, handlers
│   │   └── handler/            # Request handlers (workflows, sandbox, debug, auth, tenants, etc.)
│   ├── auth/                   # JWT issue/parse, claims, request-ctx user/tenant
│   ├── config/                 # Environment configuration
│   ├── email/                  # Transactional email sender (log + SMTP)
│   ├── llm/                    # LLM provider registry (Anthropic / OpenAI / Ollama)
│   ├── mongodb/                # MongoDB repositories (users, tenants, workflows, runs, audit, …)
│   ├── sandbox/                # Container sandbox engine
│   │   ├── dap/                # DAP client (Python) + CDP client (JS)
│   │   └── runtimes/           # Per-language Dockerfiles + entrypoints
│   │       ├── javascript/
│   │       ├── python/
│   │       ├── golang/
│   │       ├── rust/
│   │       └── php/
│   ├── ui/                     # Frontend (React + TanStack Start)
│   │   └── src/
│   │       ├── components/
│   │       │   ├── ui/         # shadcn/ui components
│   │       │   └── workflow/   # Canvas, nodes, debug dialog
│   │       ├── hooks/          # useDebugSession, useWorkflowStore
│   │       └── routes/         # Page routes
│   ├── skills/                 # Skill registry + local-fs source (versioned tool catalogs for AI agents)
│   ├── worker/                 # Background worker implementations
│   └── workflow/               # Workflow engine (executor, types)
├── scripts/                    # Dev scripts
└── tools/                      # Dev dependencies
```

---

## API Endpoints

### Workflows
| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/workflows` | List all workflows |
| `PUT` | `/api/v1/workflows/:id` | Create/update workflow |
| `DELETE` | `/api/v1/workflows/:id` | Delete workflow |
| `POST` | `/api/v1/workflows/:id/run` | Execute workflow |
| `GET` | `/api/v1/workflows/:id/run/stream` | WebSocket stream of run events |

### Sandbox
| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/sandbox/debug` | WebSocket upgrade for interactive debug session |
| `GET` | `/api/v1/sandbox/run` | WebSocket upgrade for one-shot sandbox runs (streams stdout/stderr/output) |

### Connections (data sources, LLM providers)

Supported types: `mongodb`, `redis`, `rabbitmq`, `anthropic`, `openai`, `ollama`.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/connections` | List saved connections |
| `PUT` | `/api/v1/connections/:id` | Create/update connection |
| `DELETE` | `/api/v1/connections/:id` | Delete connection |
| `POST` | `/api/v1/connections/test` | Test connection (driver dial / API ping) |

### Auth + Multi-Tenancy
| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/auth/register` | Register new user + personal tenant |
| `POST` | `/api/v1/auth/login` | Email + password login |
| `POST` | `/api/v1/auth/logout` | Clear session cookie |
| `GET` | `/api/v1/auth/me` | Current user + memberships |
| `POST` | `/api/v1/auth/switch_tenant` | Re-issue JWT for a different active tenant |
| `GET` | `/auth/oauth/:provider/start` | OAuth start (google, github) |
| `GET` | `/auth/oauth/:provider/callback` | OAuth callback |
| `POST` | `/api/v1/auth/password/change` | Change password (authed) |
| `POST` | `/api/v1/auth/password/reset/request` | Request reset email |
| `POST` | `/api/v1/auth/password/reset/confirm` | Confirm reset w/ token |
| `POST` | `/api/v1/api_keys` | Create API key |
| `DELETE` | `/api/v1/api_keys/:id` | Revoke API key |
| `POST` | `/api/v1/tenants/invites` | Create invite (owner/admin) |
| `POST` | `/api/v1/invites/:token/accept` | Accept invite |
| `DELETE` | `/api/v1/tenants/members/:user_id` | Remove member |
| `POST` | `/api/v1/tenants/transfer` | Transfer ownership (owner only) |
| `GET` | `/api/v1/audit_log` | Privileged action ledger (owner/admin) |

### Admin
| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/runs/metrics` | Run + cost rollups (owner/admin) |
| `GET` | `/api/v1/workers/health` | Live worker heartbeats |

### Workflow Runs
| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/workflow_runs` | List runs (tenant-scoped) |
| `GET` | `/api/v1/workflow_runs/:id` | Run detail w/ trace |
| `GET` | `/api/v1/workflow_runs/daily_total` | Daily cost rollup |

### Webhooks
| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/webhooks/:slug` | HMAC-verified webhook trigger |

---

## Roadmap

### Done
- [x] Visual workflow canvas (React Flow)
- [x] 8 node types (trigger, http_request, sandbox_script, ai_agent, for_each, mongo_request, redis_request, notify)
- [x] Multi-language sandbox execution (JS, Python, Go, Rust, PHP)
- [x] Interactive debugging (DAP for Python, CDP for JavaScript)
- [x] HTTP Request node with full Go `http.Client` parity (method, headers, query, body, auth, redirects, TLS, JSON parse)
- [x] Webhook trigger (HMAC SHA-256 signature verification)
- [x] Email + password auth, OAuth (Google + GitHub), API keys, password reset
- [x] Multi-tenancy w/ owner/admin/member roles, invites, ownership transfer
- [x] Audit log (append-only ledger of privileged actions)
- [x] Run metrics dashboard (`/admin`) + worker health heartbeats
- [x] Reaper worker (sweeps stuck running/paused/pending_approval runs)
- [x] Transactional email (SMTP via Mailpit dev sink or any standard relay)
- [x] CI pipeline (vet/build/test -race/lint/typecheck) on push + PR; branch protection on `main`
- [x] gVisor integration (syscall-level isolation via k3s `RuntimeClass=runsc`)
- [x] Kubernetes-API sandbox backend (single-node k3s today; portable to upstream Kubernetes — see `examples/k3s/MIGRATION.md`)
- [x] AI agent node (reason-act-observe loop, Anthropic / OpenAI / Ollama, edge-bound tool nodes, built-in `code_execute`)
- [x] LLM connection types (`anthropic`, `openai`, `ollama`)
- [x] Workflow run persistence (`workflow_runs` collection) + agent trace logging
- [x] Custom sandbox images (`data.custom_image` per node)
- [x] Per-node package list (`data.packages`) for installable runtimes
- [x] Streaming sandbox runs (WebSocket `/api/v1/sandbox/run`, stdout/stderr/output frames)

### In progress / next
- [ ] Container pool warm starts (sub-100ms cold start)
- [ ] Image-layer caching for `packages` to skip per-run installs
- [ ] Multi-node Kubernetes validation (HPA, pod priority, ResourceQuotas)
- [ ] Skills + plugins layer (`.private/ai-automation/SKILLS-AND-PLUGINS-PLAN.md`)
- [ ] Per-tenant isolation (namespaces, RBAC, secret scoping)

---

## Development

```bash
make lint                  # Run golangci-lint
make test                  # Run all tests
make clean                 # Clean build artifacts
make dev-startup           # Start k3s + local registry + docker compose (inverse of dev-teardown)
make dev-startup-fresh     # First-time bring-up: setup.sh + sandbox images + compose
make dev-teardown          # Stop docker compose + k3s + local registry (state preserved)
make dev-teardown-sandbox  # Delete pods in the sandbox namespace (cluster stays up)
make dev-teardown-full     # DESTRUCTIVE: uninstall k3s, remove registry + gVisor
```

---

## License

MIT

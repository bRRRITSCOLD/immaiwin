# immaiwin

**Visual workflow automation with sandboxed multi-language code execution and interactive debugging.**

Think **n8n** meets **Replit** — a canvas-based workflow builder where every node can run real code (JavaScript, Python, Go, Rust, PHP) inside isolated Docker containers with gVisor-grade security. Step through your code with breakpoints. Chain data transformations across languages. Connect to any WebSocket, REST API, or message queue. All self-hosted, all open source.

The long game: add AI agents that can write and execute code as part of their reasoning — like [OpenClaw](https://github.com/openclaw) but inherently safe, because every script runs in a sandboxed container with syscall interception, no network by default, and hard resource limits. The agent can't escape the sandbox.

---

## What Makes This Different

| Feature | n8n | Zapier | Make.com | **immaiwin** |
|---------|-----|--------|----------|-------------|
| Visual workflow builder | Yes | Limited | Yes | **Yes** (React Flow canvas) |
| Code execution | JS only | No | No | **5 languages** (JS, Python, Go, Rust, PHP) |
| Sandboxed isolation | No | N/A | N/A | **Docker + gVisor** (syscall-level) |
| Interactive debugging | No | No | No | **DAP/CDP debugger** (breakpoints, step, inspect) |
| WebSocket triggers | Plugin | No | No | **Native** (OAuth + WS client worker) |
| Custom parsers/scrapers | No | No | No | **JS scripting** (goja runtime, jQuery-like selectors) |
| Self-hosted | Yes | No | No | **Yes** |
| AI agent integration | No | No | No | **Planned** (safe — runs in sandbox) |

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
| `http_fetch` | Make HTTP requests (GET/POST/PUT/DELETE) |
| `js_script` | Quick JavaScript transforms (goja — no container overhead) |
| `sandbox_script` | Run code in isolated Docker container (any of 5 languages) |
| `for_each` | Loop over arrays with isolated context per iteration |
| `mongo_upsert` | Write/update MongoDB documents |
| `redis_publish` | Publish messages to Redis channels |
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
| **gVisor** (planned) | User-space kernel — intercepts all syscalls |
| **Non-root** | Runs as uid 65534 (nobody) |
| **No network** | `--network=none` by default, opt-in per node |
| **Resource limits** | CPU 0.5 cores, 128MB RAM, 256 PIDs, 30s timeout |
| **Read-only code** | User script mounted, no host filesystem access |
| **No secrets** | Only explicit params injected, never host env |

This is what makes AI agent integration safe — an agent can write and execute arbitrary code, but it runs in a sandbox that can't access the host, can't make network calls (unless explicitly allowed), and gets killed after 30 seconds.

### Workflow Triggers

Three ways to kick off workflows:

- **Cron** — schedule workflows on any interval (`workflow-cron` worker)
- **RabbitMQ** — trigger from message queue events (`workflow-rabbitmq` worker)
- **WebSocket** — connect to external WebSocket sources with OAuth, trigger on incoming messages (`workflow-ws-client` worker)

WebSocket triggers support OAuth connections — authenticate with any service, then stream their WebSocket data into your workflow. Preview incoming data with SSE before wiring up the full flow.

### Custom Scraper Scripts

Define per-source parsing logic with JavaScript. Built-in bindings:

- `$(html)` — jQuery-like CSS selectors for HTML parsing
- `parseRSS(xml)` — RSS/Atom feed parser
- `parseDate(str)` — flexible date parsing
- `now()` — current timestamp

Validate scripts before deploying. Fallback to built-in parsers (Bloomberg RSS, AlJazeera, Investing.com) when no custom script is set.

### Real-Time Streaming

All data flows through Redis Pub/Sub and is exposed via Server-Sent Events:

- `/api/v1/trades/stream` — live trade events
- `/api/v1/news/stream` — new articles as they're scraped
- `/api/v1/options/stream` — options activity
- `/api/v1/futures/stream` — futures activity

---

## Tech Stack

| Component | Technology |
|-----------|-----------|
| **API** | Go 1.22+, Gin, gorilla/websocket |
| **Frontend** | React 19, TanStack Start, React Flow, Monaco Editor, Tailwind CSS, shadcn/ui |
| **Database** | MongoDB (documents, configs, workflows) |
| **Cache/PubSub** | Redis (real-time streaming, inter-service messaging) |
| **Message Queue** | RabbitMQ (workflow triggers, event-driven execution) |
| **Containers** | Docker API (sandbox execution, debug sessions) |
| **Sandbox Runtimes** | Node.js 20, Python 3.12, Go 1.22, Rust 1.86, PHP 8.3 |
| **Debug Protocols** | DAP (Python/debugpy), CDP (JS/Node --inspect) |
| **JS Engine** | goja (lightweight in-process JS for quick transforms) |

---

## Prerequisites

- **Go** 1.22+
- **Node.js** 20+ with `pnpm`
- **Docker** (Docker Desktop or Docker Engine)
- **Make**

## Quick Start

### 1. Clone and configure

```bash
git clone https://github.com/bRRRITSCOLD/immaiwin-go.git
cd immaiwin-go
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
# Runtime images (required for sandbox_script nodes)
docker build -t immaiwin/sandbox-node:20 internal/sandbox/runtimes/javascript/
docker build -t immaiwin/sandbox-python:3.12 internal/sandbox/runtimes/python/
docker build -t immaiwin/sandbox-go:1.22 internal/sandbox/runtimes/golang/
docker build -t immaiwin/sandbox-rust:1.86 internal/sandbox/runtimes/rust/
docker build -t immaiwin/sandbox-php:8.3 internal/sandbox/runtimes/php/

# Debug images (required for interactive debugging)
docker build -f internal/sandbox/runtimes/javascript/Dockerfile.debug \
  -t immaiwin/sandbox-node-debug:20 internal/sandbox/runtimes/javascript/
docker build -f internal/sandbox/runtimes/python/Dockerfile.debug \
  -t immaiwin/sandbox-python-debug:3.12 internal/sandbox/runtimes/python/
```

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
make worker NAME=workflow-ws-client            # WebSocket-triggered workflows
make worker NAME=news-scraper                  # News scraper
make worker NAME=mongodb-writer                # Background MongoDB writes
```

---

## Project Structure

```
├── cmd/
│   ├── api/                    # API server entrypoint
│   ├── ui/                     # UI build entrypoint
│   └── worker/                 # Worker entrypoint (registry-based)
├── internal/
│   ├── api/                    # HTTP server, routes, handlers
│   │   └── handler/            # Request handlers (workflows, sandbox, debug, news, etc.)
│   ├── config/                 # Environment configuration
│   ├── mongodb/                # MongoDB repositories
│   ├── news/                   # Scraper configs, parsers, executor
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
│   ├── workflow/               # Workflow engine (executor, types)
│   └── worker/                 # Background worker implementations
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
| `GET` | `/api/v1/workflows/:id/ws-preview` | SSE preview of WebSocket trigger data |

### Sandbox Debugging
| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/sandbox/debug` | WebSocket upgrade for interactive debug session |

### Connections (OAuth/WebSocket Sources)
| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/connections` | List saved connections |
| `PUT` | `/api/v1/connections/:id` | Create/update connection |
| `DELETE` | `/api/v1/connections/:id` | Delete connection |
| `POST` | `/api/v1/connections/test` | Test connection |

### News Scrapers
| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/news/scrapers` | List scraper configs |
| `PATCH` | `/api/v1/news/scrapers/:source` | Update scraper config/script |
| `DELETE` | `/api/v1/news/scrapers/:source/script` | Remove custom parsing script |
| `POST` | `/api/v1/news/scrapers/validate` | Validate JS parsing script |

### Streaming
| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/trades/stream` | SSE — live trade events |
| `GET` | `/api/v1/news/stream` | SSE — new articles |

---

## Roadmap

- [x] Visual workflow canvas (React Flow)
- [x] 8 node types (trigger, http_fetch, js_script, sandbox_script, for_each, mongo_upsert, redis_publish, notify)
- [x] Multi-language sandbox execution (JS, Python, Go, Rust, PHP)
- [x] Interactive debugging (DAP for Python, CDP for JavaScript)
- [x] WebSocket workflow triggers with OAuth
- [x] Custom scraper scripts (goja + jQuery-like selectors)
- [x] Real-time streaming (SSE)
- [ ] gVisor integration (syscall-level isolation)
- [ ] Container pool warm starts (sub-100ms cold start)
- [ ] AI agent integration (safe code execution via sandbox)
- [ ] Package management (per-script deps, image layer caching)
- [ ] Streaming output during execution (SSE for stdout/stderr)
- [ ] Kubernetes migration (horizontal scaling)
- [ ] Custom sandbox images (user-built with pre-installed deps)

---

## Development

```bash
make lint       # Run golangci-lint
make test       # Run all tests
make clean      # Clean build artifacts
```

---

## License

MIT

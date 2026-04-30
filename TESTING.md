# Testing — Coverage Catalog

Living catalog of every test in the repo. Update on every PR that adds or removes a test. Authoring conventions live in [`.claude/rules/TESTING.md`](.claude/rules/TESTING.md).

The naming convention used everywhere is xUnit-style **`TestSubject_Scenario_Expectation`** (Roy Osherove). Each test function carries a one-line doc comment explaining what it verifies — `go doc` and `godoc` surface those automatically, and `go test -v` prints the function name, so the test surface is self-describing without external tooling.

## Layout

```
<package>/<file>_test.go              — unit
<package>/<file>_integration_test.go  — integration (real services, may self-skip)
<package>/<file>_e2e_test.go          — end-to-end (full stack)
```

Run all: `make test` or `go test -race -count=1 ./...`.

The integration suites probe `MONGO_URI` (default `mongodb://localhost:27017`) and `REDIS_URL` (default `redis://localhost:6379`) on startup and self-skip when unreachable, so `go test ./...` stays green outside the compose stack. CI (`.github/workflows/ci.yml`) brings up `docker compose up -d --wait mongodb redis` before `go test`, so the integration suites actually execute there.

---

## Unit tests

Pure-package tests with no external dependencies. Suite pattern (testify) per [TESTING.md](.claude/rules/TESTING.md).

| Package | File | Subject | Scenarios covered |
|---|---|---|---|
| `internal/skills` | `parser_test.go` | Skill manifest parser | malformed YAML, missing required fields, valid round-trip |
| `internal/skills` | `localfs_test.go` | Local-FS skill source | bundle discovery, version sort, missing dir tolerated |
| `internal/skills` | `resolver_test.go` | Skill resolver | registry-vs-source verify, drift detection, cache invalidation |

---

## Integration tests

Real Mongo + Redis (per `.claude/rules/TESTING.md` — no mocks). Each suite uses a unique test database per run and drops on teardown, so concurrent runs don't collide.

| Package | File | Subject | Test functions |
|---|---|---|---|
| `internal/worker` | `registry_integration_test.go` | Heartbeat ticker | `TestHeartbeat_TickerOver1s_AdvancesAndCleanlyStops` (tick_count >= 5 in 1s @ 100ms cadence; `MarkStopped` flips status on clean exit), `TestHeartbeat_NilHealthRepo_RegistryStillUsable` (registry usable when health repo nil) |
| `internal/api/handler` | `workflow_integration_test.go` | Workflow CRUD + tenant isolation | `TestWorkflowsList_NoCookie_Returns401`, `TestWorkflowCRUD_Lifecycle_RoundTripsAllOps` (PUT → list → update → list → DELETE → empty), `TestWorkflows_ForeignTenant_AreInvisibleAnd403OnTakeover` (alice can't read bob's wf; takeover-PUT → 403) |
| `internal/api/handler` | `workflow_run_integration_test.go` | Workflow execution | `TestRun_TriggerOnly_PersistsSuccessRow`, `TestRun_NonexistentWorkflow_404`, `TestRun_Unauth_401`, `TestRunsList_FilteredByWorkflow` (workflow_id query param narrows list) |

### Setup conventions

- Suite struct embeds `suite.Suite` and holds `*mongo.Client`, `*mongo.Database`, `*rediss.Client`, `*httptest.Server`.
- `TestSuiteName(t *testing.T)` probes Mongo, calls `t.Skipf` if unreachable, then `suite.Run(t, ...)`.
- `SetupSuite` connects services + builds repos + mounts `httptest.NewServer(srv.Handler())`.
- `SetupTest` wipes per-test collections AND clears the auth-rate-limit Redis counters (the `5/min` register ceiling otherwise trips after a few tests, since httptest reuses `127.0.0.1` as the client IP).
- `TearDownSuite` drops the test database, closes services.

---

## E2E tests

End-to-end flows against a fully running stack (UI + API + workers + services). None shipped yet — Playwright harness is on the backlog ([issue ref pending]).

Slot reserved here for the future catalog:

| Suite | Subject | Test functions |
|---|---|---|

---

## Coverage gaps (open backlog)

- **Workflow run with executable nodes** — current run suite covers trigger-only. http_request / mongo_request / redis_request / sandbox_script / ai_agent paths uncovered.
- **Webhook trigger HMAC** — `POST /api/v1/webhooks/:slug` signature-verification path has no test.
- **Skill registry CRUD** — install/refresh/uninstall + drift detection at the HTTP boundary.
- **Sandbox WS run** — `/api/v1/sandbox/run` WebSocket path has no automated test.
- **Agent loop end-to-end** — burns LLM tokens; defer to record-and-replay or VCR-style harness.
- **OAuth login** — Google + GitHub are smoke-tested manually; provider-callback path has no integration test.
- **Connection OAuth refresh** — was Schwab-only, removed in PR #38; if a new OAuth provider lands, add a test alongside.

---

## Authoring a new test

1. Pick the right tier (unit / integration / e2e) per the rules in [`.claude/rules/TESTING.md`](.claude/rules/TESTING.md).
2. Name it `TestSubject_Scenario_Expectation`.
3. Add a one-line doc comment explaining what it verifies.
4. **Update the table in this file** in the same PR.

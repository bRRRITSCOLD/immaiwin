# Testing

Below are guidelines around all things testing in this repo. It will layout theory, commands, and more.

## Naming + descriptors (project-wide invariant)

**Every** test function — unit, integration, or e2e — must follow these conventions so the test surface stays self-describing as it grows:

1. **Name format**: `TestSubject_Scenario_Expectation` (xUnit-style per Roy Osherove). The function name alone should make the failing case obvious in a CI log.
2. **One-line doc comment** above each test func: `// TestSubject_Scenario_Expectation verifies that <thing>.` `go doc` and `go test -v` surface it; failing tests in CI then carry the intent without anyone opening the source.
3. **Update the catalog**: every PR that adds, removes, or substantially changes a test must update [`/TESTING.md`](../../TESTING.md) — the root coverage catalog. Failing to update the catalog means future contributors don't know what's covered. (We have no tooling enforcing this; the rule is the enforcement.)

This naming + comment + catalog combo is intentionally low-tech. Heavier tools (cucumber/godog, BDD frameworks, custom runners) were considered and rejected — the catalog gives us "what's covered" at a glance and the test names give us "why it failed" without buying into another DSL.

## Guidlines

### Unit Testing
Unit testing verifies the smallest testable parts of an application—such as a single function, method, or class—in complete isolation.
* All unit tests are suffixed with `_test.go` and lives in the same directory as the file/package it tests
* **Descriptor requirement**: name `TestSubject_Scenario_Expectation` + one-line doc comment + add the suite to [`/TESTING.md`](../../TESTING.md) under the **Unit tests** section in the same PR. Don't ship a test that isn't catalogued.
* All unit tests always include the below, even if they aren't used (`SetupSuite` - runs before all tests in the suite, `SetupTest` - runs before each test in the suite, `TearDownTest` - runs after each test in the suite, `TearDownSuite` - runs after all tests in the suite). We follow this pattern just in case we need them and to keep patterns familiar with developers.
  ```go  
  func (s *UniqueNameOfTestSuite) SetupSuite() {}

  func (s *UniqueNameOfTestSuite) TearDownSuite() {}

  func (s *UniqueNameOfTestSuite) SetupTest() {}

  func (s *UniqueNameOfTestSuite) TearDownTest() {}
  ```

### Integration Testing
Integration testing focuses on the "seams" between components, ensuring that two or more units or services work together correctly. Usin isolated DBs, test APIs or mocked APIs or HTTP Interceptors, etc.
* All integration tests are suffixed with `_integration_test.go` and lives in the same directory as the file/package it tests
* **Build tag required**: every integration test file MUST start with `//go:build integration` followed by a blank line, before the package declaration. This isolates the tier from default `go test ./...`.
* **Descriptor requirement**: name `TestSubject_Scenario_Expectation` + one-line doc comment + add the suite to [`/TESTING.md`](../../TESTING.md) under the **Integration tests** section in the same PR. Don't ship a test that isn't catalogued.
* **No skip path**: integration suites must `t.Fatalf` (NOT `t.Skipf`) when their service deps are unreachable. We do not allow opt-out skipping anywhere — local or CI. A dev who pushes red because compose wasn't up will find out from the next `make test-integration` they run, not from a CI failure days later. Run `make docker-compose-up` before `make test-integration`.
* **How to run**:
  - `make test-unit` — fast, no service deps (default for iteration)
  - `make test-integration` — requires the compose stack up (`make docker-compose-up`)
  - `make test` — every tier in sequence (run before pushing)
  - CI runs unit + integration on every push/PR via `.github/workflows/ci.yml`.
* All integration tests always include the below, even if they aren't used (`SetupSuite` - runs before all tests in the suite, `SetupTest` - runs before each test in the suite, `TearDownTest` - runs after each test in the suite, `TearDownSuite` - runs after all tests in the suite). We follow this pattern just in case we need them and to keep patterns familiar with developers.
  ```go  
  func (s *UniqueNameOfTestSuite) SetupSuite() {}

  func (s *UniqueNameOfTestSuite) TearDownSuite() {}

  func (s *UniqueNameOfTestSuite) SetupTest() {}

  func (s *UniqueNameOfTestSuite) TearDownTest() {}
  ```


### E2E (End-to-End) Testing
E2E (End-to-End) Testing
Validates entire user workflows from start to finish in an environment that mimics production.pre
* All e2e tests are suffixed with `_e2e_test.go` and lives in the same directory as the file/package it tests
* **Build tag required**: every e2e test file MUST start with `//go:build e2e` followed by a blank line, before the package declaration.
* **Descriptor requirement**: name `TestSubject_Scenario_Expectation` + one-line doc comment + add the suite to [`/TESTING.md`](../../TESTING.md) under the **E2E tests** section in the same PR. Don't ship a test that isn't catalogued.
* **No skip path**: same rule as integration — `t.Fatalf` on missing dependencies; never `t.Skipf`. Run via `make test-e2e`.
* All e2e tests always include the below, even if they aren't used (`SetupSuite` - runs before all tests in the suite, `SetupTest` - runs before each test in the suite, `TearDownTest` - runs after each test in the suite, `TearDownSuite` - runs after all tests in the suite). We follow this pattern just in case we need them and to keep patterns familiar with developers.
  ```go  
  func (s *UniqueNameOfTestSuite) SetupSuite() {}

  func (s *UniqueNameOfTestSuite) TearDownSuite() {}

  func (s *UniqueNameOfTestSuite) SetupTest() {}

  func (s *UniqueNameOfTestSuite) TearDownTest() {}
  ```

## Commands

### Tiered targets (preferred)

```bash
make test-unit         # fast, no service deps
make test-integration  # requires `make docker-compose-up` first
make test-e2e          # requires full stack
make test              # every tier in sequence (run before pushing)
```

### Direct go invocations

```bash
go test -race -count=1 ./...                       # unit tier
go test -tags=integration -race -count=1 ./...     # integration tier
go test -tags=e2e -race -count=1 ./...             # e2e tier
```

`scripts/test/main.go` accepts `-tier=unit|integration|e2e|all` and delegates the same way; equivalent to the Makefile targets above.

### Local-only smoke shells (not part of the official test surface)

Integration tests (above) are the canonical coverage that other contributors run. If you write throwaway shell smokes locally for in-progress verification — for example, when iterating on a new endpoint before the matching integration test exists — keep them out of the tracked tree (any gitignored path works).

These do not replace integration / e2e tests and they don't get listed in [`/TESTING.md`](../../TESTING.md). Once the feature is real, the matching integration test (per the rules above) is the deliverable, and the local shell can be deleted.

If you do write one and it spawns a worker via `go run ./cmd/<x> -name <name> &`, note that `kill $PID` only kills the `go run` wrapper; the compiled child binary in `/tmp/go-build*/.../exe/<x>` is re-parented to init and keeps running. Use `pkill -TERM -f "<x> -name <name>"` in the trap cleanup so the child also dies. Without this the child keeps writing heartbeats and downstream sweeps race on stale `worker_health` rows.
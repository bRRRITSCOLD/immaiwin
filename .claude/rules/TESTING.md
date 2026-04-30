# Testing

Below are guidelines around all things testing in this repo. It will layout theory, commands, and more.

## Guidlines

### Unit Testing
Unit testing verifies the smallest testable parts of an application—such as a single function, method, or class—in complete isolation.
* All unit tests are suffixed with `_test.go` and lives in the same directory as the file/package it tests
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
* All e2e tests always include the below, even if they aren't used (`SetupSuite` - runs before all tests in the suite, `SetupTest` - runs before each test in the suite, `TearDownTest` - runs after each test in the suite, `TearDownSuite` - runs after all tests in the suite). We follow this pattern just in case we need them and to keep patterns familiar with developers.
  ```go  
  func (s *UniqueNameOfTestSuite) SetupSuite() {}

  func (s *UniqueNameOfTestSuite) TearDownSuite() {}

  func (s *UniqueNameOfTestSuite) SetupTest() {}

  func (s *UniqueNameOfTestSuite) TearDownTest() {}
  ```

## Commands
### Linux/Unix
```bash
go run ./scripts/test/main.go

# or
make test

# or

go test -v -race -count=1 ./...
```


### Windows
```bash
go run ./scripts/test/main.go

# or
make test

# or
go test -v -count=1 ./...
```

### Smoke shells (`.private/ai-automation/*.sh`)
* If a smoke spawns a background process via `go run ./cmd/<x> -name <name> &`, **`kill $PID` is not enough**. `go run` compiles to `/tmp/go-build*/.../exe/<x>` and execs the child; killing the wrapper PID leaves the child orphaned (re-parented to init, still writing heartbeats / holding ports). Always include a `pkill -TERM -f "<x> -name <name>"` in the trap cleanup so the compiled child also dies.
* Example trap pattern (see `reaper-01-smoke.sh`):
  ```bash
  PID=""
  cleanup() {
    if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
      kill "$PID" 2>/dev/null || true
      wait "$PID" 2>/dev/null || true
    fi
    pkill -TERM -f "<x> -name <name>" 2>/dev/null || true
    # … other resource cleanup …
  }
  trap cleanup EXIT
  ```
* Without this, `/admin` accumulates stale "running" worker_health rows from prior smoke runs and downstream sweeps race on them.
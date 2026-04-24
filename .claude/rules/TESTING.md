# Testing

Below are guidelines around all things testing in this repo. It will layout theory, commands, and more.

## Guidlines

### Unit Testing
Unit testing verifies the smallest testable parts of an application—such as a single function, method, or class—in complete isolation.
* All unit tests are suffixed with `_test.go` and lives in the same directory as the file/package it tests
* All unit tests always include the below, even if they aren't used (`SetupSuite` - runs before all tests in the suite, `SetupTest` - runs before each test in the suite, `TearDownTest` - runs after each test in the suite, `TearDownSuite` - runs after all tests in the suite). We follow this pattern just in case we need them and to keep patterns familiar with developers.
  ```go  
  func (s *DotEnvErrorsTestSuite) SetupSuite() {}

  func (s *DotEnvErrorsTestSuite) TearDownSuite() {}

  func (s *DotEnvErrorsTestSuite) SetupTest() {}

  func (s *DotEnvErrorsTestSuite) TearDownTest() {}
  ```

### Integration Testing
Integration testing focuses on the "seams" between components, ensuring that two or more units or services work together correctly. Usin isolated DBs, test APIs or mocked APIs or HTTP Interceptors, etc.
* All integration tests are suffixed with `_integration_test.go` and lives in the same directory as the file/package it tests
* All integration tests always include the below, even if they aren't used (`SetupSuite` - runs before all tests in the suite, `SetupTest` - runs before each test in the suite, `TearDownTest` - runs after each test in the suite, `TearDownSuite` - runs after all tests in the suite). We follow this pattern just in case we need them and to keep patterns familiar with developers.
  ```go  
  func (s *DotEnvErrorsTestSuite) SetupSuite() {}

  func (s *DotEnvErrorsTestSuite) TearDownSuite() {}

  func (s *DotEnvErrorsTestSuite) SetupTest() {}

  func (s *DotEnvErrorsTestSuite) TearDownTest() {}
  ```


### E2E (End-to-End) Testing
E2E (End-to-End) Testing
Validates entire user workflows from start to finish in an environment that mimics production.pre
* All e2e tests are suffixed with `_e2e_test.go` and lives in the same directory as the file/package it tests
* All e2e tests always include the below, even if they aren't used (`SetupSuite` - runs before all tests in the suite, `SetupTest` - runs before each test in the suite, `TearDownTest` - runs after each test in the suite, `TearDownSuite` - runs after all tests in the suite). We follow this pattern just in case we need them and to keep patterns familiar with developers.
  ```go  
  func (s *DotEnvErrorsTestSuite) SetupSuite() {}

  func (s *DotEnvErrorsTestSuite) TearDownSuite() {}

  func (s *DotEnvErrorsTestSuite) SetupTest() {}

  func (s *DotEnvErrorsTestSuite) TearDownTest() {}
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

go test -v -race -count=1 ./...
```
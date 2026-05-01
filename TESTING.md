# Testing — Coverage Catalog

Living catalog of every test in the repo. Update on every PR that adds or removes a test. Authoring conventions live in [`.claude/rules/TESTING.md`](.claude/rules/TESTING.md).

The naming convention used everywhere is xUnit-style **`TestSubject_Scenario_Expectation`** (Roy Osherove). Each test function carries a one-line doc comment explaining what it verifies — `go doc` and `godoc` surface those automatically, and `go test -v` prints the function name, so the test surface is self-describing without external tooling.

## Layout

```
<package>/<file>_test.go              — unit                (default build)
<package>/<file>_integration_test.go  — //go:build integration
<package>/<file>_e2e_test.go          — //go:build e2e
```

The build-tag gate keeps the tiers strictly separate. `go test ./...` only sees the unit tier — fast, zero service deps. The tagged tiers compile in only when the matching `-tags=...` flag is set.

### Running

```bash
make test-unit         # fast, no deps
make test-integration  # requires `make docker-compose-up` first
make test-e2e          # requires full stack
make test              # every tier in sequence (run before pushing)
```

### No skip path

Integration + e2e suites **must fail loud** when their service deps are unreachable. `t.Skipf` is forbidden here; the rule is `t.Fatalf`. Reason: a silent skip in CI would let regressions ride a green badge, and a silent skip locally would let devs push red and learn about it from CI hours later. Same code path everywhere, no env-var opt-outs.

CI (`.github/workflows/ci.yml`) brings up `docker compose up -d --wait mongodb redis` before running the integration tier — if compose hiccups, the build fails before any test executes.

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
| `internal/api/handler` | `workflow_integration_test.go` | Workflow CRUD + tenant isolation + versioning + duplicate | `TestWorkflowsList_NoCookie_Returns401`, `TestWorkflowCRUD_Lifecycle_RoundTripsAllOps` (PUT → list → update → list → DELETE → empty), `TestWorkflows_ForeignTenant_AreInvisibleAnd403OnTakeover` (alice can't read bob's wf; takeover-PUT → 403), `TestWorkflowVersion_RepeatedSaves_IncrementsServerSide` (`$inc` bumps; client-supplied `version` ignored), `TestWorkflowDuplicate_OwnTenant_CreatesIndependentCopy` ("(copy)" suffix, version=1, audit ledger entry), `TestWorkflowDuplicate_ForeignTenant_Returns404` (cross-tenant duplicate reads as not-found, no probe) |
| `internal/api/handler` | `workflow_run_integration_test.go` | Workflow execution + OOB approval (sessioned, magic-link redeem, channel dispatch) | `TestRun_TriggerOnly_PersistsSuccessRow`, `TestRun_NonexistentWorkflow_404`, `TestRun_Unauth_401`, `TestRunsList_FilteredByWorkflow` (workflow_id query param narrows list), `TestRun_NodeApproval_OOB_PendsThenResumesAfterApprove` (require_node_approval gate → pending_approval → /approval POST resolves), `TestApprovalRedeem_BadToken_Returns401`, `TestApprovalRedeem_RunIDMismatch_Returns401`, `TestApprovalRedeem_HappyPath_ApproveResumesRun` (signed token → /approval/redeem → run resumes; audit ledger), `TestApprovalRedeem_TokenSuperseded_Returns410`, `TestApprovalDispatch_SMTPChannel_SendsApprovalEmailWithMagicLink` (recording email.Sender captures msg + verifies embedded token), `TestApprovalDispatch_SlackChannel_PostsWebhookWithMagicLink` (httptest fake webhook captures POST body), `TestApprovalDispatch_ChannelNone_NoEmailSent` (none-channel short-circuits cleanly) |
| `internal/api/handler` | `auth_integration_test.go` | Auth + multi-tenancy flow | `TestRegister_Login_RoundTrip_Returns200WithCookieAndUser`, `TestLogin_WrongPassword_Returns401`, `TestMe_NoCookie_Returns401`, `TestSwitchTenant_NonMember_Returns403`, `TestLogout_ClearsCookie_SubsequentMeReturns401`, `TestRegister_DuplicateEmail_Returns409` |
| `internal/api/handler` | `webhook_integration_test.go` | Webhook trigger (HMAC SHA-256) | `TestWebhook_NoSecret_JSONBody_Returns202Accepted`, `TestWebhook_ValidSignature_WaitTrue_Returns200`, `TestWebhook_InvalidSignature_Returns401`, `TestWebhook_MissingSignature_WhenSecretConfigured_Returns401`, `TestWebhook_UnknownSlug_Returns404` |
| `internal/api/handler` | `api_keys_integration_test.go` | API keys + Bearer auth | `TestCreate_ReturnsRawKey_OncePopulatedListShowsPrefixOnly`, `TestBearerAuth_ValidKey_ReturnsMe`, `TestBearerAuth_InvalidKey_Returns401`, `TestRevoke_BearerWithRevokedKey_Returns401`, `TestList_NoKeys_ReturnsEmptyArray`, `TestCreate_NoSession_Returns401` |
| `internal/api/handler` | `tenant_integration_test.go` | Tenant invites + members + ownership transfer | `TestInviteFlow_BobAcceptsAlicesInvite_GainsAdminRole`, `TestInvite_ReAcceptSameToken_Returns410`, `TestInvite_NonAdminCaller_Returns403`, `TestOwnershipTransfer_HappyPath_FlipsRolesAndAuditLogs`, `TestOwnershipTransfer_SelfAsTarget_Returns400`, `TestOwnershipTransfer_NonMember_Returns400`, `TestOwnershipTransfer_AdminCaller_Returns403` |
| `internal/api/handler` | `password_reset_integration_test.go` | Password reset flow | `TestPasswordReset_HappyPath_NewPasswordWorks_OldPasswordRejected`, `TestPasswordReset_TokenReuse_Returns401`, `TestPasswordReset_BogusToken_Returns401`, `TestPasswordReset_NonexistentEmail_Returns200_NoEmail`, `TestPasswordReset_ConfirmMissingFields_Returns400` |
| `internal/api/handler` | `workflow_http_node_integration_test.go` | Workflow `http_request` node | `TestHTTPNode_GetWithJSON_DecodesIntoOutput`, `TestHTTPNode_4xx_PropagatesError`, `TestHTTPNode_AcceptAnyStatus_TreatsNon2xxAsSuccess`, `TestHTTPNode_RawBody_NoJSONParseOnFalse`, `TestHTTPNode_PostJSONBody_ServerSeesPayload` |
| `internal/api/handler` | `workflow_mongo_redis_node_integration_test.go` | Workflow `mongo_request` + `redis_request` nodes | `TestMongoNode_InsertOne_PersistsDoc`, `TestMongoNode_CountDocuments_AfterInserts`, `TestMongoNode_UnknownOp_FailsRun`, `TestRedisNode_SetThenGet_RoundTrips`, `TestRedisNode_Incr_PersistsCounter`, `TestRedisNode_UnknownOp_FailsRun` |
| `internal/api/handler` | `skills_integration_test.go` | Skill registry HTTP surface | `TestSkills_FreshDB_ListReturnsEmptyArray`, `TestSkills_RefreshFromBundledDir_ImportsAllManifests`, `TestSkills_RefreshTwice_IsIdempotent`, `TestSkills_NoCookie_ListReturns401`, `TestSkills_NoCookie_RefreshReturns401` |

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

- **Workflow run with executable nodes** — `http_request`, `mongo_request`, `redis_request` covered. `sandbox_script` / `ai_agent` paths still uncovered.
- **Skill registry uninstall + drift detection** — list + refresh covered (`skills_integration_test.go`); uninstall endpoint not yet shipped, drift detection (Verify) only runs as part of refresh today.
- **Sandbox WS run** — `/api/v1/sandbox/run` WebSocket path has no automated test.
- **Agent loop end-to-end** — burns LLM tokens; defer to record-and-replay or VCR-style harness.
- **OAuth login** — Google + GitHub provider-callback paths have no integration test (manual only).
- **Audit log** — append-only ledger; partially covered by ownership-transfer test (verifies write + read of one action), but not the full action surface.

---

## Authoring a new test

1. Pick the right tier (unit / integration / e2e) per the rules in [`.claude/rules/TESTING.md`](.claude/rules/TESTING.md).
2. Name it `TestSubject_Scenario_Expectation`.
3. Add a one-line doc comment explaining what it verifies.
4. **Update the table in this file** in the same PR.

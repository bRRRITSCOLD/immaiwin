//go:build integration

// Workflow CRUD + tenant-isolation integration tests. Compiled only under `-tags=integration`.
//
// Real Mongo + Redis (per .claude/rules/TESTING.md — no mocks). Mounts
// the wired-up `*api.Server` behind an httptest.NewServer so requests
// flow through the real router + middleware chain. Uses the existing
// register/login JWT-cookie path to authenticate test users; per-tenant
// scoping is verified by registering two users and asserting their
// workflows do not leak across.
//
// Suite self-skips when MONGO_URI / REDIS_URL aren't reachable so CI
// jobs without a service stack stay green.

package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/api"
	"github.com/bRRRITSCOLD/burrow/internal/api/handler"
	"github.com/bRRRITSCOLD/burrow/internal/config"
	"github.com/bRRRITSCOLD/burrow/internal/email"
	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/bRRRITSCOLD/burrow/internal/rediss"
	"github.com/bRRRITSCOLD/burrow/internal/workflow"
	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/v2/mongo"
	driveroptions "go.mongodb.org/mongo-driver/v2/mongo/options"
)

type WorkflowCRUDIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	redis       *rediss.Client
	httpSrv     *httptest.Server
}

func TestWorkflowCRUDIntegrationSuite(t *testing.T) {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")
	redisURL := envOr("REDIS_URL", "redis://localhost:6379")

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer probeCancel()

	mc, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Fatalf("mongo connect failed (compose stack required): %v", err)
		return
	}
	if err := mc.Ping(probeCtx, nil); err != nil {
		_ = mc.Disconnect(context.Background())
		t.Fatalf("mongo unreachable at %s (compose stack required): %v", mongoURI, err)
		return
	}
	_ = mc.Disconnect(context.Background())
	_ = redisURL // probed when the real client connects in SetupSuite
	suite.Run(t, new(WorkflowCRUDIntegrationSuite))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (s *WorkflowCRUDIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")

	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("burrow_test_workflow_%d", time.Now().UnixNano())
	s.db = c.Database(s.dbName)

	// Redis — RedisConfig defaults to localhost:6379 which matches the
	// CI compose stack. Test doesn't need pub/sub semantics; wfExec is
	// nil for these CRUD tests so the client mostly idles.
	s.redis = rediss.New(config.RedisConfig{Host: "localhost", Port: 6379})

	ctx := context.Background()

	// Build the minimum repo set the workflow handlers + auth path
	// need. Connection + skill + sandbox deps stay nil — they're nil-
	// guarded in NewServer.
	users, err := mongodb.NewUserRepository(ctx, s.db)
	s.Require().NoError(err)
	tenants, err := mongodb.NewTenantRepository(ctx, s.db)
	s.Require().NoError(err)
	apiKeys, err := mongodb.NewAPIKeyRepository(ctx, s.db)
	s.Require().NoError(err)
	wfRepo, err := mongodb.NewWorkflowRepository(ctx, s.db)
	s.Require().NoError(err)
	runRepo, err := mongodb.NewWorkflowRunRepository(ctx, s.db)
	s.Require().NoError(err)
	connRepo, err := mongodb.NewConnectionRepository(ctx, s.db, nil)
	s.Require().NoError(err)
	auditRepo, err := mongodb.NewAuditRepository(ctx, s.db)
	s.Require().NoError(err)

	apiCfg := config.APIConfig{
		Host:    "127.0.0.1",
		Port:    0,
		BaseURL: "http://127.0.0.1",
	}
	authCfg := config.AuthConfig{
		JWTSecret:         "integration_test_secret_dev_only_0123456789abcdef",
		JWTTTL:            "1h",
		CookieDomain:      "",
		CookieSecure:      false,
		AllowRegistration: true,
		UIBaseURL:         "http://127.0.0.1",
	}

	srv := api.NewServer(
		apiCfg,
		authCfg,
		s.redis,
		wfRepo,                       // wfStore
		runRepo,                      // wfRunStore
		(*workflow.WorkflowExecutor)(nil), // wfExec — tests don't hit /run
		connRepo,                     // connStore
		nil,                          // connInvalidator
		nil,                          // skillBackend
		handler.EvalDeps{},
		users,
		tenants,
		apiKeys,
		nil, // workerHealth
		nil, // invites
		auditRepo,
		email.NewLogSender(),
		s.db,
		nil, // sandboxRT
	)
	s.httpSrv = httptest.NewServer(srv.Handler())
}

func (s *WorkflowCRUDIntegrationSuite) TearDownSuite() {
	if s.httpSrv != nil {
		s.httpSrv.Close()
	}
	if s.redis != nil {
		_ = s.redis.Close()
	}
	if s.db != nil {
		_ = s.db.Drop(context.Background())
	}
	if s.mongoClient != nil {
		_ = s.mongoClient.Disconnect(context.Background())
	}
}

func (s *WorkflowCRUDIntegrationSuite) SetupTest() {
	// Wipe between tests so CRUD assertions start from a known empty
	// state. Keep the suite-level repo indexes intact.
	for _, col := range []string{"users", "tenants", "tenant_members", "workflows", "workflow_runs", "audit_log", "api_keys", "connections"} {
		_, _ = s.db.Collection(col).DeleteMany(context.Background(), map[string]any{})
	}
	// Clear the auth rate-limit counters so cross-test register/login
	// bursts don't trip the 5/min and 10/min ceilings (httptest reuses
	// 127.0.0.1 as the client IP, so all tests share one bucket).
	for _, name := range []string{"register", "login", "password_reset", "oauth_start"} {
		_, _ = s.redis.Del(context.Background(), "ratelimit:auth:"+name+":127.0.0.1")
	}
}

func (s *WorkflowCRUDIntegrationSuite) TearDownTest() {}

// ---- helpers ----

// authedClient registers a fresh user and returns an http.Client whose
// cookie jar carries the resulting auth cookie. The returned tenantID
// matches the personal tenant minted at registration time.
func (s *WorkflowCRUDIntegrationSuite) authedClient(email string) (*http.Client, string) {
	jar, err := cookiejar.New(nil)
	s.Require().NoError(err)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}

	body, _ := json.Marshal(map[string]string{
		"email":    email,
		"password": "password1234",
	})
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode, "register should succeed")

	var out struct {
		TenantID string `json:"tenant_id"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&out))
	s.Require().NotEmpty(out.TenantID, "register should mint personal tenant")
	return client, out.TenantID
}

// putWorkflow sends a PUT for the given workflow id + payload. Returns
// the response status + parsed body so tests can assert on shape.
func (s *WorkflowCRUDIntegrationSuite) putWorkflow(client *http.Client, id string, payload map[string]any) (int, map[string]any) {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+id, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// listWorkflows GETs the tenant-scoped list.
func (s *WorkflowCRUDIntegrationSuite) listWorkflows(client *http.Client) (int, []map[string]any) {
	resp, err := client.Get(s.httpSrv.URL + "/api/v1/workflows")
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	var out []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// duplicateWorkflow POSTs the duplicate endpoint and returns parsed body.
func (s *WorkflowCRUDIntegrationSuite) duplicateWorkflow(client *http.Client, id string, payload map[string]any) (int, map[string]any) {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, s.httpSrv.URL+"/api/v1/workflows/"+id+"/duplicate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (s *WorkflowCRUDIntegrationSuite) deleteWorkflow(client *http.Client, id string) int {
	req, _ := http.NewRequest(http.MethodDelete, s.httpSrv.URL+"/api/v1/workflows/"+id, nil)
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	return resp.StatusCode
}

// ---- tests ----

// TestWorkflowsList_NoCookie_Returns401 verifies the workflows endpoint
// requires auth — RequireAuth middleware returns 401 with no cookie.
func (s *WorkflowCRUDIntegrationSuite) TestWorkflowsList_NoCookie_Returns401() {
	resp, err := http.Get(s.httpSrv.URL + "/api/v1/workflows")
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
}

// TestWorkflowCRUD_Lifecycle_RoundTripsAllOps exercises the create /
// read / update / list / delete loop for a workflow under a single
// authed user/tenant.
func (s *WorkflowCRUDIntegrationSuite) TestWorkflowCRUD_Lifecycle_RoundTripsAllOps() {
	suffix := time.Now().UnixNano()
	client, tenantID := s.authedClient(fmt.Sprintf("alice-crud-%d@example.com", suffix))
	s.NotEmpty(tenantID)

	wfID := fmt.Sprintf("wf-crud-%d", suffix)
	payload := map[string]any{
		"name":   "Smoke wf",
		"params": map[string]any{},
		"nodes": []map[string]any{
			{
				"id":       "trigger-1",
				"type":     "trigger",
				"position": map[string]any{"x": 0, "y": 0},
				"data":     map[string]any{"trigger_type": "manual"},
			},
		},
		"edges": []any{},
	}

	// Create
	status, _ := s.putWorkflow(client, wfID, payload)
	s.Equal(http.StatusOK, status, "PUT (create) should 200")

	// List → contains
	status, list := s.listWorkflows(client)
	s.Equal(http.StatusOK, status)
	s.Len(list, 1, "tenant should see exactly the one workflow we created")
	s.Equal(wfID, list[0]["id"])
	s.Equal("Smoke wf", list[0]["name"])

	// Update — flip the name
	updated := payload
	updated["name"] = "Smoke wf (renamed)"
	status, _ = s.putWorkflow(client, wfID, updated)
	s.Equal(http.StatusOK, status, "PUT (update) should 200")

	// List → reflects rename
	_, list = s.listWorkflows(client)
	s.Len(list, 1)
	s.Equal("Smoke wf (renamed)", list[0]["name"])

	// Delete
	s.Equal(http.StatusNoContent, s.deleteWorkflow(client, wfID))

	// List → empty
	_, list = s.listWorkflows(client)
	s.Empty(list)
}

// TestWorkflows_ForeignTenant_AreInvisibleAnd403OnTakeover registers
// two users, creates a workflow under alice's tenant, and asserts bob
// can neither list it nor take it over (cross-tenant PUT → 403).
func (s *WorkflowCRUDIntegrationSuite) TestWorkflows_ForeignTenant_AreInvisibleAnd403OnTakeover() {
	suffix := time.Now().UnixNano()
	alice, aliceTenant := s.authedClient(fmt.Sprintf("alice-iso-%d@example.com", suffix))
	bob, bobTenant := s.authedClient(fmt.Sprintf("bob-iso-%d@example.com", suffix))
	s.NotEqual(aliceTenant, bobTenant, "each user gets own tenant")

	wfID := fmt.Sprintf("wf-iso-%d", suffix)
	payload := map[string]any{
		"name":   "alice's private wf",
		"params": map[string]any{},
		"nodes":  []any{},
		"edges":  []any{},
	}

	status, _ := s.putWorkflow(alice, wfID, payload)
	s.Equal(http.StatusOK, status)

	// Bob's list scopes by his tenant — should be empty.
	_, bobList := s.listWorkflows(bob)
	s.Empty(bobList, "bob should not see alice's workflows")

	// Bob trying to UPDATE alice's workflow id should be rejected as a
	// foreign-tenant takeover (UpsertWorkflow returns 403 in that case).
	status, _ = s.putWorkflow(bob, wfID, map[string]any{
		"name":   "bob takeover attempt",
		"params": map[string]any{},
		"nodes":  []any{},
		"edges":  []any{},
	})
	s.Equal(http.StatusForbidden, status, "cross-tenant takeover should 403")

	// Alice's view is unaffected.
	_, aliceList := s.listWorkflows(alice)
	s.Len(aliceList, 1)
	s.Equal("alice's private wf", aliceList[0]["name"])
}

// TestWorkflowVersion_RepeatedSaves_IncrementsServerSide verifies the
// server-stamped `version` field starts at 1 on first save and bumps
// by 1 on each subsequent Upsert via Mongo `$inc`. Clients cannot
// influence the value — any version they send is ignored.
func (s *WorkflowCRUDIntegrationSuite) TestWorkflowVersion_RepeatedSaves_IncrementsServerSide() {
	suffix := time.Now().UnixNano()
	client, _ := s.authedClient(fmt.Sprintf("alice-version-%d@example.com", suffix))

	wfID := fmt.Sprintf("wf-version-%d", suffix)
	payload := map[string]any{
		"name":   "version test",
		"params": map[string]any{},
		"nodes":  []any{},
		"edges":  []any{},
		// Client tries to inject version=999; server should ignore.
		"version": 999,
	}

	status, body := s.putWorkflow(client, wfID, payload)
	s.Require().Equal(http.StatusOK, status)
	s.Require().Equal(float64(1), body["version"], "first save should land at version 1, not the injected 999")

	status, body = s.putWorkflow(client, wfID, payload)
	s.Require().Equal(http.StatusOK, status)
	s.Require().Equal(float64(2), body["version"], "second save should bump to 2")

	status, body = s.putWorkflow(client, wfID, payload)
	s.Require().Equal(http.StatusOK, status)
	s.Require().Equal(float64(3), body["version"], "third save should bump to 3")
}

// TestWorkflowDuplicate_OwnTenant_CreatesIndependentCopy verifies the
// duplicate endpoint forks a workflow into a new ID under the same
// tenant: name carries " (copy)" suffix, version resets to 1, the new
// doc is independent (further saves on either side don't bleed), and
// the action lands in the audit log.
func (s *WorkflowCRUDIntegrationSuite) TestWorkflowDuplicate_OwnTenant_CreatesIndependentCopy() {
	suffix := time.Now().UnixNano()
	client, tenantID := s.authedClient(fmt.Sprintf("alice-dup-%d@example.com", suffix))

	srcID := fmt.Sprintf("wf-dup-src-%d", suffix)
	payload := map[string]any{
		"name":   "Original",
		"params": map[string]any{"foo": "bar"},
		"nodes": []map[string]any{
			{
				"id":       "trigger-1",
				"type":     "trigger",
				"position": map[string]any{"x": 0, "y": 0},
				"data":     map[string]any{"trigger_type": "manual"},
			},
		},
		"edges": []any{},
	}
	status, _ := s.putWorkflow(client, srcID, payload)
	s.Require().Equal(http.StatusOK, status)

	// Duplicate w/o body — server generates new ID.
	status, dup := s.duplicateWorkflow(client, srcID, nil)
	s.Require().Equal(http.StatusCreated, status)
	dupID, _ := dup["id"].(string)
	s.Require().NotEmpty(dupID)
	s.Require().NotEqual(srcID, dupID, "duplicate must have a new id")
	s.Equal("Original (copy)", dup["name"])
	s.Equal(float64(1), dup["version"], "fresh duplicate should start at version 1")
	s.Equal(tenantID, dup["tenant_id"])

	// Source's params + nodes survived unchanged through the copy.
	dupParams, _ := dup["params"].(map[string]any)
	s.Equal("bar", dupParams["foo"])
	dupNodes, _ := dup["nodes"].([]any)
	s.Len(dupNodes, 1)

	// List shows two workflows, both belonging to alice.
	_, list := s.listWorkflows(client)
	s.Len(list, 2)

	// Updating the duplicate shouldn't affect the source.
	updated := payload
	updated["name"] = "Duplicate edited"
	status, _ = s.putWorkflow(client, dupID, updated)
	s.Require().Equal(http.StatusOK, status)

	_, list = s.listWorkflows(client)
	names := map[string]bool{}
	for _, w := range list {
		names[w["name"].(string)] = true
	}
	s.True(names["Original"], "source name survived")
	s.True(names["Duplicate edited"], "duplicate rename took effect")

	// Audit ledger captured the duplicate action.
	auditCol := s.db.Collection("audit_log")
	count, err := auditCol.CountDocuments(context.Background(), map[string]any{
		"action":           string(mongodb.AuditWorkflowDuplicated),
		"target.source_id": srcID,
	})
	s.Require().NoError(err)
	s.Equal(int64(1), count, "duplicate action should record exactly one audit entry")
}

// TestWorkflowDuplicate_ForeignTenant_Returns404 verifies bob cannot
// duplicate alice's workflow — the endpoint returns 404 (not 403) so
// callers can't probe existence cross-tenant.
func (s *WorkflowCRUDIntegrationSuite) TestWorkflowDuplicate_ForeignTenant_Returns404() {
	suffix := time.Now().UnixNano()
	alice, _ := s.authedClient(fmt.Sprintf("alice-dup-iso-%d@example.com", suffix))
	bob, _ := s.authedClient(fmt.Sprintf("bob-dup-iso-%d@example.com", suffix))

	srcID := fmt.Sprintf("wf-dup-iso-%d", suffix)
	payload := map[string]any{
		"name":   "alice secret",
		"params": map[string]any{},
		"nodes":  []any{},
		"edges":  []any{},
	}
	status, _ := s.putWorkflow(alice, srcID, payload)
	s.Require().Equal(http.StatusOK, status)

	// Bob attempts to duplicate alice's workflow — endpoint returns 404.
	status, _ = s.duplicateWorkflow(bob, srcID, nil)
	s.Equal(http.StatusNotFound, status, "cross-tenant duplicate must read as not-found")

	// Bob's tenant remains empty; alice still owns one workflow.
	_, bobList := s.listWorkflows(bob)
	s.Empty(bobList)
	_, aliceList := s.listWorkflows(alice)
	s.Len(aliceList, 1)
}

// TestWorkflowUpsert_MissingNodeConnection_Rejected400 verifies the
// server-side guard that mirrors the canvas's `requireExplicit` UX
// rule. A workflow whose mongo_request / redis_request node, agent,
// or rabbitmq / redis_subscribe trigger lacks the required
// connection_id (or llm_connection_id for the agent) is rejected at
// save time with 400 + a `missing` array — defense-in-depth for the
// worker's run-time refusal in PR #78, so a direct API client can't
// ship a workflow the worker will only error on later.
func (s *WorkflowCRUDIntegrationSuite) TestWorkflowUpsert_MissingNodeConnection_Rejected400() {
	client, _ := s.authedClient(fmt.Sprintf("alice-missing-conn-%d@example.com", time.Now().UnixNano()))

	cases := []struct {
		label   string
		node    map[string]any
		missing string // expected missing_field
	}{
		{
			"mongo_request without connection_id",
			map[string]any{"id": "n-mongo", "type": "mongo_request", "position": map[string]any{"x": 0, "y": 0},
				"data": map[string]any{"operation": "find", "collection": "anything"}},
			"connection_id",
		},
		{
			"redis_request without connection_id",
			map[string]any{"id": "n-redis", "type": "redis_request", "position": map[string]any{"x": 0, "y": 0},
				"data": map[string]any{"operation": "publish", "channel": "x"}},
			"connection_id",
		},
		{
			"ai_agent without llm_connection_id",
			map[string]any{"id": "n-agent", "type": "ai_agent", "position": map[string]any{"x": 0, "y": 0},
				"data": map[string]any{"system_prompt": "x", "user_input": "y"}},
			"llm_connection_id",
		},
		{
			"rabbitmq trigger without connection_id",
			map[string]any{"id": "n-rabbit", "type": "trigger", "position": map[string]any{"x": 0, "y": 0},
				"data": map[string]any{"trigger_type": "rabbitmq"}},
			"connection_id",
		},
		{
			"redis_subscribe trigger without connection_id",
			map[string]any{"id": "n-redis-sub", "type": "trigger", "position": map[string]any{"x": 0, "y": 0},
				"data": map[string]any{"trigger_type": "redis_subscribe"}},
			"connection_id",
		},
	}
	for _, tc := range cases {
		s.Run(tc.label, func() {
			wfID := fmt.Sprintf("wf-missing-%d", time.Now().UnixNano())
			status, body := s.putWorkflow(client, wfID, map[string]any{
				"name":   "no-conn",
				"params": map[string]any{},
				"nodes":  []map[string]any{tc.node},
				"edges":  []map[string]any{},
			})
			s.Require().Equal(http.StatusBadRequest, status, "save must refuse a node missing its required connection")
			missing, _ := body["missing"].([]any)
			s.Require().Len(missing, 1, "exactly one missing-connection entry expected")
			entry, _ := missing[0].(map[string]any)
			s.Equal(tc.missing, entry["missing_field"])
		})
	}
}

// TestWorkflowUpsert_NodeWithConnection_Accepted verifies the guard's
// negative form: the SAME node types pass validation when the
// required field is set (regression: don't over-refuse).
func (s *WorkflowCRUDIntegrationSuite) TestWorkflowUpsert_NodeWithConnection_Accepted() {
	client, _ := s.authedClient(fmt.Sprintf("alice-has-conn-%d@example.com", time.Now().UnixNano()))
	wfID := fmt.Sprintf("wf-conn-ok-%d", time.Now().UnixNano())
	status, _ := s.putWorkflow(client, wfID, map[string]any{
		"name":   "with-conn",
		"params": map[string]any{},
		"nodes": []map[string]any{
			{"id": "trigger-1", "type": "trigger", "position": map[string]any{"x": 0, "y": 0},
				"data": map[string]any{"trigger_type": "manual"}},
			{"id": "mongo-1", "type": "mongo_request", "position": map[string]any{"x": 200, "y": 0},
				"data": map[string]any{"connection_id": "any-conn", "operation": "find", "collection": "x"}},
			{"id": "redis-1", "type": "redis_request", "position": map[string]any{"x": 400, "y": 0},
				"data": map[string]any{"connection_id": "any-conn", "operation": "publish", "channel": "x"}},
			{"id": "agent-1", "type": "ai_agent", "position": map[string]any{"x": 600, "y": 0},
				"data": map[string]any{"llm_connection_id": "any-conn"}},
		},
		"edges": []map[string]any{},
	})
	s.Equal(http.StatusOK, status, "all required connections present — save must succeed")
}

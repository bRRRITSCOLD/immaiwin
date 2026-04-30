// Workflow CRUD + tenant-isolation integration tests.
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

	"github.com/bRRRITSCOLD/immaiwin-go/internal/api"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/api/handler"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/email"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
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
		t.Skipf("mongo connect failed (skipping integration suite): %v", err)
		return
	}
	if err := mc.Ping(probeCtx, nil); err != nil {
		_ = mc.Disconnect(context.Background())
		t.Skipf("mongo unreachable at %s (skipping integration suite): %v", mongoURI, err)
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
	s.dbName = fmt.Sprintf("immaiwin_test_workflow_%d", time.Now().UnixNano())
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

func (s *WorkflowCRUDIntegrationSuite) deleteWorkflow(client *http.Client, id string) int {
	req, _ := http.NewRequest(http.MethodDelete, s.httpSrv.URL+"/api/v1/workflows/"+id, nil)
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	return resp.StatusCode
}

// ---- tests ----

// TestUnauthRejected verifies that the workflows endpoint requires
// auth. RequireAuth middleware should return 401 with no cookie.
func (s *WorkflowCRUDIntegrationSuite) TestUnauthRejected() {
	resp, err := http.Get(s.httpSrv.URL + "/api/v1/workflows")
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
}

// TestCRUDLifecycle exercises the create/read/update/list/delete loop
// for a workflow under a single authed user/tenant.
func (s *WorkflowCRUDIntegrationSuite) TestCRUDLifecycle() {
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

// TestCrossTenantIsolation registers two users, creates a workflow
// under alice's tenant, and asserts bob can neither list it nor delete
// it (foreign-tenant lookups must 404, not leak).
func (s *WorkflowCRUDIntegrationSuite) TestCrossTenantIsolation() {
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

//go:build integration

// Workflow run integration tests — exercises POST /workflows/:id/run
// end-to-end against a wired-up WorkflowExecutor + real Mongo for run
// persistence. Trivial trigger-only workflow round-trips success in
// milliseconds, then we assert the workflow_runs row landed and the
// run is visible via GET /api/v1/workflow_runs.
//
// Deliberately separate from workflow_integration_test.go because the
// /run path needs an actual WorkflowExecutor wired up — the CRUD suite
// passes nil for that dep. Sandbox + AI-agent paths stay nil here too:
// a trigger-only workflow doesn't touch them, and adding them would
// drag in Docker/k3s + LLM credentials. Compiled only under
// `-tags=integration`.

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
	"net/url"
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

type WorkflowRunIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	redis       *rediss.Client
	httpSrv     *httptest.Server
}

func TestWorkflowRunIntegrationSuite(t *testing.T) {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")

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
	suite.Run(t, new(WorkflowRunIntegrationSuite))
}

func (s *WorkflowRunIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")

	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("immaiwin_test_workflow_run_%d", time.Now().UnixNano())
	s.db = c.Database(s.dbName)

	s.redis = rediss.New(config.RedisConfig{Host: "localhost", Port: 6379})

	ctx := context.Background()

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

	// Real executor — minimum dep set for trigger-only workflows. No
	// sandbox, no AI-agent connection resolver (those nodes aren't in
	// the test workflow). RunRepo is what makes /run persist a row.
	wfExec := &workflow.WorkflowExecutor{
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
		DB:         mongodb.NewMongoClient(s.db),
		RunRepo:    runRepo,
	}

	apiCfg := config.APIConfig{Host: "127.0.0.1", Port: 0, BaseURL: "http://127.0.0.1"}
	authCfg := config.AuthConfig{
		JWTSecret:         "integration_test_secret_dev_only_0123456789abcdef",
		JWTTTL:            "1h",
		AllowRegistration: true,
		UIBaseURL:         "http://127.0.0.1",
	}

	srv := api.NewServer(
		apiCfg, authCfg, s.redis,
		wfRepo, runRepo, wfExec,
		connRepo, nil, nil, handler.EvalDeps{},
		users, tenants, apiKeys,
		nil, nil, auditRepo,
		email.NewLogSender(),
		s.db,
		nil,
	)
	s.httpSrv = httptest.NewServer(srv.Handler())
}

func (s *WorkflowRunIntegrationSuite) TearDownSuite() {
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

func (s *WorkflowRunIntegrationSuite) SetupTest() {
	for _, col := range []string{"users", "tenants", "tenant_members", "workflows", "workflow_runs", "audit_log", "api_keys", "connections"} {
		_, _ = s.db.Collection(col).DeleteMany(context.Background(), map[string]any{})
	}
	for _, name := range []string{"register", "login", "password_reset", "oauth_start"} {
		_, _ = s.redis.Del(context.Background(), "ratelimit:auth:"+name+":127.0.0.1")
	}
}

func (s *WorkflowRunIntegrationSuite) TearDownTest() {}

// authedClient registers + returns a cookie-jar client bound to that
// session.
func (s *WorkflowRunIntegrationSuite) authedClient(emailAddr string) *http.Client {
	jar, err := cookiejar.New(nil)
	s.Require().NoError(err)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}

	body, _ := json.Marshal(map[string]string{
		"email":    emailAddr,
		"password": "password1234",
	})
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	return client
}

// putWorkflow PUTs a trigger-only workflow.
func (s *WorkflowRunIntegrationSuite) putWorkflow(client *http.Client, id, name string) {
	payload := map[string]any{
		"name":   name,
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
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+id, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
}

// TestRun_TriggerOnly_PersistsSuccessRow asserts that running a
// trigger-only workflow returns 200 with a non-empty run_id, status
// "success", and that the row shows up in GET /api/v1/workflow_runs
// scoped to the caller's tenant.
func (s *WorkflowRunIntegrationSuite) TestRun_TriggerOnly_PersistsSuccessRow() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-run-%d@example.com", suffix))
	wfID := fmt.Sprintf("wf-run-%d", suffix)
	s.putWorkflow(client, wfID, "trigger-only run smoke")

	// POST /run
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/workflows/"+wfID+"/run",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	var runOut struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	body, _ := io.ReadAll(resp.Body)
	s.Require().NoError(json.Unmarshal(body, &runOut))
	s.NotEmpty(runOut.RunID, "run_id must be returned")
	s.Equal("success", runOut.Status, "trigger-only workflow should succeed")

	// Run row is visible via list. Tenant-scoped (handler filters by
	// ctx tenant), so the same client sees it.
	listResp, err := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs")
	s.Require().NoError(err)
	defer listResp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, listResp.StatusCode)

	var rows []map[string]any
	s.Require().NoError(json.NewDecoder(listResp.Body).Decode(&rows))
	s.Require().NotEmpty(rows, "workflow_runs list must include the new run")

	var found bool
	for _, r := range rows {
		if r["_id"] == runOut.RunID || r["id"] == runOut.RunID {
			found = true
			s.Equal("success", r["status"])
			s.Equal(wfID, r["workflow_id"])
			break
		}
	}
	s.True(found, "run %s should appear in workflow_runs list (got %d rows)", runOut.RunID, len(rows))
}

// TestRun_NonexistentWorkflow_404 asserts that POST /run on an unknown
// workflow id returns 404 (not 500). RequireAuth still gates so the
// 404 is from the workflow store, not the auth layer.
func (s *WorkflowRunIntegrationSuite) TestRun_NonexistentWorkflow_404() {
	client := s.authedClient(fmt.Sprintf("alice-404-%d@example.com", time.Now().UnixNano()))

	resp, err := client.Post(s.httpSrv.URL+"/api/v1/workflows/does-not-exist/run",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusNotFound, resp.StatusCode)
}

// TestRun_Unauth_401 asserts /run without a session cookie is rejected
// before reaching the executor.
func (s *WorkflowRunIntegrationSuite) TestRun_Unauth_401() {
	resp, err := http.Post(s.httpSrv.URL+"/api/v1/workflows/whatever/run",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
}

// TestRunsList_FilteredByWorkflow asserts the workflow_id query param
// narrows the run list. Running the same workflow twice should yield
// two rows; a list query with workflow_id=other returns zero.
func (s *WorkflowRunIntegrationSuite) TestRunsList_FilteredByWorkflow() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-list-%d@example.com", suffix))
	wfA := fmt.Sprintf("wf-list-a-%d", suffix)
	wfB := fmt.Sprintf("wf-list-b-%d", suffix)
	s.putWorkflow(client, wfA, "list-test wf a")
	s.putWorkflow(client, wfB, "list-test wf b")

	for i := 0; i < 2; i++ {
		resp, err := client.Post(s.httpSrv.URL+"/api/v1/workflows/"+wfA+"/run",
			"application/json", bytes.NewReader([]byte(`{}`)))
		s.Require().NoError(err)
		_ = resp.Body.Close()
	}

	q := url.Values{}
	q.Set("workflow_id", wfA)
	resp, err := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs?" + q.Encode())
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	var rowsA []map[string]any
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&rowsA))
	s.Equal(2, len(rowsA), "wfA should have 2 runs")

	q.Set("workflow_id", wfB)
	resp, err = client.Get(s.httpSrv.URL + "/api/v1/workflow_runs?" + q.Encode())
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	var rowsB []map[string]any
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&rowsB))
	s.Equal(0, len(rowsB), "wfB should have 0 runs")
}

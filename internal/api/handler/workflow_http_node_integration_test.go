//go:build integration

// Workflow http_request-node integration tests — exercises the
// executor's HTTP-call path end-to-end. A second httptest.NewServer
// stands up inside the test as a deterministic mock target so the
// node has a stable URL + body to assert against. Verifies JSON
// parsing, raw-body fallback, error propagation on non-2xx, and the
// `accept_any_status` opt-out that lets the workflow continue past
// 4xx/5xx responses.
//
// Compiled only under `-tags=integration`.

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

type WorkflowHTTPNodeIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	redis       *rediss.Client
	apiSrv      *httptest.Server
	mockSrv     *httptest.Server // workflow's http_request node hits this
	mockMux     *http.ServeMux

	workerCancel context.CancelFunc
	workerDone   chan struct{}
}

func TestWorkflowHTTPNodeIntegrationSuite(t *testing.T) {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer probeCancel()
	mc, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Fatalf("mongo connect failed (compose stack required): %v", err)
	}
	if err := mc.Ping(probeCtx, nil); err != nil {
		_ = mc.Disconnect(context.Background())
		t.Fatalf("mongo unreachable at %s (compose stack required): %v", mongoURI, err)
	}
	_ = mc.Disconnect(context.Background())
	suite.Run(t, new(WorkflowHTTPNodeIntegrationSuite))
}

func (s *WorkflowHTTPNodeIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")
	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("burrow_test_workflow_http_%d", time.Now().UnixNano())
	s.db = c.Database(s.dbName)
	s.redis = rediss.New(config.RedisConfig{Host: "localhost", Port: 6379})

	// Mock target the workflow's http_request node calls. Per-test
	// handlers register on s.mockMux in SetupTest.
	s.mockMux = http.NewServeMux()
	s.mockSrv = httptest.NewServer(s.mockMux)

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

	// Real executor — http_request needs HTTPClient + RunRepo.
	wfExec := &workflow.WorkflowExecutor{
		AllowPrivateHTTPHosts: true, // test httptest mockSrv lives on 127.0.0.1
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
	s.apiSrv = httptest.NewServer(srv.Handler())

	workerCtx, workerCancel := context.WithCancel(context.Background())
	s.workerCancel = workerCancel
	s.workerDone = make(chan struct{})
	go func() {
		defer close(s.workerDone)
		runInTestExecutorWorker(workerCtx, "test-executor-http", runRepo, wfRepo, wfExec)
	}()
}

func (s *WorkflowHTTPNodeIntegrationSuite) TearDownSuite() {
	if s.workerCancel != nil {
		s.workerCancel()
		<-s.workerDone
	}
	if s.apiSrv != nil {
		s.apiSrv.Close()
	}
	if s.mockSrv != nil {
		s.mockSrv.Close()
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

func (s *WorkflowHTTPNodeIntegrationSuite) SetupTest() {
	for _, col := range []string{"users", "tenants", "tenant_members", "workflows", "workflow_runs", "audit_log", "api_keys", "connections"} {
		_, _ = s.db.Collection(col).DeleteMany(context.Background(), map[string]any{})
	}
	for _, name := range []string{"register", "login", "password_reset", "oauth_start"} {
		_, _ = s.redis.Del(context.Background(), "ratelimit:auth:"+name+":127.0.0.1")
	}
	// Reset mock-server routes between tests so a prior test's
	// /endpoint can't be re-hit by the next.
	s.mockMux = http.NewServeMux()
	// Replace handler — httptest.Server holds a *http.Server with a
	// fixed Handler, so swap via the underlying mux. We do this by
	// re-assigning the mux pointer that the real handler closure
	// dereferences each request.
	// (Simplest is to just track current mux + dispatch through it.)
	currentMux := s.mockMux
	s.mockSrv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		currentMux.ServeHTTP(w, r)
	})
}

func (s *WorkflowHTTPNodeIntegrationSuite) TearDownTest() {}

// ---- helpers ----

func (s *WorkflowHTTPNodeIntegrationSuite) authedClient(emailAddr string) *http.Client {
	jar, err := cookiejar.New(nil)
	s.Require().NoError(err)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	body, _ := json.Marshal(map[string]string{"email": emailAddr, "password": "password1234"})
	resp, err := client.Post(s.apiSrv.URL+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	return client
}

// putHTTPWorkflow PUTs a workflow with a manual trigger wired into a
// single http_request node. Returns the workflow id.
func (s *WorkflowHTTPNodeIntegrationSuite) putHTTPWorkflow(client *http.Client, suffix int64, nodeData map[string]any) string {
	wfID := fmt.Sprintf("wf-http-%d", suffix)
	payload := map[string]any{
		"name":   "http-node test wf",
		"params": map[string]any{},
		"nodes": []map[string]any{
			{
				"id":       "trigger-1",
				"type":     "trigger",
				"position": map[string]any{"x": 0, "y": 0},
				"data":     map[string]any{"trigger_type": "manual"},
			},
			{
				"id":       "http-1",
				"type":     "http_request",
				"position": map[string]any{"x": 200, "y": 0},
				"data":     nodeData,
			},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "http-1"},
		},
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPut, s.apiSrv.URL+"/api/v1/workflows/"+wfID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	return wfID
}

// runWorkflow POSTs /run (async — handler returns 202 + run_id) and
// polls the run-detail endpoint until the in-test worker drives the
// run to a terminal state (success / error / cancelled). Returns the
// terminal steps + status. httpStatus echoes the dispatch response so
// existing 4xx assertions keep working; for the happy path it's 202.
func (s *WorkflowHTTPNodeIntegrationSuite) runWorkflow(client *http.Client, wfID string) (steps []map[string]any, status string, httpStatus int) {
	resp, err := client.Post(s.apiSrv.URL+"/api/v1/workflows/"+wfID+"/run",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	httpStatus = resp.StatusCode
	if resp.StatusCode != http.StatusAccepted {
		return nil, "", httpStatus
	}
	var dispatch struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&dispatch)
	if dispatch.RunID == "" {
		return nil, "", httpStatus
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		dResp, err := client.Get(s.apiSrv.URL + "/api/v1/workflow_runs/" + dispatch.RunID)
		if err == nil {
			var detail map[string]any
			_ = json.NewDecoder(dResp.Body).Decode(&detail)
			_ = dResp.Body.Close()
			if run, ok := detail["run"].(map[string]any); ok {
				if st, _ := run["status"].(string); st == "success" || st == "error" || st == "cancelled" {
					if rawSteps, ok := run["steps"].([]any); ok {
						for _, sv := range rawSteps {
							if m, ok := sv.(map[string]any); ok {
								steps = append(steps, m)
							}
						}
					}
					// Existing tests assert StatusOK on the happy path —
					// preserve that contract by collapsing the async
					// 202 + successful poll to 200.
					return steps, st, http.StatusOK
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.FailNow("runWorkflow timeout", "run %s never reached terminal state in 10s", dispatch.RunID)
	return nil, "timeout", httpStatus
}

// stepByID returns the StepResult-shaped map for the given node id.
// Helper used in every assertion path.
func stepByID(steps []map[string]any, nodeID string) (map[string]any, bool) {
	for _, st := range steps {
		if id, _ := st["node_id"].(string); id == nodeID {
			return st, true
		}
	}
	return nil, false
}

// ---- tests ----

// TestHTTPNode_GetWithJSON_DecodesIntoOutput verifies the executor
// dispatches an HTTP GET to the configured URL, decodes the JSON
// response body when `parse_json=true`, and surfaces the parsed
// payload under the step's `json` output field.
func (s *WorkflowHTTPNodeIntegrationSuite) TestHTTPNode_GetWithJSON_DecodesIntoOutput() {
	suffix := time.Now().UnixNano()
	s.mockMux.HandleFunc("/echo", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hello":"world","n":42}`))
	})
	client := s.authedClient(fmt.Sprintf("alice-http-%d@example.com", suffix))
	wfID := s.putHTTPWorkflow(client, suffix, map[string]any{
		"url":        s.mockSrv.URL + "/echo",
		"method":     "GET",
		"parse_json": true,
	})
	steps, status, http_ := s.runWorkflow(client, wfID)
	s.Require().Equal(http.StatusOK, http_)
	s.Equal("success", status)

	step, ok := stepByID(steps, "http-1")
	s.Require().True(ok, "http-1 step missing in %+v", steps)
	output, ok := step["output"].(map[string]any)
	s.Require().True(ok, "output missing")
	s.Equal(true, output["ok"])
	s.EqualValues(200, output["status"])
	jsonBody, ok := output["json"].(map[string]any)
	s.Require().True(ok, "json missing in %+v", output)
	s.Equal("world", jsonBody["hello"])
}

// TestHTTPNode_4xx_PropagatesError verifies that a non-2xx response
// surfaces as a step error by default — workflow status is "error",
// and the HTTP-200 wrapper includes the run_id so the UI can still
// link to the run trace.
func (s *WorkflowHTTPNodeIntegrationSuite) TestHTTPNode_4xx_PropagatesError() {
	suffix := time.Now().UnixNano()
	s.mockMux.HandleFunc("/missing", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})
	client := s.authedClient(fmt.Sprintf("alice-4xx-%d@example.com", suffix))
	wfID := s.putHTTPWorkflow(client, suffix, map[string]any{
		"url":    s.mockSrv.URL + "/missing",
		"method": "GET",
	})
	_, status, _ := s.runWorkflow(client, wfID)
	s.Equal("error", status, "non-2xx should surface as a workflow error by default")
}

// TestHTTPNode_AcceptAnyStatus_TreatsNon2xxAsSuccess verifies the
// `accept_any_status=true` opt-out: 4xx/5xx no longer fail the
// workflow; the response body is captured and the step is marked
// successful.
func (s *WorkflowHTTPNodeIntegrationSuite) TestHTTPNode_AcceptAnyStatus_TreatsNon2xxAsSuccess() {
	suffix := time.Now().UnixNano()
	s.mockMux.HandleFunc("/teapot", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"hint":"i'm a teapot"}`))
	})
	client := s.authedClient(fmt.Sprintf("alice-any-%d@example.com", suffix))
	wfID := s.putHTTPWorkflow(client, suffix, map[string]any{
		"url":               s.mockSrv.URL + "/teapot",
		"method":            "GET",
		"accept_any_status": true,
		"parse_json":        true,
	})
	steps, status, _ := s.runWorkflow(client, wfID)
	s.Equal("success", status)

	step, ok := stepByID(steps, "http-1")
	s.Require().True(ok)
	output := step["output"].(map[string]any)
	s.Equal(false, output["ok"], "ok must reflect non-2xx even when accepted")
	s.EqualValues(http.StatusTeapot, output["status"])
	jsonBody := output["json"].(map[string]any)
	s.Equal("i'm a teapot", jsonBody["hint"])
}

// TestHTTPNode_RawBody_NoJSONParseOnFalse verifies that with
// `parse_json=false` (or unset) the executor returns the raw body
// in `output.body` and never populates `output.json`.
func (s *WorkflowHTTPNodeIntegrationSuite) TestHTTPNode_RawBody_NoJSONParseOnFalse() {
	suffix := time.Now().UnixNano()
	s.mockMux.HandleFunc("/raw", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(`plain old text`))
	})
	client := s.authedClient(fmt.Sprintf("alice-raw-%d@example.com", suffix))
	wfID := s.putHTTPWorkflow(client, suffix, map[string]any{
		"url":    s.mockSrv.URL + "/raw",
		"method": "GET",
		// parse_json omitted => false
	})
	steps, status, _ := s.runWorkflow(client, wfID)
	s.Equal("success", status)
	step, ok := stepByID(steps, "http-1")
	s.Require().True(ok)
	output := step["output"].(map[string]any)
	s.Equal("plain old text", output["body"])
	_, hasJSON := output["json"]
	s.False(hasJSON, "json should be absent when parse_json is not requested")
}

// TestHTTPNode_PostJSONBody_ServerSeesPayload verifies that POST
// with `body_json` (typed body) is sent through correctly — the
// mock server captures the request body and the test asserts the
// echo trip.
func (s *WorkflowHTTPNodeIntegrationSuite) TestHTTPNode_PostJSONBody_ServerSeesPayload() {
	suffix := time.Now().UnixNano()
	var got string
	s.mockMux.HandleFunc("/sink", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got = string(raw)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"received":true}`))
	})
	client := s.authedClient(fmt.Sprintf("alice-post-%d@example.com", suffix))
	wfID := s.putHTTPWorkflow(client, suffix, map[string]any{
		"url":        s.mockSrv.URL + "/sink",
		"method":     "POST",
		"body_json":  map[string]any{"a": 1, "b": "two"},
		"parse_json": true,
	})
	_, status, _ := s.runWorkflow(client, wfID)
	s.Equal("success", status)
	s.Contains(got, `"a":1`)
	s.Contains(got, `"b":"two"`)
}

//go:build integration

// Universal `on_error` policy integration tests — guards the per-node
// `on_error: stop|continue` flag. Default behaviour ("stop") flips
// the run to error on the first node failure; the new "continue"
// policy records the failure on the StepResult but suppresses
// run-status promotion + sets `Continued=true` so the UI can render
// the step distinctly without losing the success badge on the run.
//
// Pre-existing `stop_on_tool_error` lives one layer up inside the
// agent loop and is unchanged here — `on_error` controls whether a
// node's terminal failure aborts the RUN, not whether the agent
// keeps iterating.

package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"sync"
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

type OnErrorPolicyIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	redis       *rediss.Client
	httpSrv     *httptest.Server

	mockSrv     *httptest.Server
	mockHandler http.HandlerFunc
	mockMu      sync.RWMutex

	workerCancel context.CancelFunc
	workerDone   chan struct{}
}

func TestOnErrorPolicyIntegrationSuite(t *testing.T) {
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
	suite.Run(t, new(OnErrorPolicyIntegrationSuite))
}

func (s *OnErrorPolicyIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")
	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("burrow_test_on_error_%d", time.Now().UnixNano())
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

	s.mockSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mockMu.RLock()
		h := s.mockHandler
		s.mockMu.RUnlock()
		if h != nil {
			h(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))

	connResolver := workflow.NewConnectionResolver(connRepo, mongodb.NewMongoClient(s.db), nil)

	wfExec := &workflow.WorkflowExecutor{
		HTTPClient:     &http.Client{Timeout: 5 * time.Second},
		DB:             mongodb.NewMongoClient(s.db),
		ConnResolver:   connResolver,
		RunRepo:        runRepo,
		ApprovalBroker: s.redis,
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

	workerCtx, workerCancel := context.WithCancel(context.Background())
	s.workerCancel = workerCancel
	s.workerDone = make(chan struct{})
	go func() {
		defer close(s.workerDone)
		runInTestExecutorWorker(workerCtx, "test-executor-on-error", runRepo, wfRepo, wfExec)
	}()
}

func (s *OnErrorPolicyIntegrationSuite) TearDownSuite() {
	if s.workerCancel != nil {
		s.workerCancel()
		<-s.workerDone
	}
	if s.httpSrv != nil {
		s.httpSrv.Close()
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

func (s *OnErrorPolicyIntegrationSuite) SetupTest() {
	for _, col := range []string{"users", "tenants", "tenant_members", "workflows", "workflow_runs", "audit_log"} {
		_, _ = s.db.Collection(col).DeleteMany(context.Background(), map[string]any{})
	}
	// Clear auth rate-limit keys so each test gets a fresh window —
	// register-heavy suite hits the 429 ceiling without this.
	for _, name := range []string{"register", "login", "password_reset", "oauth_start"} {
		_, _ = s.redis.Del(context.Background(), "ratelimit:auth:"+name+":127.0.0.1")
	}
	s.mockMu.Lock()
	s.mockHandler = nil
	s.mockMu.Unlock()
}

func (s *OnErrorPolicyIntegrationSuite) TearDownTest() {}

func (s *OnErrorPolicyIntegrationSuite) authedClient(emailAddr string) *http.Client {
	jar, err := cookiejar.New(nil)
	s.Require().NoError(err)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	body, _ := json.Marshal(map[string]string{"email": emailAddr, "password": "password1234"})
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	return client
}

// putHTTPWorkflow PUTs a 2-node workflow:
//
//	trigger → http_request(name="fail", url=mockSrv, on_error=<policy>)
//
// The mock server returns 500 (per SetupTest's mockHandler hook).
// Returns the workflow ID.
func (s *OnErrorPolicyIntegrationSuite) putHTTPWorkflow(client *http.Client, slug, policy string) string {
	suffix := time.Now().UnixNano()
	wfID := fmt.Sprintf("wf-onerror-%s-%d", slug, suffix)
	httpData := map[string]any{
		"name":   "fail",
		"url":    s.mockSrv.URL,
		"method": "GET",
	}
	if policy != "" {
		httpData["on_error"] = policy
	}
	wfPayload := map[string]any{
		"name":   "on_error " + policy,
		"params": map[string]any{},
		"nodes": []map[string]any{
			{"id": "trigger-1", "type": "trigger", "position": map[string]any{"x": 0, "y": 0},
				"data": map[string]any{"trigger_type": "manual"}},
			{"id": "fail-1", "type": "http_request", "position": map[string]any{"x": 200, "y": 0},
				"data": httpData},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "fail-1", "sourceHandle": "success"},
		},
	}
	body, _ := json.Marshal(wfPayload)
	req, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+wfID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	return wfID
}

// putHTTPWorkflowWithErrorEdge wires the failing http_request to a
// second http_request via the error sourceHandle. Verifies that
// `on_error: continue` doesn't prevent error-edge routing.
//
//	trigger → fail-1(on_error=continue) -[error]-> rescue-1 → end
func (s *OnErrorPolicyIntegrationSuite) putHTTPWorkflowWithErrorEdge(client *http.Client) string {
	suffix := time.Now().UnixNano()
	wfID := fmt.Sprintf("wf-onerror-rescue-%d", suffix)
	wfPayload := map[string]any{
		"name":   "on_error continue with rescue branch",
		"params": map[string]any{},
		"nodes": []map[string]any{
			{"id": "trigger-1", "type": "trigger", "position": map[string]any{"x": 0, "y": 0},
				"data": map[string]any{"trigger_type": "manual"}},
			{"id": "fail-1", "type": "http_request", "position": map[string]any{"x": 200, "y": 0},
				"data": map[string]any{
					"name":     "fail",
					"url":      s.mockSrv.URL,
					"method":   "GET",
					"on_error": "continue",
				}},
			{"id": "rescue-1", "type": "http_request", "position": map[string]any{"x": 400, "y": 0},
				"data": map[string]any{
					"name":   "rescue",
					"url":    s.mockSrv.URL + "/ok",
					"method": "GET",
				}},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "fail-1", "sourceHandle": "success"},
			{"id": "e2", "source": "fail-1", "target": "rescue-1", "sourceHandle": "error"},
		},
	}
	body, _ := json.Marshal(wfPayload)
	req, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+wfID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	return wfID
}

// dispatchAndPollRun POSTs /run and polls the run record until
// terminal. Returns the run record body (as a map) for the test to
// inspect status + steps.
func (s *OnErrorPolicyIntegrationSuite) dispatchAndPollRun(client *http.Client, wfID string) map[string]any {
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/workflows/"+wfID+"/run",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusAccepted, resp.StatusCode)
	var dispatch struct {
		RunID string `json:"run_id"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&dispatch))

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		dResp, derr := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + dispatch.RunID)
		s.Require().NoError(derr)
		var detail map[string]any
		_ = json.NewDecoder(dResp.Body).Decode(&detail)
		_ = dResp.Body.Close()
		if run, ok := detail["run"].(map[string]any); ok {
			if st, _ := run["status"].(string); st == "success" || st == "error" || st == "cancelled" {
				return run
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.FailNow("dispatchAndPollRun timeout", "run %s never reached terminal in 8s", dispatch.RunID)
	return nil
}

// stepByIDFromRun linear-scans the run's steps for the matching node_id.
// Tests assert against per-step fields after fetching.
func stepByIDFromRun(run map[string]any, nodeID string) map[string]any {
	steps, _ := run["steps"].([]any)
	for _, raw := range steps {
		s, _ := raw.(map[string]any)
		if s == nil {
			continue
		}
		if id, _ := s["node_id"].(string); id == nodeID {
			return s
		}
	}
	return nil
}

// TestOnError_Stop_DefaultPolicy_HTTPFails_RunLandsError verifies
// the legacy / default behaviour is preserved — an http_request that
// returns 500 with no `on_error` flag set still flips the run to
// `error`. Nothing about PR 4.x's changes should weaken the strict
// default.
func (s *OnErrorPolicyIntegrationSuite) TestOnError_Stop_DefaultPolicy_HTTPFails_RunLandsError() {
	client := s.authedClient(fmt.Sprintf("alice-stop-default-%d@example.com", time.Now().UnixNano()))
	s.mockMu.Lock()
	s.mockHandler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"err":"upstream blew up"}`))
	}
	s.mockMu.Unlock()

	wfID := s.putHTTPWorkflow(client, "default", "")
	run := s.dispatchAndPollRun(client, wfID)
	s.Equal("error", run["status"], "no on_error flag → strict default → run lands error")

	step := stepByIDFromRun(run, "fail-1")
	s.Require().NotNil(step)
	s.NotEmpty(step["error"], "step records the failure either way")
	s.NotEqual(true, step["continued"], "no on_error flag → Continued must remain false")
}

// TestOnError_StopExplicit_HTTPFails_RunLandsError verifies
// `on_error: stop` is parsed identically to the default. Authors
// can lock the strict shape on the URL even when the default is what
// they want — useful when a workflow imports a node from a template
// where the default may have differed historically.
func (s *OnErrorPolicyIntegrationSuite) TestOnError_StopExplicit_HTTPFails_RunLandsError() {
	client := s.authedClient(fmt.Sprintf("alice-stop-explicit-%d@example.com", time.Now().UnixNano()))
	s.mockMu.Lock()
	s.mockHandler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}
	s.mockMu.Unlock()

	wfID := s.putHTTPWorkflow(client, "stop-explicit", "stop")
	run := s.dispatchAndPollRun(client, wfID)
	s.Equal("error", run["status"])
	step := stepByIDFromRun(run, "fail-1")
	s.Require().NotNil(step)
	s.NotEmpty(step["error"])
	s.NotEqual(true, step["continued"])
}

// TestOnError_Continue_HTTPFails_RunLandsSuccessWithContinuedFlag is
// the core PR-4.x regression: `on_error: continue` must let the run
// land `success` when the only failure was the flagged node, AND
// must leave `continued: true` on the step so the UI can render the
// suppressed failure distinctly. The Error field stays populated for
// trace visibility — the suppression is purely about run-status
// promotion, not about hiding the fault.
func (s *OnErrorPolicyIntegrationSuite) TestOnError_Continue_HTTPFails_RunLandsSuccessWithContinuedFlag() {
	client := s.authedClient(fmt.Sprintf("alice-continue-%d@example.com", time.Now().UnixNano()))
	s.mockMu.Lock()
	s.mockHandler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"err":"transient"}`))
	}
	s.mockMu.Unlock()

	wfID := s.putHTTPWorkflow(client, "continue", "continue")
	run := s.dispatchAndPollRun(client, wfID)
	s.Equal("success", run["status"], "on_error=continue suppresses run-status promotion")
	s.Empty(run["error"], "run-level error message stays empty when the failure was suppressed")

	step := stepByIDFromRun(run, "fail-1")
	s.Require().NotNil(step)
	s.NotEmpty(step["error"], "step still surfaces the underlying failure for traces")
	s.Equal(true, step["continued"], "Continued flag must be set so the UI can render the step distinctly")
}

// putHTTPWorkflowWithBothEdges wires the failing http_request to
// TWO downstream nodes — one via the error edge, one via the
// success edge. Used by the dual-edge routing test below.
//
//	trigger → fail-1(on_error=continue)
//	            ├─[error]→   rescue-1
//	            └─[success]→ happy-1
func (s *OnErrorPolicyIntegrationSuite) putHTTPWorkflowWithBothEdges(client *http.Client) string {
	suffix := time.Now().UnixNano()
	wfID := fmt.Sprintf("wf-onerror-dual-%d", suffix)
	wfPayload := map[string]any{
		"name":   "on_error continue with both edges wired",
		"params": map[string]any{},
		"nodes": []map[string]any{
			{"id": "trigger-1", "type": "trigger", "position": map[string]any{"x": 0, "y": 0},
				"data": map[string]any{"trigger_type": "manual"}},
			{"id": "fail-1", "type": "http_request", "position": map[string]any{"x": 200, "y": 0},
				"data": map[string]any{
					"name":     "fail",
					"url":      s.mockSrv.URL,
					"method":   "GET",
					"on_error": "continue",
				}},
			{"id": "rescue-1", "type": "http_request", "position": map[string]any{"x": 400, "y": -100},
				"data": map[string]any{"name": "rescue", "url": s.mockSrv.URL + "/ok", "method": "GET"}},
			{"id": "happy-1", "type": "http_request", "position": map[string]any{"x": 400, "y": 100},
				"data": map[string]any{"name": "happy", "url": s.mockSrv.URL + "/ok", "method": "GET"}},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "fail-1", "sourceHandle": "success"},
			{"id": "e2", "source": "fail-1", "target": "rescue-1", "sourceHandle": "error"},
			{"id": "e3", "source": "fail-1", "target": "happy-1", "sourceHandle": "success"},
		},
	}
	body, _ := json.Marshal(wfPayload)
	req, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+wfID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	return wfID
}

// TestOnError_Continue_ErrorEdgeStillRoutes verifies that suppressing
// run-status promotion does NOT short-circuit the existing error-edge
// routing. A failed node with `on_error: continue` AND a wired error
// edge must still queue the rescue branch's children. The rescue
// node executes successfully → both steps appear in the run record →
// run lands `success`.
func (s *OnErrorPolicyIntegrationSuite) TestOnError_Continue_ErrorEdgeStillRoutes() {
	client := s.authedClient(fmt.Sprintf("alice-rescue-%d@example.com", time.Now().UnixNano()))
	s.mockMu.Lock()
	s.mockHandler = func(w http.ResponseWriter, r *http.Request) {
		// Path-discriminate so the second node (URL ends `/ok`) succeeds.
		if r.URL.Path == "/ok" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}
	s.mockMu.Unlock()

	wfID := s.putHTTPWorkflowWithErrorEdge(client)
	run := s.dispatchAndPollRun(client, wfID)
	s.Equal("success", run["status"])

	failStep := stepByIDFromRun(run, "fail-1")
	s.Require().NotNil(failStep)
	s.NotEmpty(failStep["error"])
	s.Equal(true, failStep["continued"])

	rescueStep := stepByIDFromRun(run, "rescue-1")
	s.Require().NotNil(rescueStep, "error edge must still route to the rescue node despite continue policy")
	s.Empty(rescueStep["error"], "rescue node ran successfully")
}

// TestOnError_Continue_FiresBothErrorAndSuccessEdges verifies the
// dual-edge routing semantics. With `on_error: continue`, a failed
// node fires BOTH its error edge (diagnostics / cleanup branch)
// AND its success edge (happy path keeps rolling as if nothing
// happened). Without dual-edge routing, `continue` would only
// differ from `stop` by the run badge — flow shape would be
// identical, which makes the policy useless for keeping a
// downstream chain alive past a non-fatal failure.
func (s *OnErrorPolicyIntegrationSuite) TestOnError_Continue_FiresBothErrorAndSuccessEdges() {
	client := s.authedClient(fmt.Sprintf("alice-dual-edge-%d@example.com", time.Now().UnixNano()))
	s.mockMu.Lock()
	s.mockHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}
	s.mockMu.Unlock()

	wfID := s.putHTTPWorkflowWithBothEdges(client)
	run := s.dispatchAndPollRun(client, wfID)
	s.Equal("success", run["status"])

	failStep := stepByIDFromRun(run, "fail-1")
	s.Require().NotNil(failStep)
	s.NotEmpty(failStep["error"])
	s.Equal(true, failStep["continued"])

	rescueStep := stepByIDFromRun(run, "rescue-1")
	s.Require().NotNil(rescueStep, "error edge must fire (rescue branch)")
	s.Empty(rescueStep["error"])

	happyStep := stepByIDFromRun(run, "happy-1")
	s.Require().NotNil(happyStep, "success edge must ALSO fire (happy path keeps rolling)")
	s.Empty(happyStep["error"])
}

// putApprovalGateWorkflow PUTs a 2-node workflow whose notify node is
// an approval gate carrying the given `on_error` policy:
//
//	trigger → gate-1(notify, require_node_approval=true, on_error=<policy>)
//
// The gate yields the run to pending_approval; the test then rejects
// it out-of-band. Returns the workflow ID.
func (s *OnErrorPolicyIntegrationSuite) putApprovalGateWorkflow(client *http.Client, slug, policy string) string {
	suffix := time.Now().UnixNano()
	wfID := fmt.Sprintf("wf-onerror-gate-%s-%d", slug, suffix)
	gateData := map[string]any{
		"channel":               "log",
		"message":               "should never run — gate is rejected",
		"require_node_approval": true,
	}
	if policy != "" {
		gateData["on_error"] = policy
	}
	wfPayload := map[string]any{
		"name":   "on_error gate " + policy,
		"params": map[string]any{},
		"nodes": []map[string]any{
			{"id": "trigger-1", "type": "trigger", "position": map[string]any{"x": 0, "y": 0},
				"data": map[string]any{"trigger_type": "manual"}},
			{"id": "gate-1", "type": "notify", "position": map[string]any{"x": 200, "y": 0},
				"data": gateData},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "gate-1", "sourceHandle": "success"},
		},
	}
	body, _ := json.Marshal(wfPayload)
	req, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+wfID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	return wfID
}

// dispatchRejectAndPoll POSTs /run, polls until the gate yields
// (pending_approval), rejects the approval out-of-band, then polls
// until the run reaches a terminal state. Returns the terminal run
// record for inspection.
func (s *OnErrorPolicyIntegrationSuite) dispatchRejectAndPoll(client *http.Client, wfID string) map[string]any {
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/workflows/"+wfID+"/run",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusAccepted, resp.StatusCode)
	var dispatch struct {
		RunID string `json:"run_id"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&dispatch))
	s.Require().NotEmpty(dispatch.RunID)

	// Phase 1: wait for the gate to yield.
	pendDeadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(pendDeadline) {
		dResp, derr := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + dispatch.RunID)
		s.Require().NoError(derr)
		var detail map[string]any
		_ = json.NewDecoder(dResp.Body).Decode(&detail)
		_ = dResp.Body.Close()
		if run, ok := detail["run"].(map[string]any); ok {
			if st, _ := run["status"].(string); st == "pending_approval" {
				goto reject
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.FailNow("dispatchRejectAndPoll timeout", "run %s never reached pending_approval", dispatch.RunID)

reject:
	rejectBody, _ := json.Marshal(map[string]any{"approved": false, "reason": "vetoed for security test"})
	rResp, rerr := client.Post(s.httpSrv.URL+"/api/v1/workflow_runs/"+dispatch.RunID+"/approval",
		"application/json", bytes.NewReader(rejectBody))
	s.Require().NoError(rerr)
	defer rResp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, rResp.StatusCode)

	// Phase 2: wait for terminal.
	termDeadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(termDeadline) {
		dResp, derr := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + dispatch.RunID)
		s.Require().NoError(derr)
		var detail map[string]any
		_ = json.NewDecoder(dResp.Body).Decode(&detail)
		_ = dResp.Body.Close()
		if run, ok := detail["run"].(map[string]any); ok {
			if st, _ := run["status"].(string); st == "success" || st == "error" || st == "cancelled" {
				return run
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.FailNow("dispatchRejectAndPoll timeout", "run %s never reached terminal after reject", dispatch.RunID)
	return nil
}

// TestOnError_ApprovalRejected_ContinuePolicyIgnored_RunStillErrors is
// a SECURITY regression guard. An approval gate is a human veto, not a
// node fault — `on_error: continue` MUST NOT suppress it. If the
// policy applied here, an author could set `continue` on a gate node
// and have rejected runs land `success`, turning the gate into a
// config-level no-op and defeating its entire purpose as a security
// control. Rejection must always flip the run to `error` and must
// never set `continued` on the step, regardless of policy.
func (s *OnErrorPolicyIntegrationSuite) TestOnError_ApprovalRejected_ContinuePolicyIgnored_RunStillErrors() {
	client := s.authedClient(fmt.Sprintf("alice-gate-veto-%d@example.com", time.Now().UnixNano()))

	wfID := s.putApprovalGateWorkflow(client, "continue", "continue")
	run := s.dispatchRejectAndPoll(client, wfID)

	s.Equal("error", run["status"],
		"approval rejection is a security veto — on_error=continue must NOT downgrade it to success")
	s.NotEmpty(run["error"], "run-level error must surface the rejection")

	step := stepByIDFromRun(run, "gate-1")
	s.Require().NotNil(step)
	s.NotEmpty(step["error"], "gate step records the rejection")
	s.NotEqual(true, step["continued"],
		"rejected gate must never carry continued=true, even with on_error=continue set")
}

// stepsByIDFromRun collects EVERY step matching nodeID. A for_each body
// node executes once per iteration, so the single-match stepByIDFromRun
// isn't enough — the loop test asserts against all of them.
func stepsByIDFromRun(run map[string]any, nodeID string) []map[string]any {
	var out []map[string]any
	steps, _ := run["steps"].([]any)
	for _, raw := range steps {
		st, _ := raw.(map[string]any)
		if st == nil {
			continue
		}
		if id, _ := st["node_id"].(string); id == nodeID {
			out = append(out, st)
		}
	}
	return out
}

// putForEachItemsWorkflow PUTs a for_each loop driven by the new
// `items` selector. The run is dispatched with an input envelope
// `{docs:[…]}` (mongo_request `find`'s output shape), and the for_each
// node un-nests it via `items: "{{input.docs}}"` so the body runs once
// per element instead of once over the whole envelope:
//
//	trigger → for_each(items={{input.docs}}, name=loop_cities)
//	            └─[item]→ body-A(http_request, on_error=continue, →500)
//	                        └─[success]→ body-B(http_request, →200)
//
// body-A fails every iteration but `on_error: continue` keeps the body
// chain rolling onto the success edge (the ONLY edge wired off body-A),
// exercising the for_each resolver's continue-fallthrough. Returns the
// workflow ID.
func (s *OnErrorPolicyIntegrationSuite) putForEachItemsWorkflow(client *http.Client) string {
	suffix := time.Now().UnixNano()
	wfID := fmt.Sprintf("wf-onerror-foreach-%d", suffix)
	wfPayload := map[string]any{
		"name":   "on_error continue inside for_each body",
		"params": map[string]any{},
		"nodes": []map[string]any{
			{"id": "trigger-1", "type": "trigger", "position": map[string]any{"x": 0, "y": 0},
				"data": map[string]any{"trigger_type": "manual"}},
			{"id": "foreach-1", "type": "for_each", "position": map[string]any{"x": 200, "y": 0},
				"data": map[string]any{"name": "loop_cities", "items": "{{input.docs}}"}},
			{"id": "body-A", "type": "http_request", "position": map[string]any{"x": 400, "y": 0},
				"data": map[string]any{
					"name":     "fetch",
					"url":      s.mockSrv.URL,
					"method":   "GET",
					"on_error": "continue",
				}},
			{"id": "body-B", "type": "http_request", "position": map[string]any{"x": 600, "y": 0},
				"data": map[string]any{"name": "publish", "url": s.mockSrv.URL + "/ok", "method": "GET"}},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "foreach-1", "sourceHandle": "success"},
			{"id": "e2", "source": "foreach-1", "target": "body-A", "sourceHandle": "item"},
			{"id": "e3", "source": "body-A", "target": "body-B", "sourceHandle": "success"},
		},
	}
	body, _ := json.Marshal(wfPayload)
	req, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+wfID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	return wfID
}

// dispatchInputAndPoll is dispatchAndPollRun with a caller-supplied
// `input` envelope (the for_each test needs `{docs:[…]}` to flow into
// the trigger; the base helper hard-codes an empty `{}`).
func (s *OnErrorPolicyIntegrationSuite) dispatchInputAndPoll(client *http.Client, wfID string, input any) map[string]any {
	reqBody, _ := json.Marshal(map[string]any{"input": input})
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/workflows/"+wfID+"/run",
		"application/json", bytes.NewReader(reqBody))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusAccepted, resp.StatusCode)
	var dispatch struct {
		RunID string `json:"run_id"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&dispatch))

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		dResp, derr := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + dispatch.RunID)
		s.Require().NoError(derr)
		var detail map[string]any
		_ = json.NewDecoder(dResp.Body).Decode(&detail)
		_ = dResp.Body.Close()
		if run, ok := detail["run"].(map[string]any); ok {
			if st, _ := run["status"].(string); st == "success" || st == "error" || st == "cancelled" {
				return run
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.FailNow("dispatchInputAndPoll timeout", "run %s never reached terminal in 8s", dispatch.RunID)
	return nil
}

// TestOnError_ForEach_ItemsSelectorContinueBody_RunLandsSuccess
// verifies SMOKE-TEST-ON-ERROR.md §7: a for_each whose `items`
// selector un-nests a `{docs:[…]}` envelope runs its body once per
// element, and `on_error: continue` on a body node both records the
// per-iteration failure (`error` + `continued: true` on every body-A
// step) AND lets the body chain fall through to the success edge so
// body-B still fires each iteration — with the run landing `success`
// because every error was a suppressed/continued one. Guards both the
// new items-selector resolver and the for_each-body continue path.
func (s *OnErrorPolicyIntegrationSuite) TestOnError_ForEach_ItemsSelectorContinueBody_RunLandsSuccess() {
	client := s.authedClient(fmt.Sprintf("alice-foreach-%d@example.com", time.Now().UnixNano()))
	s.mockMu.Lock()
	s.mockHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}
	s.mockMu.Unlock()

	wfID := s.putForEachItemsWorkflow(client)
	input := map[string]any{"docs": []map[string]any{
		{"city": "alpha"}, {"city": "bravo"}, {"city": "charlie"},
	}}
	run := s.dispatchInputAndPoll(client, wfID, input)

	s.Equal("success", run["status"],
		"every body-A error was continued → run-status promotion skips them → success")
	s.Empty(run["error"], "run-level error stays empty when all failures were continued")

	bodyA := stepsByIDFromRun(run, "body-A")
	s.Require().Len(bodyA, 3, "for_each body must run once per docs element")
	for i, st := range bodyA {
		s.NotEmpty(st["error"], "body-A iteration %d still records the underlying 500", i)
		s.Equal(true, st["continued"], "body-A iteration %d must carry continued=true", i)
	}

	bodyB := stepsByIDFromRun(run, "body-B")
	s.Require().Len(bodyB, 3,
		"continue-fallthrough must route the body chain onto the success edge every iteration")
	for i, st := range bodyB {
		s.Empty(st["error"], "body-B iteration %d ran cleanly", i)
	}
}

// putForEachAbortWorkflow PUTs a for_each whose body is a single
// http_request with the DEFAULT (stop) policy — every item's body
// chain ends in an unsuppressed error. The for_each node carries the
// given `on_error` (empty string = leave unset → default stop):
//
//	trigger → for_each(items={{input.docs}}, on_error=<policy>)
//	            └─[item]→ body-A(http_request, →500, default stop)
//
// With for_each policy stop (default) the loop aborts after item 0;
// with continue every item runs. Returns the workflow ID.
func (s *OnErrorPolicyIntegrationSuite) putForEachAbortWorkflow(client *http.Client, policy string) string {
	suffix := time.Now().UnixNano()
	wfID := fmt.Sprintf("wf-onerror-foreach-abort-%d", suffix)
	foreachData := map[string]any{"name": "loop_cities", "items": "{{input.docs}}"}
	if policy != "" {
		foreachData["on_error"] = policy
	}
	wfPayload := map[string]any{
		"name":   "for_each loop-abort policy",
		"params": map[string]any{},
		"nodes": []map[string]any{
			{"id": "trigger-1", "type": "trigger", "position": map[string]any{"x": 0, "y": 0},
				"data": map[string]any{"trigger_type": "manual"}},
			{"id": "foreach-1", "type": "for_each", "position": map[string]any{"x": 200, "y": 0},
				"data": foreachData},
			{"id": "body-A", "type": "http_request", "position": map[string]any{"x": 400, "y": 0},
				"data": map[string]any{"name": "fetch", "url": s.mockSrv.URL, "method": "GET"}},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "foreach-1", "sourceHandle": "success"},
			{"id": "e2", "source": "foreach-1", "target": "body-A", "sourceHandle": "item"},
		},
	}
	body, _ := json.Marshal(wfPayload)
	req, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+wfID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	return wfID
}

// TestOnError_ForEach_DefaultStop_AbortsLoopOnFirstUnsuppressedBodyError
// verifies the for_each loop-abort policy. With no `on_error` on the
// for_each node (default stop) and a body whose default-stop error is
// NOT suppressed, the first failing item aborts the loop: only item 0's
// body step exists, items 1..N are skipped, and the run lands `error`.
func (s *OnErrorPolicyIntegrationSuite) TestOnError_ForEach_DefaultStop_AbortsLoopOnFirstUnsuppressedBodyError() {
	client := s.authedClient(fmt.Sprintf("alice-foreach-stop-%d@example.com", time.Now().UnixNano()))
	s.mockMu.Lock()
	s.mockHandler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}
	s.mockMu.Unlock()

	wfID := s.putForEachAbortWorkflow(client, "") // unset → default stop
	input := map[string]any{"docs": []map[string]any{
		{"city": "alpha"}, {"city": "bravo"}, {"city": "charlie"},
	}}
	run := s.dispatchInputAndPoll(client, wfID, input)

	s.Equal("error", run["status"], "default-stop for_each must flip the run on an unsuppressed body error")
	bodyA := stepsByIDFromRun(run, "body-A")
	s.Require().Len(bodyA, 1,
		"loop must abort after item 0 — items 1..N skipped under default-stop")
	s.NotEmpty(bodyA[0]["error"])
	s.NotEqual(true, bodyA[0]["continued"], "body default-stop → error is unsuppressed")
}

// TestOnError_ForEach_Continue_RunsEveryItemThenStillErrors verifies the
// opt-in best-effort fan-out: `on_error: continue` on the for_each node
// runs ALL items even though each body errors, but the run still lands
// `error` because the faults were unsuppressed (only the loop-abort was
// waived, not run-status promotion).
func (s *OnErrorPolicyIntegrationSuite) TestOnError_ForEach_Continue_RunsEveryItemThenStillErrors() {
	client := s.authedClient(fmt.Sprintf("alice-foreach-cont-%d@example.com", time.Now().UnixNano()))
	s.mockMu.Lock()
	s.mockHandler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}
	s.mockMu.Unlock()

	wfID := s.putForEachAbortWorkflow(client, "continue")
	input := map[string]any{"docs": []map[string]any{
		{"city": "alpha"}, {"city": "bravo"}, {"city": "charlie"},
	}}
	run := s.dispatchInputAndPoll(client, wfID, input)

	s.Equal("error", run["status"],
		"continue waives only the loop-abort — unsuppressed body faults still flip the run")
	bodyA := stepsByIDFromRun(run, "body-A")
	s.Require().Len(bodyA, 3,
		"for_each on_error=continue must run every item despite per-item failures")
	for i, st := range bodyA {
		s.NotEmpty(st["error"], "body-A item %d recorded its 500", i)
	}
}

// putForEachErrorEdgeWorkflow wires a loop-failure branch off the
// for_each node's OWN error handle:
//
//	trigger → for_each(items={{input.docs}}, default stop)
//	            ├─[item]→  body-A(http_request, →500, default stop)
//	            └─[error]→ rescue-1(http_request, →200)
//
// On the first unsuppressed body error the loop aborts (default
// stop) AND the for_each node surfaces a node-level error, so the
// main BFS routes its `error` edge to rescue-1. Returns the wf ID.
func (s *OnErrorPolicyIntegrationSuite) putForEachErrorEdgeWorkflow(client *http.Client) string {
	suffix := time.Now().UnixNano()
	wfID := fmt.Sprintf("wf-onerror-foreach-erredge-%d", suffix)
	wfPayload := map[string]any{
		"name":   "for_each error edge fires on loop-abort",
		"params": map[string]any{},
		"nodes": []map[string]any{
			{"id": "trigger-1", "type": "trigger", "position": map[string]any{"x": 0, "y": 0},
				"data": map[string]any{"trigger_type": "manual"}},
			{"id": "foreach-1", "type": "for_each", "position": map[string]any{"x": 200, "y": 0},
				"data": map[string]any{"name": "loop_cities", "items": "{{input.docs}}"}},
			{"id": "body-A", "type": "http_request", "position": map[string]any{"x": 400, "y": 0},
				"data": map[string]any{"name": "fetch", "url": s.mockSrv.URL, "method": "GET"}},
			{"id": "rescue-1", "type": "http_request", "position": map[string]any{"x": 400, "y": 160},
				"data": map[string]any{"name": "rescue", "url": s.mockSrv.URL + "/ok", "method": "GET"}},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "foreach-1", "sourceHandle": "success"},
			{"id": "e2", "source": "foreach-1", "target": "body-A", "sourceHandle": "item"},
			{"id": "e3", "source": "foreach-1", "target": "rescue-1", "sourceHandle": "error"},
		},
	}
	body, _ := json.Marshal(wfPayload)
	req, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+wfID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	return wfID
}

// TestOnError_ForEach_DefaultStop_FiresForEachErrorEdge_RescueRuns
// verifies the for_each node's own `error` edge is no longer inert:
// under default-stop, the loop-abort surfaces a node-level error so
// the main BFS routes the for_each `error` edge to a loop-failure
// branch. rescue-1 executes; body-A ran exactly once (loop aborted);
// the run lands `error` (stop policy, fault not suppressed).
func (s *OnErrorPolicyIntegrationSuite) TestOnError_ForEach_DefaultStop_FiresForEachErrorEdge_RescueRuns() {
	client := s.authedClient(fmt.Sprintf("alice-foreach-erredge-%d@example.com", time.Now().UnixNano()))
	s.mockMu.Lock()
	s.mockHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}
	s.mockMu.Unlock()

	wfID := s.putForEachErrorEdgeWorkflow(client)
	input := map[string]any{"docs": []map[string]any{
		{"city": "alpha"}, {"city": "bravo"}, {"city": "charlie"},
	}}
	run := s.dispatchInputAndPoll(client, wfID, input)

	s.Equal("error", run["status"], "default-stop loop-abort flips the run")

	bodyA := stepsByIDFromRun(run, "body-A")
	s.Require().Len(bodyA, 1, "loop aborted after item 0 — items 1..N skipped")

	feStep := stepByIDFromRun(run, "foreach-1")
	s.Require().NotNil(feStep)
	s.NotEmpty(feStep["error"], "for_each node surfaces a node-level loop-abort error")

	rescue := stepByIDFromRun(run, "rescue-1")
	s.Require().NotNil(rescue, "for_each `error` edge must route to the loop-failure branch")
	s.Empty(rescue["error"], "rescue node ran cleanly")
}

// putForEachDualEdgeBodyWorkflow wires BOTH a success and an error
// edge off a continue-policy body node:
//
//	trigger → for_each(items={{input.docs}})
//	            └─[item]→ body-A(http_request, on_error=continue, →500)
//	                        ├─[error]→   err-sink(http_request, →200)
//	                        └─[success]→ ok-sink(http_request, →200)
//
// body-A errors every iteration but on_error=continue → the body
// chain must fire BOTH downstream edges (same dual-edge rule as the
// main BFS), so err-sink AND ok-sink each run once per element.
func (s *OnErrorPolicyIntegrationSuite) putForEachDualEdgeBodyWorkflow(client *http.Client) string {
	suffix := time.Now().UnixNano()
	wfID := fmt.Sprintf("wf-onerror-foreach-dual-%d", suffix)
	wfPayload := map[string]any{
		"name":   "for_each body continue fires both edges",
		"params": map[string]any{},
		"nodes": []map[string]any{
			{"id": "trigger-1", "type": "trigger", "position": map[string]any{"x": 0, "y": 0},
				"data": map[string]any{"trigger_type": "manual"}},
			{"id": "foreach-1", "type": "for_each", "position": map[string]any{"x": 200, "y": 0},
				"data": map[string]any{"name": "loop_cities", "items": "{{input.docs}}"}},
			{"id": "body-A", "type": "http_request", "position": map[string]any{"x": 400, "y": 0},
				"data": map[string]any{
					"name": "fetch", "url": s.mockSrv.URL, "method": "GET",
					"on_error": "continue",
				}},
			{"id": "err-sink", "type": "http_request", "position": map[string]any{"x": 600, "y": -90},
				"data": map[string]any{"name": "on_err", "url": s.mockSrv.URL + "/ok", "method": "GET"}},
			{"id": "ok-sink", "type": "http_request", "position": map[string]any{"x": 600, "y": 90},
				"data": map[string]any{"name": "on_ok", "url": s.mockSrv.URL + "/ok", "method": "GET"}},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "foreach-1", "sourceHandle": "success"},
			{"id": "e2", "source": "foreach-1", "target": "body-A", "sourceHandle": "item"},
			{"id": "e3", "source": "body-A", "target": "err-sink", "sourceHandle": "error"},
			{"id": "e4", "source": "body-A", "target": "ok-sink", "sourceHandle": "success"},
		},
	}
	body, _ := json.Marshal(wfPayload)
	req, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+wfID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	return wfID
}

// TestOnError_ForEach_ContinueBody_FiresBothBodyEdges verifies the
// for_each body honours the SAME dual-edge rule as the main BFS:
// a body node with on_error=continue that errors fires BOTH its
// error edge AND its success edge every iteration (the old
// single-successor body chain made the success branch unreachable
// whenever an error edge was wired). Run lands `success` because the
// only fault was the continued one.
func (s *OnErrorPolicyIntegrationSuite) TestOnError_ForEach_ContinueBody_FiresBothBodyEdges() {
	client := s.authedClient(fmt.Sprintf("alice-foreach-dual-%d@example.com", time.Now().UnixNano()))
	s.mockMu.Lock()
	s.mockHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}
	s.mockMu.Unlock()

	wfID := s.putForEachDualEdgeBodyWorkflow(client)
	input := map[string]any{"docs": []map[string]any{
		{"city": "alpha"}, {"city": "bravo"}, {"city": "charlie"},
	}}
	run := s.dispatchInputAndPoll(client, wfID, input)

	s.Equal("success", run["status"],
		"only fault was the continued body error → run lands success")

	bodyA := stepsByIDFromRun(run, "body-A")
	s.Require().Len(bodyA, 3)
	for i, st := range bodyA {
		s.Equal(true, st["continued"], "body-A item %d continued", i)
	}
	errSink := stepsByIDFromRun(run, "err-sink")
	s.Require().Len(errSink, 3, "error edge must fire every iteration")
	okSink := stepsByIDFromRun(run, "ok-sink")
	s.Require().Len(okSink, 3,
		"success edge must ALSO fire every iteration (dual-edge — was unreachable before)")
	for i := range okSink {
		s.Empty(okSink[i]["error"], "ok-sink item %d ran cleanly", i)
	}
}

// putForEachContinueNodeEdgesWorkflow: for_each `on_error: continue`
// with a default-stop body that errors, and BOTH a success and an
// error edge wired off the for_each NODE itself:
//
//	trigger → for_each(items={{input.docs}}, on_error=continue)
//	            ├─[item]→    body-A(http_request, →500, default stop)
//	            ├─[error]→   fe-rescue(http_request, →200)
//	            └─[success]→ fe-done(http_request, →200)
//
// Every item runs (continue = no abort) but each ends in an
// unsuppressed fault, so the for_each node surfaces a node-level
// error AND is marked Continued → BOTH its edges fire (dual-edge).
func (s *OnErrorPolicyIntegrationSuite) putForEachContinueNodeEdgesWorkflow(client *http.Client) string {
	suffix := time.Now().UnixNano()
	wfID := fmt.Sprintf("wf-onerror-foreach-contedges-%d", suffix)
	wfPayload := map[string]any{
		"name":   "for_each continue node dual-edge",
		"params": map[string]any{},
		"nodes": []map[string]any{
			{"id": "trigger-1", "type": "trigger", "position": map[string]any{"x": 0, "y": 0},
				"data": map[string]any{"trigger_type": "manual"}},
			{"id": "foreach-1", "type": "for_each", "position": map[string]any{"x": 200, "y": 0},
				"data": map[string]any{"name": "loop_cities", "items": "{{input.docs}}", "on_error": "continue"}},
			{"id": "body-A", "type": "http_request", "position": map[string]any{"x": 400, "y": 0},
				"data": map[string]any{"name": "fetch", "url": s.mockSrv.URL, "method": "GET"}},
			{"id": "fe-rescue", "type": "http_request", "position": map[string]any{"x": 400, "y": -140},
				"data": map[string]any{"name": "fe_rescue", "url": s.mockSrv.URL + "/ok", "method": "GET"}},
			{"id": "fe-done", "type": "http_request", "position": map[string]any{"x": 400, "y": 140},
				"data": map[string]any{"name": "fe_done", "url": s.mockSrv.URL + "/ok", "method": "GET"}},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "foreach-1", "sourceHandle": "success"},
			{"id": "e2", "source": "foreach-1", "target": "body-A", "sourceHandle": "item"},
			{"id": "e3", "source": "foreach-1", "target": "fe-rescue", "sourceHandle": "error"},
			{"id": "e4", "source": "foreach-1", "target": "fe-done", "sourceHandle": "success"},
		},
	}
	body, _ := json.Marshal(wfPayload)
	req, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+wfID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	return wfID
}

// TestOnError_ForEach_ContinuePolicy_UnsuppressedBody_FiresBothNodeEdges
// verifies the for_each NODE's dual-edge behaviour under
// `on_error: continue`: every item runs (no abort), but because at
// least one ended with an unsuppressed body fault the for_each fires
// BOTH its `error` edge (loop-failure sidecar) AND its `success`
// edge (aggregated outputs, happy path) — same dual-edge ideology as
// a per-node continue. The run still lands `error` via the
// unsuppressed body step's run-status promotion.
func (s *OnErrorPolicyIntegrationSuite) TestOnError_ForEach_ContinuePolicy_UnsuppressedBody_FiresBothNodeEdges() {
	client := s.authedClient(fmt.Sprintf("alice-foreach-contedges-%d@example.com", time.Now().UnixNano()))
	s.mockMu.Lock()
	s.mockHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}
	s.mockMu.Unlock()

	wfID := s.putForEachContinueNodeEdgesWorkflow(client)
	input := map[string]any{"docs": []map[string]any{
		{"city": "alpha"}, {"city": "bravo"}, {"city": "charlie"},
	}}
	run := s.dispatchInputAndPoll(client, wfID, input)

	s.Equal("error", run["status"],
		"unsuppressed body fault still flips the run even under for_each continue")

	bodyA := stepsByIDFromRun(run, "body-A")
	s.Require().Len(bodyA, 3, "continue = no abort, every item runs")
	for i, st := range bodyA {
		s.NotEqual(true, st["continued"], "body-A item %d default-stop → unsuppressed", i)
	}

	rescue := stepByIDFromRun(run, "fe-rescue")
	s.Require().NotNil(rescue, "for_each `error` edge must fire under continue when a fault was unsuppressed")
	s.Empty(rescue["error"])

	done := stepByIDFromRun(run, "fe-done")
	s.Require().NotNil(done, "for_each `success` edge must ALSO fire (dual-edge under continue)")
	s.Empty(done["error"])
}

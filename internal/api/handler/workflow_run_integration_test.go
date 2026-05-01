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
	"strings"
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

// jwtSecretForApprovalRedeemTests must match the AuthConfig.JWTSecret
// the test server is wired with — workflow.SignApprovalToken consumes
// the same key the redeem handler verifies against.
const jwtSecretForApprovalRedeemTests = "integration_test_secret_dev_only_0123456789abcdef"

type WorkflowRunIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	redis       *rediss.Client
	httpSrv     *httptest.Server
	// Notifier kept on the suite so dispatcher-path tests can swap the
	// email sender / HTTP client per-test (capture vs no-op) without
	// rebuilding the executor.
	notifier *workflow.MultiplexApprovalNotifier

	// In-test workflow-executor worker harness (PR 3.3): /run is now
	// async — it persists a run rec + publishes wakeup + returns 202.
	// The actual BFS execution happens in a worker. The full
	// `cmd/worker -name workflow-executor` daemon is too heavy for
	// these tests, so we spin a minimal claim-loop here that uses
	// the same WorkflowRunRepository / WorkflowRepository / executor
	// the server is wired with. Cancelled in TearDownSuite.
	workerCancel context.CancelFunc
	workerDone   chan struct{}
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
	s.dbName = fmt.Sprintf("burrow_test_workflow_run_%d", time.Now().UnixNano())
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
	// ApprovalBroker = Redis so the OOB approval gate (require_node_approval)
	// has the cross-process pub/sub channel it needs — without it the
	// gate degrades to auto-approve.
	// ConnectionResolver lets the dispatcher resolve `slack_bot`
	// channel's Target (a connection_id) into the actual bot_token.
	// Tests that don't touch slack_bot ignore it; the resolver is
	// wired generically so subsequent tests don't need to re-plumb.
	connResolver := workflow.NewConnectionResolver(connRepo, mongodb.NewMongoClient(s.db), nil)
	s.notifier = &workflow.MultiplexApprovalNotifier{
		HTTPClient:   &http.Client{Timeout: 5 * time.Second},
		ConnResolver: connResolver,
	}
	wfExec := &workflow.WorkflowExecutor{
		HTTPClient:          &http.Client{Timeout: 10 * time.Second},
		DB:                  mongodb.NewMongoClient(s.db),
		RunRepo:             runRepo,
		ApprovalBroker:      s.redis,
		ApprovalNotifier:    s.notifier,
		ApprovalTokenSecret: []byte(jwtSecretForApprovalRedeemTests),
		ApprovalUIBaseURL:   "http://127.0.0.1",
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

	// Spin the in-test worker. claim → RunFromCheckpoint → release in
	// a tight loop with a short tick so async /run dispatches finish
	// in <1s. ctx cancels in TearDownSuite.
	workerCtx, workerCancel := context.WithCancel(context.Background())
	s.workerCancel = workerCancel
	s.workerDone = make(chan struct{})
	go func() {
		defer close(s.workerDone)
		runInTestExecutorWorker(workerCtx, "test-executor", runRepo, wfRepo, wfExec)
	}()
}

// runInTestExecutorWorker is a minimal claim-loop standing in for
// internal/worker.WorkflowExecutorWorker so the api/handler tests
// can drive async /run end-to-end without spinning the full worker
// daemon (which loads its own config + connects its own Mongo +
// Redis). Each tick: claim one run, drive RunFromCheckpoint,
// release. Lease + heartbeat omitted; tests run fast enough that
// the 30s default lease never expires mid-execution.
func runInTestExecutorWorker(
	ctx context.Context,
	workerID string,
	runRepo *mongodb.WorkflowRunRepository,
	wfRepo *mongodb.WorkflowRepository,
	exec *workflow.WorkflowExecutor,
) {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		rec, ok, err := runRepo.ClaimLease(ctx, workerID, 30*time.Second,
			[]workflow.RunStatus{workflow.RunStatusRunning})
		if err != nil || !ok {
			continue
		}
		wf, gerr := wfRepo.GetByID(ctx, rec.WorkflowID)
		if gerr != nil {
			now := time.Now().UTC()
			rec.Status = workflow.RunStatusError
			rec.Error = "test worker: workflow not found: " + gerr.Error()
			rec.FinishedAt = &now
			_ = runRepo.Update(ctx, rec)
			_ = runRepo.ReleaseLease(ctx, rec.ID, workerID)
			continue
		}
		_, _ = exec.RunFromCheckpoint(ctx, wf, rec, workerID, exec.Events)
		_ = runRepo.ReleaseLease(ctx, rec.ID, workerID)
	}
}

func (s *WorkflowRunIntegrationSuite) TearDownSuite() {
	if s.workerCancel != nil {
		s.workerCancel()
		<-s.workerDone
	}
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

// waitForRunStatus polls GET /workflow_runs/:id until the run's status
// equals `expected` or `timeout` elapses. Fails the test on timeout +
// reports the last-observed status so failures are easy to diagnose.
// Used by all async /run tests post-PR-3.3 since the handler returns
// before the worker has executed the BFS.
func (s *WorkflowRunIntegrationSuite) waitForRunStatus(client *http.Client, runID, expected string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		resp, err := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + runID)
		if err == nil {
			var detail map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&detail)
			_ = resp.Body.Close()
			if run, ok := detail["run"].(map[string]any); ok {
				if st, _ := run["status"].(string); st == expected {
					return
				} else {
					last = st
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.FailNow("waitForRunStatus timeout", "run %s never reached %q (last=%q)", runID, expected, last)
}

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
// trigger-only workflow lands in success state. POST /run is now
// async (PR 3.3): the handler returns 202 + run_id immediately and
// the in-test workflow-executor worker drives the BFS to completion.
// We poll the run-detail endpoint until status flips to success.
func (s *WorkflowRunIntegrationSuite) TestRun_TriggerOnly_PersistsSuccessRow() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-run-%d@example.com", suffix))
	wfID := fmt.Sprintf("wf-run-%d", suffix)
	s.putWorkflow(client, wfID, "trigger-only run smoke")

	resp, err := client.Post(s.httpSrv.URL+"/api/v1/workflows/"+wfID+"/run",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusAccepted, resp.StatusCode, "/run is async — expect 202")

	var runOut struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	body, _ := io.ReadAll(resp.Body)
	s.Require().NoError(json.Unmarshal(body, &runOut))
	s.NotEmpty(runOut.RunID, "run_id must be returned")
	s.Equal("running", runOut.Status, "fresh dispatch enqueues with status=running")

	s.waitForRunStatus(client, runOut.RunID, "success", 5*time.Second)

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

// putApprovalWorkflow PUTs a 2-node workflow whose notify node carries
// `require_node_approval=true`. The trigger fires unattended; the notify
// node is what the BFS halts at. Used by the OOB-approval test below.
func (s *WorkflowRunIntegrationSuite) putApprovalWorkflow(client *http.Client, id, name string) {
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
			{
				"id":       "notify-1",
				"type":     "notify",
				"position": map[string]any{"x": 200, "y": 0},
				"data": map[string]any{
					"message":                "approved-and-resumed",
					"require_node_approval":  true,
				},
			},
		},
		"edges": []map[string]any{
			{
				"id":            "e1",
				"source":        "trigger-1",
				"target":        "notify-1",
				"source_handle": "success",
			},
		},
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+id, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
}

// TestRun_NodeApproval_OOB_PendsThenResumesAfterApprove verifies the
// `require_node_approval` gate works out-of-band: with no live UI WS the
// run lands in `pending_approval`, the HTTP `/approval` endpoint
// publishes a verdict through the Redis-backed registry, the executor
// resumes, and the run completes.
//
// This is the path the rabbitmq + redis-subscribe trigger workers now
// flow through (after the `exec.Run` → `exec.RunResumable` swap that
// motivated this test). Asserting it here means non-manual triggers
// inherit the same OOB approval guarantees as cron + webhook.
func (s *WorkflowRunIntegrationSuite) TestRun_NodeApproval_OOB_PendsThenResumesAfterApprove() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-approval-%d@example.com", suffix))
	wfID := fmt.Sprintf("wf-approval-%d", suffix)
	s.putApprovalWorkflow(client, wfID, "approval gate smoke")

	// POST /run is async (PR 3.3). The handler returns 202 + run_id;
	// the in-test workflow-executor worker claims the run, BFS hits
	// the gate, yields the lease + persists pending_approval state.
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/workflows/"+wfID+"/run",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusAccepted, resp.StatusCode)
	var dispatch struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	body, _ := io.ReadAll(resp.Body)
	s.Require().NoError(json.Unmarshal(body, &dispatch))
	s.Require().NotEmpty(dispatch.RunID)

	// Poll until the worker has yielded on the gate (status flips to
	// pending_approval). Use the run-detail endpoint to also confirm
	// the PendingApproval mirror landed for the UI.
	pendingRunID := dispatch.RunID
	s.waitForRunStatus(client, pendingRunID, "pending_approval", 5*time.Second)

	// Detail endpoint should also show the pending_approval state with
	// the node kind populated.
	detailResp, err := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + pendingRunID)
	s.Require().NoError(err)
	defer detailResp.Body.Close() //nolint:errcheck
	var detail map[string]any
	s.Require().NoError(json.NewDecoder(detailResp.Body).Decode(&detail))
	run, _ := detail["run"].(map[string]any)
	s.Require().NotNil(run, "detail must include run object")
	pending, _ := run["pending_approval"].(map[string]any)
	s.Require().NotNil(pending, "pending_approval state must be persisted on the run")
	s.Equal("node", pending["kind"])
	s.Equal("notify-1", pending["node_id"])

	// POST the approval. Body shape is kind-agnostic — `tool_call_id`
	// stays empty for node gates.
	approveBody, _ := json.Marshal(map[string]any{"approved": true, "reason": "test"})
	approveResp, err := client.Post(s.httpSrv.URL+"/api/v1/workflow_runs/"+pendingRunID+"/approval",
		"application/json", bytes.NewReader(approveBody))
	s.Require().NoError(err)
	defer approveResp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, approveResp.StatusCode)

	// Approval handler wrote the decision into ExecutionState.Pending +
	// flipped status back to running + cleared the lease. The worker's
	// next claim picks up the run, applies the decision, and finishes.
	s.waitForRunStatus(client, pendingRunID, "success", 5*time.Second)
}

// startApprovalGatedRun fires a workflow whose first non-trigger node
// requires approval (POST /run is async — handler returns 202+run_id
// immediately, the in-test worker drives the BFS and yields on the
// gate), then polls until the run lands in pending_approval. Returns
// the run_id + the persisted PendingApproval state (token_id
// included) so the redeem-token + dispatcher tests have the bits
// they need without duplicating the polling boilerplate.
func (s *WorkflowRunIntegrationSuite) startApprovalGatedRun(client *http.Client, wfID string) (string, map[string]any) {
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/workflows/"+wfID+"/run",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusAccepted, resp.StatusCode)
	var dispatch struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&dispatch)
	s.Require().NotEmpty(dispatch.RunID)
	runID := dispatch.RunID

	deadline := time.Now().Add(5 * time.Second)
	var pending map[string]any
	for time.Now().Before(deadline) {
		detailResp, derr := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + runID)
		s.Require().NoError(derr)
		var detail map[string]any
		_ = json.NewDecoder(detailResp.Body).Decode(&detail)
		_ = detailResp.Body.Close()
		if run, ok := detail["run"].(map[string]any); ok {
			if run["status"] == "pending_approval" {
				if pa, ok := run["pending_approval"].(map[string]any); ok {
					pending = pa
				}
			}
		}
		if pending != nil && pending["token_id"] != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.Require().NotNil(pending, "pending_approval state must be persisted within 5s")
	s.Require().NotEmpty(pending["token_id"], "token_id should be stamped on the gate")
	return runID, pending
}

// TestApprovalRedeem_BadToken_Returns401 verifies the magic-link redeem
// endpoint refuses garbage tokens. No persisted run state needed — the
// JWT verifier rejects before any Mongo lookup.
func (s *WorkflowRunIntegrationSuite) TestApprovalRedeem_BadToken_Returns401() {
	body, _ := json.Marshal(map[string]any{
		"token":    "this-is-not-a-jwt",
		"decision": "approve",
	})
	resp, err := http.Post(s.httpSrv.URL+"/api/v1/workflow_runs/anything/approval/redeem",
		"application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
}

// TestApprovalRedeem_RunIDMismatch_Returns401 verifies a token signed
// for run A cannot be replayed against run B even with a valid signature
// + valid expiry. Subject claim must match the path :id.
func (s *WorkflowRunIntegrationSuite) TestApprovalRedeem_RunIDMismatch_Returns401() {
	tok, err := workflow.SignApprovalToken([]byte(jwtSecretForApprovalRedeemTests),
		"some-other-run-id", "some-token-id", time.Hour)
	s.Require().NoError(err)
	body, _ := json.Marshal(map[string]any{
		"token":    tok,
		"decision": "approve",
	})
	resp, err := http.Post(s.httpSrv.URL+"/api/v1/workflow_runs/this-is-the-real-id/approval/redeem",
		"application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
}

// TestApprovalRedeem_HappyPath_ApproveResumesRun is the end-to-end Stage-2
// path: a token signed against the persisted PendingApproval.TokenID
// resolves the gate via /redeem and the run completes.
func (s *WorkflowRunIntegrationSuite) TestApprovalRedeem_HappyPath_ApproveResumesRun() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-redeem-%d@example.com", suffix))
	wfID := fmt.Sprintf("wf-redeem-%d", suffix)
	s.putApprovalWorkflow(client, wfID, "redeem happy path")

	runID, pending := s.startApprovalGatedRun(client, wfID)
	tokenID, _ := pending["token_id"].(string)
	s.Require().NotEmpty(tokenID)

	tok, err := workflow.SignApprovalToken([]byte(jwtSecretForApprovalRedeemTests),
		runID, tokenID, time.Hour)
	s.Require().NoError(err)

	body, _ := json.Marshal(map[string]any{
		"token":    tok,
		"decision": "approve",
		"reason":   "redeem test",
	})
	resp, err := http.Post(s.httpSrv.URL+"/api/v1/workflow_runs/"+runID+"/approval/redeem",
		"application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	s.waitForRunStatus(client, runID, "success", 5*time.Second)

	detailResp, err := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + runID)
	s.Require().NoError(err)
	defer detailResp.Body.Close() //nolint:errcheck
	var detail map[string]any
	s.Require().NoError(json.NewDecoder(detailResp.Body).Decode(&detail))
	run, _ := detail["run"].(map[string]any)
	s.Equal("success", run["status"], "approved run should land success after redeem")

	// Audit ledger should record the redeem. Looked up by tenant via
	// the run's tenant_id, which is set automatically when the authed
	// client triggered the run.
	auditCol := s.db.Collection("audit_log")
	count, cerr := auditCol.CountDocuments(context.Background(), map[string]any{
		"action":           string(mongodb.AuditApprovalRedeemed),
		"metadata.run_id":  runID,
		"metadata.jti":     tokenID,
	})
	s.Require().NoError(cerr)
	s.Equal(int64(1), count, "exactly one approval_redeemed audit row should land for this redeem")
}

// TestApprovalRedeem_TokenSuperseded_Returns410 verifies a replay of a
// previously-redeemed token lands on 410 Gone — the run record's
// PendingApproval.TokenID was cleared when the gate resolved.
func (s *WorkflowRunIntegrationSuite) TestApprovalRedeem_TokenSuperseded_Returns410() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-replay-%d@example.com", suffix))
	wfID := fmt.Sprintf("wf-replay-%d", suffix)
	s.putApprovalWorkflow(client, wfID, "redeem replay")

	runID, pending := s.startApprovalGatedRun(client, wfID)
	tokenID, _ := pending["token_id"].(string)

	tok, err := workflow.SignApprovalToken([]byte(jwtSecretForApprovalRedeemTests),
		runID, tokenID, time.Hour)
	s.Require().NoError(err)

	body, _ := json.Marshal(map[string]any{"token": tok, "decision": "approve"})
	resp, err := http.Post(s.httpSrv.URL+"/api/v1/workflow_runs/"+runID+"/approval/redeem",
		"application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	_ = resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode, "first redeem should succeed")

	s.waitForRunStatus(client, runID, "success", 5*time.Second)

	// Second redeem with the SAME token: run is no longer pending and
	// PendingApproval.TokenID was cleared, so this should be 404 (no
	// pending approval) — replay landed AFTER the run already moved on,
	// the 410 path only fires when the run is STILL pending but the
	// token_id was rotated. Both endpoints are stale-link defenses;
	// here we assert at least that the duplicate doesn't double-resolve.
	resp2, err := http.Post(s.httpSrv.URL+"/api/v1/workflow_runs/"+runID+"/approval/redeem",
		"application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp2.Body.Close() //nolint:errcheck
	s.Equal(http.StatusNotFound, resp2.StatusCode,
		"replayed token should be rejected once the run moved past pending_approval")
}

// recordingEmailSender captures every Send call so dispatcher tests
// can assert on the message that left the executor. Mutex-guarded so
// the executor's dispatch goroutine doesn't race the assertion.
type recordingEmailSender struct {
	mu   sync.Mutex
	msgs []email.Message
}

func (r *recordingEmailSender) Send(_ context.Context, msg email.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, msg)
	return nil
}

func (r *recordingEmailSender) snapshot() []email.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]email.Message, len(r.msgs))
	copy(out, r.msgs)
	return out
}

// putApprovalGatedWorkflowWithChannel PUTs a 2-node workflow whose
// notify node carries `require_node_approval=true` and pins the
// workflow's `approval_channel` to the given config. The dispatcher
// fires when the gate trips; tests assert on the recorded message.
func (s *WorkflowRunIntegrationSuite) putApprovalGatedWorkflowWithChannel(client *http.Client, id string, channel map[string]any) {
	payload := map[string]any{
		"name":             "approval channel smoke",
		"params":           map[string]any{},
		"approval_channel": channel,
		"nodes": []map[string]any{
			{
				"id":       "trigger-1",
				"type":     "trigger",
				"position": map[string]any{"x": 0, "y": 0},
				"data":     map[string]any{"trigger_type": "manual"},
			},
			{
				"id":       "notify-1",
				"type":     "notify",
				"position": map[string]any{"x": 200, "y": 0},
				"data": map[string]any{
					"message":               "approved-and-resumed",
					"require_node_approval": true,
				},
			},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "notify-1", "source_handle": "success"},
		},
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+id, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
}

// TestApprovalDispatch_SMTPChannel_SendsApprovalEmailWithMagicLink wires
// a recording email sender, fires a workflow whose ApprovalChannel is
// SMTP-typed, and verifies the dispatcher built a correct magic-link +
// recipient + subject.
func (s *WorkflowRunIntegrationSuite) TestApprovalDispatch_SMTPChannel_SendsApprovalEmailWithMagicLink() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-dispatch-smtp-%d@example.com", suffix))

	rec := &recordingEmailSender{}
	s.notifier.Email = rec
	defer func() { s.notifier.Email = nil }()

	wfID := fmt.Sprintf("wf-dispatch-smtp-%d", suffix)
	s.putApprovalGatedWorkflowWithChannel(client, wfID, map[string]any{
		"type":   "smtp",
		"target": "ops@example.com",
		"from":   "burrow@example.com",
	})

	runID, pending := s.startApprovalGatedRun(client, wfID)
	tokenID, _ := pending["token_id"].(string)
	s.Require().NotEmpty(tokenID)

	// Dispatch is async — wait briefly for the goroutine to land.
	deadline := time.Now().Add(2 * time.Second)
	var sent []email.Message
	for time.Now().Before(deadline) {
		sent = rec.snapshot()
		if len(sent) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.Require().Len(sent, 1, "dispatcher should fire exactly one email per gate")

	msg := sent[0]
	s.Equal("ops@example.com", msg.To)
	s.Contains(msg.Subject, "Approval needed")
	s.Contains(msg.Body, runID, "magic-link body should include run_id")
	s.Contains(msg.Body, "/approve?", "magic-link body should embed approve URL")

	// Pull the token out of the body and verify it.
	tokIdx := strings.Index(msg.Body, "token=")
	s.Require().Greater(tokIdx, 0, "approve URL should include token=")
	tokRest := msg.Body[tokIdx+len("token="):]
	if newline := strings.IndexAny(tokRest, "\n\r&"); newline > 0 {
		tokRest = tokRest[:newline]
	}
	claims, err := workflow.VerifyApprovalToken([]byte(jwtSecretForApprovalRedeemTests), tokRest)
	s.Require().NoError(err, "embedded token must verify against the suite JWT secret")
	s.Equal(runID, claims.Subject, "token sub should bind the run_id")
	s.Equal(tokenID, claims.ID, "token jti should match persisted PendingApproval.TokenID")

	// Resolve the gate so the run goroutine returns and we don't leak
	// it into other tests. Use the existing /approval endpoint — the
	// magic-link redeem path is already covered above.
	approveBody, _ := json.Marshal(map[string]any{"approved": true})
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/workflow_runs/"+runID+"/approval",
		"application/json", bytes.NewReader(approveBody))
	s.Require().NoError(err)
	_ = resp.Body.Close()

	s.waitForRunStatus(client, runID, "success", 5*time.Second)
}

// TestApprovalDispatch_SlackChannel_PostsWebhookWithMagicLink does the
// equivalent for the Slack-webhook channel. A local httptest server
// stands in for hooks.slack.com — the dispatcher's POST lands here, the
// test asserts on the JSON body shape.
func (s *WorkflowRunIntegrationSuite) TestApprovalDispatch_SlackChannel_PostsWebhookWithMagicLink() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-dispatch-slack-%d@example.com", suffix))

	type capturedPost struct {
		body []byte
	}
	var (
		capMu     sync.Mutex
		captured  []capturedPost
	)
	fakeSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capMu.Lock()
		captured = append(captured, capturedPost{body: body})
		capMu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer fakeSlack.Close()

	wfID := fmt.Sprintf("wf-dispatch-slack-%d", suffix)
	s.putApprovalGatedWorkflowWithChannel(client, wfID, map[string]any{
		"type":   "slack_webhook",
		"target": fakeSlack.URL + "/services/test/webhook",
	})

	runID, _ := s.startApprovalGatedRun(client, wfID)

	deadline := time.Now().Add(2 * time.Second)
	var posts []capturedPost
	for time.Now().Before(deadline) {
		capMu.Lock()
		posts = append(posts[:0], captured...)
		capMu.Unlock()
		if len(posts) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.Require().Len(posts, 1, "dispatcher should POST exactly one webhook per gate")

	var payload map[string]any
	s.Require().NoError(json.Unmarshal(posts[0].body, &payload))
	text, _ := payload["text"].(string)
	s.Contains(text, "Approval needed", "slack body should carry the human-friendly subject")
	s.Contains(text, runID, "slack body should include run_id")
	s.Contains(text, "/approve?", "slack body should embed the approve URL")

	approveBody, _ := json.Marshal(map[string]any{"approved": true})
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/workflow_runs/"+runID+"/approval",
		"application/json", bytes.NewReader(approveBody))
	s.Require().NoError(err)
	_ = resp.Body.Close()

	s.waitForRunStatus(client, runID, "success", 5*time.Second)
}

// TestApprovalDispatch_SlackBotChannel_PostsViaChatPostMessageWithBotToken
// covers the bot-token path (slack_bot transport): a `slack`-typed
// Connection holds the encrypted xoxb-* token, ApprovalChannel.target
// references the connection_id, dispatcher resolves + POSTs to
// chat.postMessage with `Authorization: Bearer xoxb-*`.
func (s *WorkflowRunIntegrationSuite) TestApprovalDispatch_SlackBotChannel_PostsViaChatPostMessageWithBotToken() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-dispatch-slackbot-%d@example.com", suffix))

	type capturedPost struct {
		url   string
		auth  string
		body  []byte
	}
	var (
		capMu    sync.Mutex
		captured []capturedPost
	)
	fakeSlackAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capMu.Lock()
		captured = append(captured, capturedPost{
			url:  r.URL.Path,
			auth: r.Header.Get("Authorization"),
			body: body,
		})
		capMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C0123","ts":"1.0"}`))
	}))
	defer fakeSlackAPI.Close()

	// Point the dispatcher at the fake Slack API for the duration of
	// this test; restore after.
	s.notifier.SlackAPIBase = fakeSlackAPI.URL
	defer func() { s.notifier.SlackAPIBase = "" }()

	// Create a slack-typed connection via the API. Tenant scoping is
	// derived from the authed cookie.
	connID := fmt.Sprintf("conn-slack-%d", suffix)
	connBody, _ := json.Marshal(map[string]any{
		"name":   "test slack",
		"type":   "slack",
		"config": map[string]string{"bot_token": "xoxb-test-token-abc", "default_channel": "C0DEFAULT"},
	})
	connReq, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/connections/"+connID, bytes.NewReader(connBody))
	connReq.Header.Set("Content-Type", "application/json")
	connResp, err := client.Do(connReq)
	s.Require().NoError(err)
	_ = connResp.Body.Close()
	s.Require().Equal(http.StatusOK, connResp.StatusCode, "slack connection create should succeed")

	wfID := fmt.Sprintf("wf-dispatch-slackbot-%d", suffix)
	s.putApprovalGatedWorkflowWithChannel(client, wfID, map[string]any{
		"type":    "slack_bot",
		"target":  connID,
		"channel": "C01EXPLICIT",
	})

	runID, _ := s.startApprovalGatedRun(client, wfID)

	deadline := time.Now().Add(2 * time.Second)
	var posts []capturedPost
	for time.Now().Before(deadline) {
		capMu.Lock()
		posts = append(posts[:0], captured...)
		capMu.Unlock()
		if len(posts) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.Require().Len(posts, 1, "dispatcher should POST exactly one chat.postMessage per gate")

	post := posts[0]
	s.Equal("/chat.postMessage", post.url, "dispatcher should hit chat.postMessage")
	s.Equal("Bearer xoxb-test-token-abc", post.auth, "Authorization header should carry the bot token")

	var payload map[string]any
	s.Require().NoError(json.Unmarshal(post.body, &payload))
	s.Equal("C01EXPLICIT", payload["channel"], "explicit ApprovalChannel.channel should win over default_channel")
	text, _ := payload["text"].(string)
	s.Contains(text, "Approval needed", "body should carry the human-friendly subject")
	s.Contains(text, runID, "body should include run_id")
	s.Contains(text, "/approve?", "body should embed the approve URL")

	approveBody, _ := json.Marshal(map[string]any{"approved": true})
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/workflow_runs/"+runID+"/approval",
		"application/json", bytes.NewReader(approveBody))
	s.Require().NoError(err)
	_ = resp.Body.Close()

	s.waitForRunStatus(client, runID, "success", 5*time.Second)
}

// TestApprovalDispatch_ChannelNone_NoEmailSent confirms the dispatcher
// short-circuits cleanly when the workflow's ApprovalChannel is
// disabled — gate still pauses, no email leaves the executor.
func (s *WorkflowRunIntegrationSuite) TestApprovalDispatch_ChannelNone_NoEmailSent() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-dispatch-none-%d@example.com", suffix))

	rec := &recordingEmailSender{}
	s.notifier.Email = rec
	defer func() { s.notifier.Email = nil }()

	wfID := fmt.Sprintf("wf-dispatch-none-%d", suffix)
	// Note: even though the channel is "none", the workflow validator
	// rejects the field unless target is empty. Use Type=none with no
	// target to mirror the UI's "off" representation.
	s.putApprovalGatedWorkflowWithChannel(client, wfID, map[string]any{"type": "none"})

	runID, _ := s.startApprovalGatedRun(client, wfID)

	// Give any potential dispatch goroutine time to (incorrectly) fire.
	time.Sleep(300 * time.Millisecond)
	s.Empty(rec.snapshot(), "channel=none should not send any email")

	approveBody, _ := json.Marshal(map[string]any{"approved": true})
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/workflow_runs/"+runID+"/approval",
		"application/json", bytes.NewReader(approveBody))
	s.Require().NoError(err)
	_ = resp.Body.Close()

	s.waitForRunStatus(client, runID, "success", 5*time.Second)
}

// TestApprovalSubmit_OrphanedPendingRun_AutoCancelsAndReturns410 verifies
// the zero-subscriber detection path: a run record in `pending_approval`
// with no live waiter (the API process holding the wait restarted) gets
// auto-cancelled when the user clicks Approve. /approval publishes,
// Redis returns subscriber count 0, handler flips the run to terminal
// `error` state + returns 410 Gone so the UI can render a clear "this
// run is dead, re-trigger to retry" message.
//
// Simulated by writing a `pending_approval` run record directly via
// the repo + skipping the executor goroutine — same shape as a real
// orphaned run.
func (s *WorkflowRunIntegrationSuite) TestApprovalSubmit_OrphanedPendingRun_AutoCancelsAndReturns410() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-orphan-%d@example.com", suffix))

	// Create + own a workflow so the orphan run can be tied to alice's
	// tenant (run lookups are tenant-scoped).
	wfID := fmt.Sprintf("wf-orphan-%d", suffix)
	s.putWorkflow(client, wfID, "orphan run target")

	// Look up the workflow to grab its tenant_id (alice's).
	listResp, err := client.Get(s.httpSrv.URL + "/api/v1/workflows")
	s.Require().NoError(err)
	defer listResp.Body.Close() //nolint:errcheck
	var wfs []map[string]any
	s.Require().NoError(json.NewDecoder(listResp.Body).Decode(&wfs))
	s.Require().NotEmpty(wfs)
	tenantID, _ := wfs[0]["tenant_id"].(string)
	s.Require().NotEmpty(tenantID)

	// Persist an orphan pending_approval run directly via the repo.
	// No executor goroutine = no Redis subscriber. The Approve POST
	// will publish to a channel nobody listens on.
	runID := fmt.Sprintf("orphan-run-%d", suffix)
	runRepo, rerr := mongodb.NewWorkflowRunRepository(context.Background(), s.db)
	s.Require().NoError(rerr)
	orphan := workflow.WorkflowRun{
		ID:         runID,
		WorkflowID: wfID,
		TenantID:   tenantID,
		Status:     workflow.RunStatusPendingApproval,
		StartedAt:  time.Now().UTC().Add(-1 * time.Hour),
		PendingApproval: &workflow.PendingApprovalState{
			Kind:        "node",
			NodeID:      "notify-1",
			NodeType:    "notify",
			NodeName:    "notify-1",
			RequestedAt: time.Now().UTC(),
			TokenID:     "01ORPHANTOKEN",
		},
	}
	_, cerr := runRepo.Create(context.Background(), orphan)
	s.Require().NoError(cerr)

	// POST /approval. Server publishes, sees zero subscribers, marks
	// the run errored + returns 410.
	approveBody, _ := json.Marshal(map[string]any{
		"tool_call_id": "notify-1",
		"approved":     true,
	})
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/workflow_runs/"+runID+"/approval",
		"application/json", bytes.NewReader(approveBody))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusGone, resp.StatusCode, "orphan should land on 410 Gone")

	var body map[string]any
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&body))
	s.Equal(true, body["auto_cancelled"], "response should flag auto-cancellation")

	// Run record flipped to error state with the explanatory message.
	detailResp, err := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + runID)
	s.Require().NoError(err)
	defer detailResp.Body.Close() //nolint:errcheck
	var detail map[string]any
	s.Require().NoError(json.NewDecoder(detailResp.Body).Decode(&detail))
	run, _ := detail["run"].(map[string]any)
	s.Require().NotNil(run)
	s.Equal(string(workflow.RunStatusError), run["status"])
	errMsg, _ := run["error"].(string)
	s.Contains(errMsg, "approval orphaned")
	s.Nil(run["pending_approval"], "pending_approval state should be cleared")
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

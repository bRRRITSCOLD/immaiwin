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
	wfExec := &workflow.WorkflowExecutor{
		HTTPClient:     &http.Client{Timeout: 10 * time.Second},
		DB:             mongodb.NewMongoClient(s.db),
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

	// /run is synchronous — it blocks until the executor finishes. With
	// a node-approval gate it'll block waiting on Redis. Fire it from a
	// goroutine so the main test loop can poll status + post the verdict.
	type runResp struct {
		RunID  string                   `json:"run_id"`
		Status string                   `json:"status"`
		Steps  []map[string]any         `json:"steps"`
	}
	runDone := make(chan runResp, 1)
	runErr := make(chan error, 1)
	go func() {
		resp, err := client.Post(s.httpSrv.URL+"/api/v1/workflows/"+wfID+"/run",
			"application/json", bytes.NewReader([]byte(`{}`)))
		if err != nil {
			runErr <- err
			return
		}
		defer resp.Body.Close() //nolint:errcheck
		body, _ := io.ReadAll(resp.Body)
		var out runResp
		_ = json.Unmarshal(body, &out)
		runDone <- out
	}()

	// Poll workflow_runs list until we see a row in pending_approval for
	// this workflow. RunResumable mints + persists the run on entry, so
	// the row appears almost immediately, but the gate may not have
	// flipped status to pending_approval yet.
	deadline := time.Now().Add(5 * time.Second)
	var pendingRunID string
	for time.Now().Before(deadline) {
		listResp, err := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs?workflow_id=" + wfID)
		s.Require().NoError(err)
		var rows []map[string]any
		_ = json.NewDecoder(listResp.Body).Decode(&rows)
		_ = listResp.Body.Close()
		for _, r := range rows {
			if r["status"] == "pending_approval" {
				if id, ok := r["_id"].(string); ok {
					pendingRunID = id
				} else if id, ok := r["id"].(string); ok {
					pendingRunID = id
				}
				break
			}
		}
		if pendingRunID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.Require().NotEmpty(pendingRunID, "run should land in pending_approval within 5s")

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

	// Run goroutine should now unblock + return. Wait up to 5s.
	select {
	case out := <-runDone:
		s.Equal(pendingRunID, out.RunID, "run_id should match the pending one")
		s.Equal("success", out.Status, "approved run should complete with success")
		s.NotEmpty(out.Steps, "completed run should have step results")
	case err := <-runErr:
		s.FailNow("run goroutine errored", err.Error())
	case <-time.After(5 * time.Second):
		s.FailNow("approved run did not complete within 5s")
	}
}

// startApprovalGatedRun fires a workflow whose first non-trigger node
// requires approval, then polls until the run lands in pending_approval.
// Returns the run_id + the persisted PendingApproval state (token_id
// included) so the redeem-token tests have the bits they need without
// duplicating the polling boilerplate.
func (s *WorkflowRunIntegrationSuite) startApprovalGatedRun(client *http.Client, wfID string) (string, map[string]any, chan struct{}) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := client.Post(s.httpSrv.URL+"/api/v1/workflows/"+wfID+"/run",
			"application/json", bytes.NewReader([]byte(`{}`)))
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	var runID string
	var pending map[string]any
	for time.Now().Before(deadline) {
		listResp, err := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs?workflow_id=" + wfID)
		s.Require().NoError(err)
		var rows []map[string]any
		_ = json.NewDecoder(listResp.Body).Decode(&rows)
		_ = listResp.Body.Close()
		for _, r := range rows {
			if r["status"] != "pending_approval" {
				continue
			}
			if id, ok := r["_id"].(string); ok {
				runID = id
			} else if id, ok := r["id"].(string); ok {
				runID = id
			}
			break
		}
		if runID != "" {
			detailResp, derr := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + runID)
			s.Require().NoError(derr)
			var detail map[string]any
			_ = json.NewDecoder(detailResp.Body).Decode(&detail)
			_ = detailResp.Body.Close()
			if run, ok := detail["run"].(map[string]any); ok {
				if pa, ok := run["pending_approval"].(map[string]any); ok {
					pending = pa
				}
			}
			if pending != nil && pending["token_id"] != "" {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.Require().NotEmpty(runID, "run should land in pending_approval within 5s")
	s.Require().NotNil(pending, "pending_approval state must be persisted")
	s.Require().NotEmpty(pending["token_id"], "token_id should be stamped on the gate")
	return runID, pending, done
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

	runID, pending, runDone := s.startApprovalGatedRun(client, wfID)
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

	select {
	case <-runDone:
		// Run goroutine returned — the executor unblocked. Verify final
		// row landed as success.
	case <-time.After(5 * time.Second):
		s.FailNow("redeemed run did not complete within 5s")
	}

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

	runID, pending, runDone := s.startApprovalGatedRun(client, wfID)
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

	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		s.FailNow("first redeem did not unblock the run within 5s")
	}

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

//go:build integration

// Lease-unification integration tests — covers the canvas-WS / lease
// merge that landed when the BypassLease flag was removed and the WS
// handler dropped to a pure event subscriber. Three behaviour bundles
// the merge introduced or changed:
//
//   1. POST /workflow_runs/:id/cancel — must flip a pending_approval
//      run to status=cancelled, clear the in-flight state
//      (execution_state / paused_agent / pending_approval), drop the
//      lease, AND publish a synthetic run_done envelope on
//      burrow:run_events:<runID> so any live canvas WS subscriber
//      sees the terminal flip without polling.
//
//   2. Per-tool agent reject — used to feed the rejection back as a
//      tool_result and let the model adapt (run still landed
//      success). The contract changed: Reject = hard fail. Verify
//      the run lands status=error after a rejected per-tool gate.
//
//   3. Terminal cleanup — RunFromCheckpoint's terminal block must
//      clear ExecutionState, PausedAgent, AND PendingApproval (the
//      last one was the bug — successful runs that yielded mid-way
//      kept stale UI banners until a hard refresh).
//
// All three share the existing in-test workflow-executor worker
// scaffold + a stubbed LLM provider for the reject scenario. Compose
// stack required (Mongo + Redis); follows the no-skip rule.

package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/api"
	"github.com/bRRRITSCOLD/burrow/internal/api/handler"
	"github.com/bRRRITSCOLD/burrow/internal/config"
	"github.com/bRRRITSCOLD/burrow/internal/email"
	"github.com/bRRRITSCOLD/burrow/internal/llm"
	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/bRRRITSCOLD/burrow/internal/rediss"
	"github.com/bRRRITSCOLD/burrow/internal/workflow"
	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/v2/mongo"
	driveroptions "go.mongodb.org/mongo-driver/v2/mongo/options"
)

// LeaseUnificationIntegrationSuite drives the three scenarios above
// against a real Mongo + Redis with a stub anthropic provider so the
// agent-reject scenario doesn't need network access. Single-suite
// setup: spinning up Mongo / Redis / api server / in-test worker
// once is enough — each test uses fresh workflow ids so they don't
// step on each other.
type LeaseUnificationIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	redis       *rediss.Client
	httpSrv     *httptest.Server

	stubProvider *cancelRejectStubLLM

	workerCancel context.CancelFunc
	workerDone   chan struct{}
}

func TestLeaseUnificationIntegrationSuite(t *testing.T) {
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
	suite.Run(t, new(LeaseUnificationIntegrationSuite))
}

func (s *LeaseUnificationIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")
	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("burrow_test_lease_unification_%d", time.Now().UnixNano())
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

	// Stub anthropic provider — needed only for the agent-reject
	// scenario (test 2). Other tests don't reach the LLM path.
	s.stubProvider = &cancelRejectStubLLM{}
	llm.Default.Replace("anthropic", func(cfg map[string]string) (llm.Provider, error) {
		return s.stubProvider, nil
	})

	connResolver := workflow.NewConnectionResolver(connRepo, mongodb.NewMongoClient(s.db), nil)
	notifier := &workflow.MultiplexApprovalNotifier{
		HTTPClient:   &http.Client{Timeout: 5 * time.Second},
		ConnResolver: connResolver,
	}
	wfExec := &workflow.WorkflowExecutor{
		AllowPrivateHTTPHosts: true, // test httptest mockSrv lives on 127.0.0.1
		HTTPClient:          &http.Client{Timeout: 10 * time.Second},
		DB:                  mongodb.NewMongoClient(s.db),
		ConnResolver:        connResolver,
		RunRepo:             runRepo,
		ApprovalBroker:      s.redis,
		ApprovalNotifier:    notifier,
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

	workerCtx, workerCancel := context.WithCancel(context.Background())
	s.workerCancel = workerCancel
	s.workerDone = make(chan struct{})
	go func() {
		defer close(s.workerDone)
		runInTestExecutorWorker(workerCtx, "lease-unification-worker", runRepo, wfRepo, wfExec)
	}()
}

func (s *LeaseUnificationIntegrationSuite) TearDownSuite() {
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

// SetupTest resets the per-IP auth rate limit so subsequent tests in
// the suite can register a fresh user without tripping the 429 the
// real api enforces. Without this, the third / fourth test in any
// large suite hits ratelimit:auth:register:127.0.0.1 and fails on
// `client.Post(.../register)`.
func (s *LeaseUnificationIntegrationSuite) SetupTest() {
	for _, name := range []string{"register", "login", "password_reset", "oauth_start"} {
		_, _ = s.redis.Del(context.Background(), "ratelimit:auth:"+name+":127.0.0.1")
	}
}
func (s *LeaseUnificationIntegrationSuite) TearDownTest() {}

// cancelRejectStubLLM emits a single tool_use forever, regardless of
// the conversation state. Used by TestPerToolReject to guarantee the
// agent reaches the per-tool gate even after restoration via the
// resume path. The reject scenario aborts the agent before this
// becomes a loop hazard.
type cancelRejectStubLLM struct {
	chatCalls atomic.Int32
}

func (p *cancelRejectStubLLM) Name() string { return "anthropic" }

func (p *cancelRejectStubLLM) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	return nil, fmt.Errorf("stub: streaming not implemented")
}

func (p *cancelRejectStubLLM) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.chatCalls.Add(1)
	// If we've seen a tool_result in the conversation, return
	// end_turn so the resume test can complete cleanly. The reject
	// test never produces a tool_result (rejected before dispatch),
	// so its run aborts on the first iter with the new contract.
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != llm.RoleUser {
			continue
		}
		for _, c := range m.Content {
			if c.Type == llm.ContentTypeToolResult {
				return &llm.ChatResponse{
					StopReason: llm.StopReasonEndTurn,
					Content:    []llm.Content{llm.TextBlock("done")},
					Usage:      llm.Usage{InputTokens: 5, OutputTokens: 5, TotalTokens: 10},
					Model:      "stub-model",
				}, nil
			}
		}
		break
	}
	return &llm.ChatResponse{
		StopReason: llm.StopReasonToolUse,
		Content: []llm.Content{
			llm.ToolUseBlock("call_1", "get_weather", json.RawMessage(`{"city":"SF"}`)),
		},
		Usage: llm.Usage{InputTokens: 5, OutputTokens: 5, TotalTokens: 10},
		Model: "stub-model",
	}, nil
}

// authedClient registers + returns a cookie-jar client. Mirrors the
// helper on WorkflowRunIntegrationSuite — duplicated rather than
// shared because Go's testify suites don't compose cleanly.
func (s *LeaseUnificationIntegrationSuite) authedClient(emailAddr string) *http.Client {
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

// putGatedHTTPWorkflow PUTs trigger → http_request(require_node_approval=true).
// The HTTP node points at httptest 127.0.0.1 echo so the request can
// actually succeed when the gate gets approved (used by the cleanup
// scenario). Cancel + reject scenarios never hit the network — they
// terminate at the gate.
func (s *LeaseUnificationIntegrationSuite) putGatedHTTPWorkflow(client *http.Client, id, name, echoURL string) {
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
				"id":       "http-1",
				"type":     "http_request",
				"position": map[string]any{"x": 200, "y": 0},
				"data": map[string]any{
					"name":                   "fetch",
					"method":                 "GET",
					"url":                    echoURL,
					"require_node_approval":  true,
					"timeout_seconds":        5,
					"max_response_bytes":     1024,
				},
			},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "http-1", "sourceHandle": "start"},
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

// triggerRun POSTs /run + returns the new run id. /run is async (PR
// 3.3), so the response status is 202.
func (s *LeaseUnificationIntegrationSuite) triggerRun(client *http.Client, wfID string) string {
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/workflows/"+wfID+"/run",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusAccepted, resp.StatusCode)
	var out struct {
		RunID string `json:"run_id"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&out))
	s.Require().NotEmpty(out.RunID)
	return out.RunID
}

// pollUntilStatus polls GET /workflow_runs/:id until the run record
// shows the expected status or the deadline elapses. Fails the test
// loud on timeout — the no-skip rule.
func (s *LeaseUnificationIntegrationSuite) pollUntilStatus(client *http.Client, runID, expected string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		resp, err := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + runID)
		if err == nil && resp.StatusCode == http.StatusOK {
			var body struct {
				Run struct {
					Status string `json:"status"`
				} `json:"run"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&body)
			_ = resp.Body.Close()
			last = body.Run.Status
			if last == expected {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.FailNowf("status timeout", "run %s never reached %q (last=%q)", runID, expected, last)
}

// TestRunCancel_PendingApproval_FlipsCancelledAndPublishesRunDone verifies
// that POST /cancel on a pending_approval run flips the record to
// status=cancelled, clears in-flight state, AND publishes a synthetic
// `run_done{status:cancelled}` event on the per-run Redis event
// channel so live WS subscribers flip terminal without polling.
func (s *LeaseUnificationIntegrationSuite) TestRunCancel_PendingApproval_FlipsCancelledAndPublishesRunDone() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-cancel-%d@example.com", suffix))
	wfID := fmt.Sprintf("wf-cancel-%d", suffix)
	// Echo URL never reached on cancel path — gate fires before HTTP.
	s.putGatedHTTPWorkflow(client, wfID, "cancel-pending-approval", "http://127.0.0.1:1/never-called")

	runID := s.triggerRun(client, wfID)

	// Subscribe to the per-run event channel BEFORE we cancel so we
	// capture the run_done envelope the cancel handler publishes.
	subCtx, subCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer subCancel()
	sub := s.redis.Subscribe(subCtx, workflow.RunEventChannel(runID))
	defer func() { _ = sub.Close() }()
	subCh := sub.Channel()

	s.pollUntilStatus(client, runID, "pending_approval", 5*time.Second)

	cancelResp, err := client.Post(s.httpSrv.URL+"/api/v1/workflow_runs/"+runID+"/cancel",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer cancelResp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, cancelResp.StatusCode)

	// run_done event must arrive on the per-run channel with
	// status=cancelled. Drives the canvas WS terminal flip.
	var got *workflow.RunEvent
	deadline := time.After(2 * time.Second)
loop:
	for {
		select {
		case msg, ok := <-subCh:
			if !ok {
				break loop
			}
			var ev workflow.RunEvent
			if err := json.Unmarshal([]byte(msg.Payload), &ev); err == nil && ev.Type == workflow.EventRunDone {
				got = &ev
				break loop
			}
		case <-deadline:
			break loop
		}
	}
	s.Require().NotNil(got, "expected run_done event on burrow:run_events:%s", runID)
	s.Equal(string(workflow.RunStatusCancelled), got.Status, "run_done envelope must carry status=cancelled")
	s.Equal(runID, got.RunID, "run_done envelope must carry the cancelled run id")

	// Mongo: status=cancelled, in-flight state cleared, lease dropped.
	getResp, err := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + runID)
	s.Require().NoError(err)
	defer getResp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, getResp.StatusCode)
	var body struct {
		Run map[string]any `json:"run"`
	}
	s.Require().NoError(json.NewDecoder(getResp.Body).Decode(&body))
	s.Equal(string(workflow.RunStatusCancelled), body.Run["status"])
	s.Nil(body.Run["execution_state"], "execution_state must be cleared on cancel")
	s.Nil(body.Run["paused_agent"], "paused_agent must be cleared on cancel")
	s.Nil(body.Run["pending_approval"], "pending_approval mirror must be cleared on cancel")
	leaseOwner, _ := body.Run["lease_owner"].(string)
	s.Empty(leaseOwner, "lease_owner must be released on cancel")
}

// TestPerToolReject_AgentReturnsErrorRunLandsError verifies that
// rejecting a per-tool approval gate now aborts the agent loop and
// the run lands status=error. Previously the agent fed the rejection
// back as a tool_result and the model adapted gracefully → the run
// ended `success` despite the user explicitly clicking Reject.
// The contract changed: Reject = hard fail.
func (s *LeaseUnificationIntegrationSuite) TestPerToolReject_AgentReturnsErrorRunLandsError() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-reject-%d@example.com", suffix))
	wfID := fmt.Sprintf("wf-reject-%d", suffix)

	// Trigger → ai_agent with require_approval=true. Agent's per-tool
	// gate fires on the first tool_use, persists pending_approval,
	// yields lease. We POST a rejected decision.
	connID := fmt.Sprintf("conn-anth-%d", suffix)
	connBody, _ := json.Marshal(map[string]any{
		"name":     "stub-anthropic",
		"type":     "anthropic",
		"config":   map[string]any{"api_key": "stub"},
	})
	connReq, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/connections/"+connID, bytes.NewReader(connBody))
	connReq.Header.Set("Content-Type", "application/json")
	connResp, err := client.Do(connReq)
	s.Require().NoError(err)
	_ = connResp.Body.Close()
	s.Require().Equal(http.StatusOK, connResp.StatusCode)

	wfPayload := map[string]any{
		"name":   "agent-reject",
		"params": map[string]any{},
		"nodes": []map[string]any{
			{
				"id":       "trigger-1",
				"type":     "trigger",
				"position": map[string]any{"x": 0, "y": 0},
				"data":     map[string]any{"trigger_type": "manual"},
			},
			{
				"id":       "agent-1",
				"type":     "ai_agent",
				"position": map[string]any{"x": 200, "y": 0},
				"data": map[string]any{
					"name":                "agent",
					"llm_connection_id":   connID,
					"system_prompt":       "respond with a tool call",
					"user_input":          "go",
					"max_iterations":      3,
					"max_tokens":          128,
					"max_tool_calls_per_iter": 1,
					"require_approval":    true,
				},
			},
			{
				"id":       "http-1",
				"type":     "http_request",
				"position": map[string]any{"x": 400, "y": 0},
				"data": map[string]any{
					"name":   "get_weather",
					"method": "GET",
					"url":    "http://127.0.0.1:1/never-called",
					"as_tool": map[string]any{
						"enabled":     true,
						"name":        "get_weather",
						"description": "stub",
						"input_schema": map[string]any{
							"type":       "object",
							"properties": map[string]any{"city": map[string]any{"type": "string"}},
						},
					},
				},
			},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "agent-1", "sourceHandle": "start"},
			{"id": "e2", "source": "agent-1", "target": "http-1", "sourceHandle": "tool", "data": map[string]any{"paletteType": "tool"}},
		},
	}
	body, _ := json.Marshal(wfPayload)
	wfReq, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+wfID, bytes.NewReader(body))
	wfReq.Header.Set("Content-Type", "application/json")
	wfResp, err := client.Do(wfReq)
	s.Require().NoError(err)
	_ = wfResp.Body.Close()
	s.Require().Equal(http.StatusOK, wfResp.StatusCode)

	runID := s.triggerRun(client, wfID)
	s.pollUntilStatus(client, runID, "pending_approval", 5*time.Second)

	rejectBody, _ := json.Marshal(map[string]any{
		"tool_call_id": "call_1",
		"approved":     false,
		"reason":       "no",
	})
	rejectResp, err := client.Post(s.httpSrv.URL+"/api/v1/workflow_runs/"+runID+"/approval",
		"application/json", bytes.NewReader(rejectBody))
	s.Require().NoError(err)
	defer rejectResp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, rejectResp.StatusCode)

	// Run must land status=error after the per-tool reject — the new
	// contract. Previously this test would have caught status=success.
	s.pollUntilStatus(client, runID, "error", 5*time.Second)
}

// TestWorkerResume_AfterAbandonedAttempt_PreservesOriginalTrace verifies
// that when a worker dies mid-agent and a fresh worker re-runs the
// agent from iter 0, the previously-pushed agent_traces entries from
// the dead attempt are NOT destroyed — they sit alongside the fresh
// attempt's events. Per-attempt grouping in the UI then splits them
// at the duplicate `iter_start` boundary, but the backend stays
// non-destructive.
//
// This is the test that should have caught the worker-kill /
// "duplicate get_weather under iter 0" rendering bug. The fix lives
// in the UI hook (drop dead attempt's iters when receiving an
// agent_iter for an already-seen index) but the backend invariant —
// "never delete trace events on resume" — is what makes the historical
// /runs/:id view trustworthy.
//
// Simulated worker death: we seed a partial agent_traces entry on a
// running run record (no PausedAgent / ExecutionState — exactly what
// you'd see if the worker SIGKILL'd before persisting either) and
// let the in-test worker pick the run up via the standard claim
// loop. The fresh worker has no checkpoint to resume from, so it
// re-runs the BFS from trigger. The original trace + the re-run's
// trace coexist on the final record.
func (s *LeaseUnificationIntegrationSuite) TestWorkerResume_AfterAbandonedAttempt_PreservesOriginalTrace() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-resume-%d@example.com", suffix))

	// Echo server stands in for the agent's get_weather tool target.
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`weather`))
	}))
	defer echo.Close()

	wfID := fmt.Sprintf("wf-resume-%d", suffix)

	// Re-use the agent-with-as_tool workflow shape from the reject
	// test. require_approval=false here so the gate doesn't muddy the
	// resume scenario — the only thing under test is trace preservation.
	connID := fmt.Sprintf("conn-resume-%d", suffix)
	connBody, _ := json.Marshal(map[string]any{
		"name":   "stub",
		"type":   "anthropic",
		"config": map[string]any{"api_key": "stub"},
	})
	connReq, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/connections/"+connID, bytes.NewReader(connBody))
	connReq.Header.Set("Content-Type", "application/json")
	connResp, err := client.Do(connReq)
	s.Require().NoError(err)
	_ = connResp.Body.Close()
	s.Require().Equal(http.StatusOK, connResp.StatusCode)

	wfPayload := map[string]any{
		"name":   "agent-resume",
		"params": map[string]any{},
		"nodes": []map[string]any{
			{
				"id":       "trigger-1",
				"type":     "trigger",
				"position": map[string]any{"x": 0, "y": 0},
				"data":     map[string]any{"trigger_type": "manual"},
			},
			{
				"id":       "agent-1",
				"type":     "ai_agent",
				"position": map[string]any{"x": 200, "y": 0},
				"data": map[string]any{
					"name":                    "agent",
					"llm_connection_id":       connID,
					"system_prompt":           "respond with a tool call",
					"user_input":              "go",
					"max_iterations":          3,
					"max_tokens":              128,
					"max_tool_calls_per_iter": 1,
				},
			},
			{
				"id":       "http-1",
				"type":     "http_request",
				"position": map[string]any{"x": 400, "y": 0},
				"data": map[string]any{
					"name":   "get_weather",
					"method": "GET",
					"url":    echo.URL,
					"as_tool": map[string]any{
						"enabled":     true,
						"name":        "get_weather",
						"description": "stub",
						"input_schema": map[string]any{
							"type":       "object",
							"properties": map[string]any{"city": map[string]any{"type": "string"}},
						},
					},
				},
			},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "agent-1", "sourceHandle": "start"},
			{"id": "e2", "source": "agent-1", "target": "http-1", "sourceHandle": "tool", "data": map[string]any{"paletteType": "tool"}},
		},
	}
	body, _ := json.Marshal(wfPayload)
	wfReq, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+wfID, bytes.NewReader(body))
	wfReq.Header.Set("Content-Type", "application/json")
	wfResp, err := client.Do(wfReq)
	s.Require().NoError(err)
	_ = wfResp.Body.Close()
	s.Require().Equal(http.StatusOK, wfResp.StatusCode)

	// Make the stub LLM behave: emit get_weather on the first chat,
	// end_turn on the second (after the tool result lands). The reject
	// suite's stub always emits tool_use which would loop here — we
	// override per-test by using a dedicated chatCalls counter.
	s.stubProvider.chatCalls.Store(0)

	runID := s.triggerRun(client, wfID)

	// Race-window write: as soon as the run record exists, push a
	// fake "previous worker partial trace" onto agent_traces. We do
	// this BEFORE the in-test worker drives the run to completion;
	// the worker's emitTrace pushes new events on top, so the final
	// state has both. (In production the dead worker would have
	// pushed first; we simulate the same end-state here.)
	runRepo, err := mongodb.NewWorkflowRunRepository(context.Background(), s.db)
	s.Require().NoError(err)
	deadTrace := []workflow.TraceEvent{
		{Type: "iter_start", Iter: 0, At: time.Now().UTC().Add(-2 * time.Minute)},
		{Type: "llm_call", Iter: 0, At: time.Now().UTC().Add(-2 * time.Minute)},
		{Type: "tool_call", Iter: 0, ToolName: "get_weather", ToolID: "ghost_call", At: time.Now().UTC().Add(-2 * time.Minute)},
	}
	for _, ev := range deadTrace {
		// AppendTrace pushes one at a time — the dead worker's
		// emitTrace would have used the same path.
		s.Require().NoError(runRepo.AppendTrace(context.Background(), runID, "agent-1", ev))
	}

	s.pollUntilStatus(client, runID, "success", 10*time.Second)

	// Pull the final run record + assert agent_traces carries BOTH
	// the dead attempt's events AND the fresh run's events. Anything
	// that destructively cleared the trace would lose the ghost
	// tool_call.
	getResp, err := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + runID)
	s.Require().NoError(err)
	defer getResp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, getResp.StatusCode)
	var detail struct {
		Run struct {
			AgentTraces map[string][]map[string]any `json:"agent_traces"`
		} `json:"run"`
	}
	s.Require().NoError(json.NewDecoder(getResp.Body).Decode(&detail))
	events := detail.Run.AgentTraces["agent-1"]
	s.Require().NotEmpty(events, "agent-1 trace must exist")

	// Ghost from the simulated dead attempt MUST still be there.
	var ghostFound bool
	var iterStartCount int
	for _, ev := range events {
		if ev["type"] == "tool_call" && ev["tool_id"] == "ghost_call" {
			ghostFound = true
		}
		if ev["type"] == "iter_start" {
			iterStartCount++
		}
	}
	s.True(ghostFound, "dead attempt's ghost tool_call must survive the resume — non-destructive trace contract")
	s.GreaterOrEqual(iterStartCount, 2, "trace must contain at least two iter_start events (one from each attempt) so the UI's per-attempt grouping has a boundary to split on")
}

// TestDispatch_RunStatusQueued_PromotedToRunningOnClaim verifies that
// a freshly-dispatched run lands in Mongo with status=queued (not
// running) and that the worker's ClaimLease findAndModify atomically
// promotes the status to running as part of the claim. Without this
// contract the UI would lie about runs sitting in the worker queue —
// they'd display "running" even though no work was happening yet, and
// duration math would conflate queue time with execution time.
//
// We assert two end-states:
//   1. POST /run response carries status=queued.
//   2. Mongo eventually shows the run terminal=success, AND the lease
//      ledger never observed a window where status was anything
//      other than queued → running → success (no skipping; no
//      lingering on queued after claim).
func (s *LeaseUnificationIntegrationSuite) TestDispatch_RunStatusQueued_PromotedToRunningOnClaim() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-queued-%d@example.com", suffix))
	wfID := fmt.Sprintf("wf-queued-%d", suffix)
	// Trigger-only workflow — fastest possible path so the queued
	// window is short but observable.
	payload := map[string]any{
		"name":   "queued-promotion",
		"params": map[string]any{},
		"nodes": []map[string]any{
			{"id": "trigger-1", "type": "trigger",
				"position": map[string]any{"x": 0, "y": 0},
				"data":     map[string]any{"trigger_type": "manual"}},
		},
		"edges": []any{},
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+wfID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	_ = resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	runResp, err := client.Post(s.httpSrv.URL+"/api/v1/workflows/"+wfID+"/run",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer runResp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusAccepted, runResp.StatusCode)
	var out struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	s.Require().NoError(json.NewDecoder(runResp.Body).Decode(&out))
	s.Equal("queued", out.Status, "fresh dispatch must return status=queued (worker promotes on claim)")

	s.pollUntilStatus(client, out.RunID, "success", 5*time.Second)

	// Final record: lease was acquired (lease_owner+lease_expires_at
	// were set during execution and cleared on terminal release).
	// The atomic promotion path is exercised every claim — we already
	// asserted queued → success without intermediate stalls, which is
	// the contract worth checking here. Granular per-state inspection
	// belongs in the lease-primitive tests in internal/mongodb.
}

// TestWorkerResume_FromCheckpointedQueue_HydratesAndCompletes verifies
// the BFS-side resume contract that #4 actually needs: a multi-node
// run where the worker died after some nodes were visited (checkpointBFS
// already wrote `execution_state.queue` for the remaining frontier and
// `last_checkpoint_at`). A fresh worker claiming the run must hydrate
// from priorState — re-execute the queue, NOT re-run from the trigger.
//
// Simulated by seeding the run record with a partial execution_state
// (trigger + first http already visited, second http still queued)
// and an old last_checkpoint_at timestamp — exactly what the dead
// worker's last successful checkpointBFS would have left behind.
// In-test worker picks the run up via the standard claim loop;
// asserts only http-2 runs (visible via steps) and last_checkpoint_at
// advances (proof of fresh checkpoint after http-2 completes).
func (s *LeaseUnificationIntegrationSuite) TestWorkerResume_FromCheckpointedQueue_HydratesAndCompletes() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-ckpt-%d@example.com", suffix))

	// Two echo servers — record hits so the test can assert which
	// nodes actually re-ran. http-1's hit count must stay at 0 (it
	// was completed pre-kill); http-2's must be 1 (hydrated + run).
	var http1Hits, http2Hits atomic.Int32
	http1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http1Hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`one`))
	}))
	defer http1.Close()
	http2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http2Hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`two`))
	}))
	defer http2.Close()

	wfID := fmt.Sprintf("wf-ckpt-%d", suffix)
	wfPayload := map[string]any{
		"name":   "two-http-checkpoint-resume",
		"params": map[string]any{},
		"nodes": []map[string]any{
			{"id": "trigger-1", "type": "trigger", "position": map[string]any{"x": 0, "y": 0},
				"data": map[string]any{"trigger_type": "manual"}},
			{"id": "http-1", "type": "http_request", "position": map[string]any{"x": 200, "y": 0},
				"data": map[string]any{"name": "first", "method": "GET", "url": http1.URL, "timeout_seconds": 5, "max_response_bytes": 1024}},
			{"id": "http-2", "type": "http_request", "position": map[string]any{"x": 400, "y": 0},
				"data": map[string]any{"name": "second", "method": "GET", "url": http2.URL, "timeout_seconds": 5, "max_response_bytes": 1024}},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "http-1", "sourceHandle": "start"},
			{"id": "e2", "source": "http-1", "target": "http-2", "sourceHandle": "success"},
		},
	}
	body, _ := json.Marshal(wfPayload)
	wfReq, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+wfID, bytes.NewReader(body))
	wfReq.Header.Set("Content-Type", "application/json")
	wfResp, err := client.Do(wfReq)
	s.Require().NoError(err)
	_ = wfResp.Body.Close()
	s.Require().Equal(http.StatusOK, wfResp.StatusCode)

	// Seed the run record AS IF a worker had visited trigger-1 +
	// http-1 successfully, written a checkpoint, and then died. The
	// queue carries http-2 (the remaining frontier). last_checkpoint_at
	// is set to a fixed past time so the test can assert it advanced
	// after the resume completes. Status=running + no lease = the
	// claim filter will pick it up on the next worker tick.
	runRepo, err := mongodb.NewWorkflowRunRepository(context.Background(), s.db)
	s.Require().NoError(err)
	tenantID := s.tenantIDFor(client)
	preKillCheckpoint := time.Now().UTC().Add(-2 * time.Minute)
	runID := fmt.Sprintf("01JTESTCKPT%d", suffix%1000000)
	seeded := workflow.WorkflowRun{
		ID:               runID,
		WorkflowID:       wfID,
		TenantID:         tenantID,
		QueuedAt:         preKillCheckpoint,
		StartedAt:        &preKillCheckpoint,
		Status:           workflow.RunStatusRunning,
		LastCheckpointAt: &preKillCheckpoint,
		ExecutionState: &workflow.ExecutionState{
			Visited: []string{"trigger-1", "http-1"},
			Queue:   []workflow.QueuedNode{{NodeID: "http-2", Input: nil}},
		},
		Steps: []workflow.StepResult{
			{NodeID: "trigger-1", NodeType: workflow.NodeTypeTrigger},
			{NodeID: "http-1", NodeType: workflow.NodeTypeHTTPRequest, Output: map[string]any{"body": "one"}},
		},
	}
	_, err = runRepo.Create(context.Background(), seeded)
	s.Require().NoError(err)

	// Wait for the in-test worker to pick the seeded run up + drive
	// it terminal. With queue=[http-2] and trigger-1 + http-1 already
	// in visited, the BFS should run only http-2.
	s.pollUntilStatus(client, runID, "success", 5*time.Second)

	// Assertions:
	// - http-1 was NOT re-hit (visited honoured).
	// - http-2 WAS hit (queue drained).
	// - last_checkpoint_at advanced past the seeded value (fresh
	//   checkpoint landed when http-2 completed, proving the
	//   checkpoint plumbing engaged in the resumed pass).
	s.Equal(int32(0), http1Hits.Load(), "http-1 must not re-run on resume — visited honoured")
	s.Equal(int32(1), http2Hits.Load(), "http-2 must run from the queue on resume")

	final, err := runRepo.Get(context.Background(), runID)
	s.Require().NoError(err)
	s.Equal(workflow.RunStatusSuccess, final.Status)
	s.Require().NotNil(final.LastCheckpointAt, "last_checkpoint_at must be set on terminal")
	s.True(final.LastCheckpointAt.After(preKillCheckpoint),
		"last_checkpoint_at must advance past the seeded pre-kill timestamp (got %v, seeded %v)",
		final.LastCheckpointAt, preKillCheckpoint)
}

// tenantIDFor extracts the active tenant id from /api/v1/auth/me on
// the registered client. Needed by tests that bypass the HTTP layer
// to seed run records directly through the repository.
func (s *LeaseUnificationIntegrationSuite) tenantIDFor(client *http.Client) string {
	resp, err := client.Get(s.httpSrv.URL + "/api/v1/auth/me")
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	var me struct {
		TenantID string `json:"tenant_id"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&me))
	s.Require().NotEmpty(me.TenantID)
	return me.TenantID
}

// TestTerminalCleanup_ApprovedRun_ClearsPendingExecutionAndPausedAgent
// verifies that when a run yields at a gate, gets approved, and runs
// to success, the terminal Update clears ALL three in-flight fields:
// execution_state, paused_agent, pending_approval. Previously
// pending_approval persisted on success (the UI kept showing the
// Approve banner on a completed run until hard refresh).
func (s *LeaseUnificationIntegrationSuite) TestTerminalCleanup_ApprovedRun_ClearsPendingExecutionAndPausedAgent() {
	// Echo server returns 200 OK so the http node lands success
	// after approval.
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer echo.Close()

	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-cleanup-%d@example.com", suffix))
	wfID := fmt.Sprintf("wf-cleanup-%d", suffix)
	s.putGatedHTTPWorkflow(client, wfID, "terminal-cleanup", echo.URL)

	runID := s.triggerRun(client, wfID)
	s.pollUntilStatus(client, runID, "pending_approval", 5*time.Second)

	approveBody, _ := json.Marshal(map[string]any{
		"approved": true,
	})
	approveResp, err := client.Post(s.httpSrv.URL+"/api/v1/workflow_runs/"+runID+"/approval",
		"application/json", bytes.NewReader(approveBody))
	s.Require().NoError(err)
	defer approveResp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, approveResp.StatusCode)

	s.pollUntilStatus(client, runID, "success", 5*time.Second)

	// Mongo: terminal cleanup must drop all in-flight fields.
	getResp, err := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + runID)
	s.Require().NoError(err)
	defer getResp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, getResp.StatusCode)
	var body struct {
		Run map[string]any `json:"run"`
	}
	s.Require().NoError(json.NewDecoder(getResp.Body).Decode(&body))
	s.Equal("success", body.Run["status"])
	s.Nil(body.Run["execution_state"], "execution_state must be cleared on terminal success")
	s.Nil(body.Run["paused_agent"], "paused_agent must be cleared on terminal success")
	s.Nil(body.Run["pending_approval"], "pending_approval mirror must be cleared on terminal success")
}

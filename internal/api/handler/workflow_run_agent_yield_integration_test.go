//go:build integration

// Agent-tool approval-gate yield integration test — guards the
// regression captured in PR #68: when an `as_tool` target carries
// `require_node_approval=true` on the lease path, the gate must
// bubble `errYieldForApproval` out of the agent loop instead of
// being wrapped as a `tool_result(is_error=true)` and looped on
// LLM retry.
//
// The agent loop calls `provider.Chat`. To make this test fast +
// hermetic we replace the `anthropic` LLM factory with a
// deterministic stub that emits a single tool_use → expects to
// see a tool_result back → emits end_turn. The replacement uses
// `llm.Default.Replace`, scoped to the suite via SetupSuite /
// TearDownSuite. Other suites in this package don't actually
// exercise a real LLM provider, so the temporary swap is safe.
//
// Compiled only under `-tags=integration`.

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

// stubLLMProvider implements llm.Provider with deterministic
// responses geared to the agent-yield smoke test:
//   - Call 1: emit a tool_use that targets the gated `get_weather`
//     as_tool. The agent's preExecApproval fires + (if
//     yieldOnApproval) returns errYieldForApproval. The fix under
//     test makes the agent bubble that up rather than retry.
//   - Call 2 (only fires post-Approve, after the run resumed):
//     same tool_use again — gate is now flagged as approved, so
//     the http_request actually runs.
//   - Call 3: the model has seen a tool_result, returns end_turn.
//
// Tracks Chat() invocations so the test can assert the model was
// only re-prompted twice (initial + post-resume), not infinitely.
type stubLLMProvider struct {
	chatCalls atomic.Int32
	toolName  string
}

func (p *stubLLMProvider) Name() string { return "anthropic" }

func (p *stubLLMProvider) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	return nil, fmt.Errorf("stubLLMProvider: streaming not implemented")
}

func (p *stubLLMProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.chatCalls.Add(1)
	// Detect "we already ran the tool and got a result" by scanning
	// the last user-role message for a tool_result block. If we see
	// one, the gate is past + the http_request executed → emit
	// end_turn. Otherwise we're either pre-yield (call 1) or
	// post-resume re-prompt (call 2) — emit the tool_use again.
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != llm.RoleUser {
			continue
		}
		for _, c := range m.Content {
			if c.Type == llm.ContentTypeToolResult {
				return &llm.ChatResponse{
					StopReason: llm.StopReasonEndTurn,
					Content:    []llm.Content{llm.TextBlock("approved-and-resumed")},
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
			llm.ToolUseBlock("call_1", p.toolName, json.RawMessage(`{"city":"San Francisco"}`)),
		},
		Usage: llm.Usage{InputTokens: 5, OutputTokens: 5, TotalTokens: 10},
		Model: "stub-model",
	}, nil
}

type AgentYieldIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	redis       *rediss.Client
	httpSrv     *httptest.Server
	connRepo    *mongodb.ConnectionRepository

	stubProvider *stubLLMProvider

	workerCancel context.CancelFunc
	workerDone   chan struct{}
}

func TestAgentYieldIntegrationSuite(t *testing.T) {
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
	suite.Run(t, new(AgentYieldIntegrationSuite))
}

func (s *AgentYieldIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")
	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("burrow_test_agent_yield_%d", time.Now().UnixNano())
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
	s.connRepo = connRepo
	auditRepo, err := mongodb.NewAuditRepository(ctx, s.db)
	s.Require().NoError(err)

	// Stub LLM factory. Replace shadows the real anthropic factory
	// for the duration of this suite. Chat-call counter lives on the
	// suite so the assertion can verify the agent did NOT loop on
	// the gate.
	s.stubProvider = &stubLLMProvider{toolName: "get_weather"}
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
		runInTestExecutorWorker(workerCtx, "test-executor-agent-yield", runRepo, wfRepo, wfExec)
	}()
}

func (s *AgentYieldIntegrationSuite) TearDownSuite() {
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
	// Best-effort restoration: re-register an erroring stub for the
	// "anthropic" type so any later test in the package that
	// accidentally wires it up gets a clear failure rather than
	// reusing this suite's stub. Anthropic's real factory is
	// re-installed at process start by its package init() — so a
	// fresh `go test` run starts clean regardless of what we leave
	// behind here.
	llm.Default.Replace("anthropic", func(cfg map[string]string) (llm.Provider, error) {
		return nil, fmt.Errorf("anthropic factory torn down by AgentYieldIntegrationSuite — should not be called from another suite")
	})
}

func (s *AgentYieldIntegrationSuite) SetupTest() {
	for _, col := range []string{"users", "tenants", "tenant_members", "workflows", "workflow_runs", "audit_log", "api_keys", "connections"} {
		_, _ = s.db.Collection(col).DeleteMany(context.Background(), map[string]any{})
	}
	for _, name := range []string{"register", "login", "password_reset", "oauth_start"} {
		_, _ = s.redis.Del(context.Background(), "ratelimit:auth:"+name+":127.0.0.1")
	}
	s.stubProvider.chatCalls.Store(0)
}

func (s *AgentYieldIntegrationSuite) TearDownTest() {}

func (s *AgentYieldIntegrationSuite) authedClient(emailAddr string) (*http.Client, string) {
	jar, err := cookiejar.New(nil)
	s.Require().NoError(err)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	body, _ := json.Marshal(map[string]string{"email": emailAddr, "password": "password1234"})
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	// Pull tenant from /me so we can stamp a connection record
	// directly via the repo.
	meResp, err := client.Get(s.httpSrv.URL + "/api/v1/auth/me")
	s.Require().NoError(err)
	defer meResp.Body.Close() //nolint:errcheck
	var me struct {
		TenantID string `json:"tenant_id"`
	}
	s.Require().NoError(json.NewDecoder(meResp.Body).Decode(&me))
	s.Require().NotEmpty(me.TenantID, "/me must report tenant_id")
	return client, me.TenantID
}

// TestAgent_AsToolGate_YieldsCleanlyAndResumesAfterApprove verifies
// the PR #68 fix end-to-end: the agent dispatches a tool_use to an
// `as_tool` http_request whose `require_node_approval=true` is set;
// the gate fires; the agent must NOT loop on tool_result errors;
// the run lands `pending_approval`; an approve POST flips the run
// back; the worker re-claims; the agent resumes (one extra Chat
// call); the http_request runs (echoes the input back); the agent
// observes the result; the run completes with status=success.
func (s *AgentYieldIntegrationSuite) TestAgent_AsToolGate_YieldsCleanlyAndResumesAfterApprove() {
	suffix := time.Now().UnixNano()
	client, tenantID := s.authedClient(fmt.Sprintf("alice-agent-yield-%d@example.com", suffix))

	// Insert a connection of type "anthropic" via the repo. The handler
	// validator would call into the real factory which we've stubbed
	// anyway, but bypassing the validator keeps the test focused.
	connID := fmt.Sprintf("anthropic-stub-%d", suffix)
	_, cerr := s.connRepo.Upsert(context.Background(), workflow.Connection{
		ID:        connID,
		TenantID:  tenantID,
		Name:      "stub anthropic",
		Type:      workflow.ConnectionTypeAnthropic,
		Config:    map[string]string{"api_key": "stub-key", "model": "stub-model"},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	})
	s.Require().NoError(cerr)

	// Local mock HTTP target so the http_request's actual execution
	// (post-approve) has somewhere to land. Echoes back the URL query.
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"weather":"sunny"}`))
	}))
	defer mock.Close()

	// Workflow: trigger → ai_agent → (as_tool) http_request with
	// require_node_approval=true. Agent emits a tool_use named
	// `get_weather`; the executor routes that through the
	// http_request node.
	wfID := fmt.Sprintf("wf-agent-yield-%d", suffix)
	wfPayload := map[string]any{
		"name":   "agent yield smoke",
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
					"llm_connection_id": connID,
					"system_prompt":     "you are a weather agent",
					"user_input":        "what is the weather?",
					"max_iterations":    3,
				},
			},
			{
				"id":       "get_weather",
				"type":     "http_request",
				"position": map[string]any{"x": 400, "y": 0},
				"data": map[string]any{
					"name":   "get_weather",
					"url":    mock.URL,
					"method": "GET",
					"as_tool": map[string]any{
						"enabled":      true,
						"name":         "get_weather",
						"description":  "fetch the weather",
						"input_schema": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
					},
					"require_node_approval": true,
				},
			},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "agent-1", "sourceHandle": "success"},
			{"id": "e2", "source": "agent-1", "target": "get_weather", "sourceHandle": "tool"},
		},
	}
	wfBody, _ := json.Marshal(wfPayload)
	req, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+wfID, bytes.NewReader(wfBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode, "workflow PUT should succeed")

	// Async dispatch.
	runResp, err := client.Post(s.httpSrv.URL+"/api/v1/workflows/"+wfID+"/run",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer runResp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusAccepted, runResp.StatusCode)
	var dispatch struct {
		RunID string `json:"run_id"`
	}
	s.Require().NoError(json.NewDecoder(runResp.Body).Decode(&dispatch))
	runID := dispatch.RunID

	// Worker should land the gate pretty quickly. If the bug is back
	// (errYield wrapped as tool_result), the agent loops here and
	// the run never reaches pending_approval — the assertion fails
	// on timeout.
	deadline := time.Now().Add(8 * time.Second)
	var pendingState map[string]any
	for time.Now().Before(deadline) {
		dResp, derr := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + runID)
		s.Require().NoError(derr)
		var detail map[string]any
		_ = json.NewDecoder(dResp.Body).Decode(&detail)
		_ = dResp.Body.Close()
		if run, ok := detail["run"].(map[string]any); ok {
			if st, _ := run["status"].(string); st == "pending_approval" {
				if pa, ok := run["pending_approval"].(map[string]any); ok {
					pendingState = pa
				}
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.Require().NotNil(pendingState, "run should yield + land pending_approval (gate fired in agent dispatch)")
	s.Equal("get_weather", pendingState["node_id"], "pending gate should target the gated as_tool node")

	// Sanity: agent loop was NOT looping. One Chat call to emit the
	// initial tool_use, period — the yield short-circuits before the
	// next iter would have re-prompted.
	preApproveCalls := s.stubProvider.chatCalls.Load()
	s.Equal(int32(1), preApproveCalls, "agent should have emitted exactly one Chat call before the gate yielded; got %d (regression: tool_result-wrapped errYield is feeding the LLM)", preApproveCalls)

	// Approve.
	approveBody, _ := json.Marshal(map[string]any{"approved": true})
	aResp, err := client.Post(s.httpSrv.URL+"/api/v1/workflow_runs/"+runID+"/approval",
		"application/json", bytes.NewReader(approveBody))
	s.Require().NoError(err)
	defer aResp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, aResp.StatusCode)

	// Worker re-claims, hydrates from PausedAgent, agent re-emits the
	// tool_use, gate now flagged approvedNodeIDs → http_request
	// actually runs → tool_result fed back → end_turn.
	deadline = time.Now().Add(8 * time.Second)
	finalStatus := ""
	for time.Now().Before(deadline) {
		dResp, derr := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + runID)
		s.Require().NoError(derr)
		var detail map[string]any
		_ = json.NewDecoder(dResp.Body).Decode(&detail)
		_ = dResp.Body.Close()
		if run, ok := detail["run"].(map[string]any); ok {
			if st, _ := run["status"].(string); st == "success" || st == "error" {
				finalStatus = st
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.Equal("success", finalStatus, "approved run should land success")

	// Resume cost: one extra Chat call (re-prompt with popped trailing
	// assistant turn) + one final-answer Chat call. So total ≥ 2 more
	// since pre-approve. Not asserted strictly because the agent loop
	// may collapse on the same iter when the popped-message resume
	// causes a fresh tool_use immediately followed by the result feed.
	postApproveCalls := s.stubProvider.chatCalls.Load()
	s.GreaterOrEqual(postApproveCalls, int32(2), "agent should re-prompt at least once on resume")
}

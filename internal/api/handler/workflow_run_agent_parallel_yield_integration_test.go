//go:build integration

// Parallel-tool-use approval-gate replay test — guards the PR #72
// fix for the infinite-approve-loop regression.
//
// Repro shape: agent emits TWO parallel `tool_use` blocks in one
// assistant turn (`get_weather` + `format_weather`); both targets
// have `require_node_approval=true`. Pre-fix, after approving the
// first gate, resume re-prompted the model, model re-emitted the
// SAME parallel tool_uses, the now-unflagged earlier tool fired
// the gate again → infinite cycle.
//
// Post-fix, the agent's PausedAgent snapshots the partial-dispatch
// state (toolCalls + already-collected results + the index of the
// gated call); resume skips Chat for that iter and continues
// dispatch from PartialNextIndex. Each tool gates exactly once.

package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// parallelToolStubProvider emits TWO tool_uses in one assistant turn
// (call_get + call_format) when there's no tool_result in the message
// stack yet, and switches to end_turn once both results have flowed
// back. Tracks chatCalls so the test can assert that resume after
// each gate did NOT re-prompt the model.
type parallelToolStubProvider struct {
	chatCalls atomic.Int32
}

func (p *parallelToolStubProvider) Name() string { return "anthropic" }

func (p *parallelToolStubProvider) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	return nil, errors.New("parallelToolStubProvider: streaming not implemented")
}

func (p *parallelToolStubProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.chatCalls.Add(1)
	// Count tool_results across the whole conversation. Once we see
	// two (one per dispatched tool), the model is "satisfied" and
	// emits end_turn. Until then, emit the parallel tool_use pair.
	resultCount := 0
	for _, m := range req.Messages {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, c := range m.Content {
			if c.Type == llm.ContentTypeToolResult {
				resultCount++
			}
		}
	}
	if resultCount >= 2 {
		return &llm.ChatResponse{
			StopReason: llm.StopReasonEndTurn,
			Content:    []llm.Content{llm.TextBlock("done — both tools observed")},
			Usage:      llm.Usage{InputTokens: 5, OutputTokens: 5, TotalTokens: 10},
			Model:      "stub-model",
		}, nil
	}
	return &llm.ChatResponse{
		StopReason: llm.StopReasonToolUse,
		Content: []llm.Content{
			llm.ToolUseBlock("call_get", "get_weather", json.RawMessage(`{"city":"San Francisco"}`)),
			llm.ToolUseBlock("call_format", "format_weather", json.RawMessage(`{"data":{}}`)),
		},
		Usage: llm.Usage{InputTokens: 5, OutputTokens: 5, TotalTokens: 10},
		Model: "stub-model",
	}, nil
}

type AgentParallelYieldIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	redis       *rediss.Client
	httpSrv     *httptest.Server
	connRepo    *mongodb.ConnectionRepository

	stubProvider *parallelToolStubProvider

	workerCancel context.CancelFunc
	workerDone   chan struct{}
}

func TestAgentParallelYieldIntegrationSuite(t *testing.T) {
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
	suite.Run(t, new(AgentParallelYieldIntegrationSuite))
}

func (s *AgentParallelYieldIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")
	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("burrow_test_agent_parallel_yield_%d", time.Now().UnixNano())
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

	s.stubProvider = &parallelToolStubProvider{}
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
		runInTestExecutorWorker(workerCtx, "test-executor-parallel-yield", runRepo, wfRepo, wfExec)
	}()
}

func (s *AgentParallelYieldIntegrationSuite) TearDownSuite() {
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
	llm.Default.Replace("anthropic", func(cfg map[string]string) (llm.Provider, error) {
		return nil, fmt.Errorf("anthropic factory torn down by AgentParallelYieldIntegrationSuite — should not be called from another suite")
	})
}

func (s *AgentParallelYieldIntegrationSuite) SetupTest() {
	for _, col := range []string{"users", "tenants", "tenant_members", "workflows", "workflow_runs", "audit_log", "api_keys", "connections"} {
		_, _ = s.db.Collection(col).DeleteMany(context.Background(), map[string]any{})
	}
	for _, name := range []string{"register", "login", "password_reset", "oauth_start"} {
		_, _ = s.redis.Del(context.Background(), "ratelimit:auth:"+name+":127.0.0.1")
	}
	s.stubProvider.chatCalls.Store(0)
}

func (s *AgentParallelYieldIntegrationSuite) TearDownTest() {}

func (s *AgentParallelYieldIntegrationSuite) authedClient(emailAddr string) (*http.Client, string) {
	jar, err := cookiejar.New(nil)
	s.Require().NoError(err)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	body, _ := json.Marshal(map[string]string{"email": emailAddr, "password": "password1234"})
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	meResp, err := client.Get(s.httpSrv.URL + "/api/v1/auth/me")
	s.Require().NoError(err)
	defer meResp.Body.Close() //nolint:errcheck
	var me struct {
		TenantID string `json:"tenant_id"`
	}
	s.Require().NoError(json.NewDecoder(meResp.Body).Decode(&me))
	return client, me.TenantID
}

// pollForPendingApproval polls the run-detail endpoint until status
// flips to pending_approval; returns the gated tool name. Fails the
// test on timeout.
func (s *AgentParallelYieldIntegrationSuite) pollForPendingApproval(client *http.Client, runID string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		dResp, err := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + runID)
		s.Require().NoError(err)
		var detail map[string]any
		_ = json.NewDecoder(dResp.Body).Decode(&detail)
		_ = dResp.Body.Close()
		if run, ok := detail["run"].(map[string]any); ok {
			if st, _ := run["status"].(string); st == "pending_approval" {
				if pa, ok := run["pending_approval"].(map[string]any); ok {
					if name, _ := pa["node_id"].(string); name != "" {
						return name
					}
				}
				return ""
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.FailNow("pollForPendingApproval timeout", "run %s never reached pending_approval", runID)
	return ""
}

// pollForStatus polls until the run lands one of the terminal states
// or timeout elapses; returns the final status.
func (s *AgentParallelYieldIntegrationSuite) pollForStatus(client *http.Client, runID string, terminal []string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		dResp, err := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + runID)
		s.Require().NoError(err)
		var detail map[string]any
		_ = json.NewDecoder(dResp.Body).Decode(&detail)
		_ = dResp.Body.Close()
		if run, ok := detail["run"].(map[string]any); ok {
			if st, _ := run["status"].(string); st != "" {
				last = st
				for _, t := range terminal {
					if st == t {
						return st
					}
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.FailNow("pollForStatus timeout", "run %s never reached terminal in %s (last=%q)", runID, timeout, last)
	return last
}

// TestAgent_ParallelToolUse_TwoGates_NoLoopAndCompletes verifies the
// PR #72 fix end-to-end. Stub LLM emits two parallel tool_uses
// (`get_weather` + `format_weather`), both targets carry
// `require_node_approval=true`. Sequence under the fix:
//
//  1. POST /run → 202 + run_id.
//  2. Worker claims, BFS dispatches agent. Agent loop iter 0:
//     stub.Chat → emits BOTH tool_uses (chatCalls=1). Dispatch
//     loop hits get_weather first → preExecApproval yields →
//     bubbles up → BFS yields lease. Run lands pending_approval
//     for get_weather. Critical: PartialToolCalls / Results /
//     NextIndex saved on PausedAgent.
//  3. Approve get_weather → /approval handler routes through
//     ApplyApprovalDecision + wakeup.
//  4. Worker re-claims. Hydrates priorState.Pending → sets
//     env.approvedNodeIDs[get_weather]=true. Hydrates
//     PausedAgent.PartialToolCalls. Agent loop iter 0:
//     partialResumeActive=true → SKIPS provider.Chat (chatCalls
//     stays at 1). Dispatch loop continues from index 0 →
//     get_weather has flag → consumed → http_request runs OK →
//     index 1 is format_weather → preExecApproval yields →
//     PausedAgent now updated with partialNextIndex=1 +
//     get_weather's result already in PartialToolResults.
//     Run lands pending_approval for format_weather.
//  5. Approve format_weather. Worker re-claims.
//     env.approvedNodeIDs[format_weather]=true. PartialToolCalls
//     hydrated. Loop iter 0: SKIPS Chat (chatCalls still 1).
//     Dispatch from index 1 → format_weather consumed → runs.
//     All toolCalls done; messages.append(tool_results); continue.
//  6. Iter 1: stub.Chat sees 2 tool_results → emits end_turn
//     (chatCalls=2). Run completes status=success.
//
// The chatCalls assertion (==2 at the end, never 3+) is the
// regression guard: pre-fix, every gate caused a re-prompt and
// the count balloon would have hit 3+ with the run still cycling.
func (s *AgentParallelYieldIntegrationSuite) TestAgent_ParallelToolUse_TwoGates_NoLoopAndCompletes() {
	suffix := time.Now().UnixNano()
	client, tenantID := s.authedClient(fmt.Sprintf("alice-parallel-%d@example.com", suffix))

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

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer mock.Close()

	wfID := fmt.Sprintf("wf-parallel-%d", suffix)
	wfPayload := map[string]any{
		"name":   "parallel tool yield",
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
					"system_prompt":     "you are a parallel-tool-call agent",
					"user_input":        "fetch + format the weather",
					"max_iterations":    3,
				},
			},
			{
				"id":       "get_weather",
				"type":     "http_request",
				"position": map[string]any{"x": 400, "y": -50},
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
			{
				"id":       "format_weather",
				"type":     "http_request",
				"position": map[string]any{"x": 400, "y": 50},
				"data": map[string]any{
					"name":   "format_weather",
					"url":    mock.URL,
					"method": "POST",
					"as_tool": map[string]any{
						"enabled":      true,
						"name":         "format_weather",
						"description":  "format the weather",
						"input_schema": map[string]any{"type": "object"},
					},
					"require_node_approval": true,
				},
			},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "agent-1", "sourceHandle": "success"},
			{"id": "e2", "source": "agent-1", "target": "get_weather", "sourceHandle": "tool"},
			{"id": "e3", "source": "agent-1", "target": "format_weather", "sourceHandle": "tool"},
		},
	}
	wfBody, _ := json.Marshal(wfPayload)
	req, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+wfID, bytes.NewReader(wfBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)

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

	// Gate #1: get_weather (model emits this first in the parallel
	// pair).
	gated1 := s.pollForPendingApproval(client, runID, 8*time.Second)
	s.Equal("get_weather", gated1, "first parallel tool_use should gate on get_weather")
	s.Equal(int32(1), s.stubProvider.chatCalls.Load(), "agent should have emitted exactly one Chat call before the first gate yielded")

	// Approve.
	approveBody, _ := json.Marshal(map[string]any{"approved": true})
	aResp, err := client.Post(s.httpSrv.URL+"/api/v1/workflow_runs/"+runID+"/approval",
		"application/json", bytes.NewReader(approveBody))
	s.Require().NoError(err)
	defer aResp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, aResp.StatusCode)

	// Gate #2: format_weather. Critical regression check: stubProvider
	// must NOT have been re-prompted between gate #1 and gate #2.
	// Pre-fix the count would balloon as the model re-emitted both
	// tool_uses every resume.
	gated2 := s.pollForPendingApproval(client, runID, 8*time.Second)
	s.Equal("format_weather", gated2, "second tool_use should gate on format_weather (partial-replay continued the dispatch)")
	s.Equal(int32(1), s.stubProvider.chatCalls.Load(), "no re-prompt between gates — partial-replay must skip Chat on resume")

	// Approve format_weather.
	aResp2, err := client.Post(s.httpSrv.URL+"/api/v1/workflow_runs/"+runID+"/approval",
		"application/json", bytes.NewReader(approveBody))
	s.Require().NoError(err)
	defer aResp2.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, aResp2.StatusCode)

	// Run completes. Stub emits end_turn on iter 1's Chat call (the
	// model has now seen both tool_results), so chatCalls bumps to 2.
	finalStatus := s.pollForStatus(client, runID, []string{"success", "error"}, 8*time.Second)
	s.Equal("success", finalStatus)
	s.Equal(int32(2), s.stubProvider.chatCalls.Load(), "exactly two Chat calls total: initial parallel emit + post-resume end_turn. Anything higher = regression to the loop bug")
}

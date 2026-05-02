//go:build integration

// Sequential 3-tool nested-gate replay test — guards the PR #73
// fix for the per-tool ↔ node-level gate ping-pong.
//
// Mirrors the bundled AI Weather Agent template:
//
//	trigger → ai_agent → as_tool: get_weather (http_request)
//	                  → as_tool: format_weather (http_request)
//	                  → as_tool: publish_weather (redis_request stand-in / http_request here)
//
// Agent has BOTH gates on:
//   - `require_approval=true` (per-tool gate fires on every tool_call
//     inside the agent loop).
//   - `require_node_approval=true` on each as_tool target node (the
//     pre-exec node-level gate fires INSIDE catalog.Execute).
//
// Each tool_call therefore yields TWICE:
//   1. Per-tool gate yields → approve → resume
//   2. Resume hydration restores per-tool flag; per-tool consume is
//      now sticky (no delete) so the second yield's resume can still
//      see it. catalog.Execute → preExecApproval (node-level) yields
//      → approve → resume → both flags consumed → tool runs.
//
// Pre-fix: per-tool flag was deleted on consume → second yield's
// resume re-fired per-tool gate → infinite ping-pong between the
// two gates, never advancing.
//
// Test asserts: each tool gates exactly TWICE (per-tool then node-
// level), six approval clicks total across three tools, run
// completes status=success after the agent emits end_turn.

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

// nestedToolStubProvider walks 3 tools sequentially:
//   - Iter 0: emit tool_use(get_weather)
//   - Iter 1: emit tool_use(format_weather)
//   - Iter 2: emit tool_use(publish_weather)
//   - Iter 3: end_turn
//
// Decision based on count of tool_results in the conversation: 0
// → get_weather; 1 → format_weather; 2 → publish_weather; 3+ →
// end_turn. Matches the AI Weather Agent template's prescribed
// procedure.
type nestedToolStubProvider struct {
	chatCalls atomic.Int32
}

func (p *nestedToolStubProvider) Name() string { return "anthropic" }

func (p *nestedToolStubProvider) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	return nil, errors.New("nestedToolStubProvider: streaming not implemented")
}

func (p *nestedToolStubProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.chatCalls.Add(1)
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
	switch resultCount {
	case 0:
		return &llm.ChatResponse{
			StopReason: llm.StopReasonToolUse,
			Content: []llm.Content{
				llm.ToolUseBlock("call_get", "get_weather", json.RawMessage(`{"city":"San Francisco"}`)),
			},
			Usage: llm.Usage{InputTokens: 5, OutputTokens: 5, TotalTokens: 10},
			Model: "stub-model",
		}, nil
	case 1:
		return &llm.ChatResponse{
			StopReason: llm.StopReasonToolUse,
			Content: []llm.Content{
				llm.ToolUseBlock("call_format", "format_weather", json.RawMessage(`{"data":{}}`)),
			},
			Usage: llm.Usage{InputTokens: 5, OutputTokens: 5, TotalTokens: 10},
			Model: "stub-model",
		}, nil
	case 2:
		return &llm.ChatResponse{
			StopReason: llm.StopReasonToolUse,
			Content: []llm.Content{
				llm.ToolUseBlock("call_publish", "publish_weather", json.RawMessage(`{"summary":"sunny"}`)),
			},
			Usage: llm.Usage{InputTokens: 5, OutputTokens: 5, TotalTokens: 10},
			Model: "stub-model",
		}, nil
	default:
		return &llm.ChatResponse{
			StopReason: llm.StopReasonEndTurn,
			Content:    []llm.Content{llm.TextBlock("done — three tools observed")},
			Usage:      llm.Usage{InputTokens: 5, OutputTokens: 5, TotalTokens: 10},
			Model:      "stub-model",
		}, nil
	}
}

type AgentNestedYieldIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	redis       *rediss.Client
	httpSrv     *httptest.Server
	connRepo    *mongodb.ConnectionRepository

	stubProvider *nestedToolStubProvider

	workerCancel context.CancelFunc
	workerDone   chan struct{}
}

func TestAgentNestedYieldIntegrationSuite(t *testing.T) {
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
	suite.Run(t, new(AgentNestedYieldIntegrationSuite))
}

func (s *AgentNestedYieldIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")
	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("burrow_test_agent_nested_yield_%d", time.Now().UnixNano())
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

	s.stubProvider = &nestedToolStubProvider{}
	llm.Default.Replace("anthropic", func(cfg map[string]string) (llm.Provider, error) {
		return s.stubProvider, nil
	})

	connResolver := workflow.NewConnectionResolver(connRepo, mongodb.NewMongoClient(s.db), nil)
	notifier := &workflow.MultiplexApprovalNotifier{
		HTTPClient:   &http.Client{Timeout: 5 * time.Second},
		ConnResolver: connResolver,
	}

	wfExec := &workflow.WorkflowExecutor{
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
		runInTestExecutorWorker(workerCtx, "test-executor-nested-yield", runRepo, wfRepo, wfExec)
	}()
}

func (s *AgentNestedYieldIntegrationSuite) TearDownSuite() {
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
		return nil, fmt.Errorf("anthropic factory torn down by AgentNestedYieldIntegrationSuite — should not be called from another suite")
	})
}

func (s *AgentNestedYieldIntegrationSuite) SetupTest() {
	for _, col := range []string{"users", "tenants", "tenant_members", "workflows", "workflow_runs", "audit_log", "api_keys", "connections"} {
		_, _ = s.db.Collection(col).DeleteMany(context.Background(), map[string]any{})
	}
	for _, name := range []string{"register", "login", "password_reset", "oauth_start"} {
		_, _ = s.redis.Del(context.Background(), "ratelimit:auth:"+name+":127.0.0.1")
	}
	s.stubProvider.chatCalls.Store(0)
}

func (s *AgentNestedYieldIntegrationSuite) TearDownTest() {}

func (s *AgentNestedYieldIntegrationSuite) authedClient(emailAddr string) (*http.Client, string) {
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

// pollForPending polls until the run is in pending_approval; returns
// the kind ("tool_call" or "node") + the gate's identifier (tool name
// for tool_call, node id for node).
func (s *AgentNestedYieldIntegrationSuite) pollForPending(client *http.Client, runID string, timeout time.Duration) (string, string) {
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
					kind, _ := pa["kind"].(string)
					var ident string
					if kind == "tool_call" {
						ident, _ = pa["tool_name"].(string)
					} else {
						ident, _ = pa["node_id"].(string)
					}
					if ident != "" {
						return kind, ident
					}
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.FailNow("pollForPending timeout", "run %s never reached pending_approval", runID)
	return "", ""
}

// approve clicks Approve via the cookie-authed handler.
func (s *AgentNestedYieldIntegrationSuite) approve(client *http.Client, runID string) {
	body, _ := json.Marshal(map[string]any{"approved": true})
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/workflow_runs/"+runID+"/approval",
		"application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
}

// pollForStatus waits for the run to land any of `terminal`.
func (s *AgentNestedYieldIntegrationSuite) pollForStatus(client *http.Client, runID string, terminal []string, timeout time.Duration) string {
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

// TestAgent_NestedGates_3Tools_NoLoopAndCompletes is the regression
// guard for the bundled AI Weather Agent template.
//
// Both `require_approval` (agent) and `require_node_approval` (each
// as_tool target) are on, so each of the 3 sequential tool_calls
// gates twice: per-tool first, then node-level inside catalog.Execute.
//
// Total = 6 approve clicks. Pre-fix the run looped because per-tool
// flag was consumed on the first resume + lost by the second yield.
// Post-fix the per-tool flag is sticky across the cascade and gets
// cleared at iter-end.
//
// Chat-call count = 4 across the run: 1 per fresh iter (model
// emits the next tool_use). Each yield resume's partial-replay
// skips Chat → so re-prompts happen only when iter advances.
func (s *AgentNestedYieldIntegrationSuite) TestAgent_NestedGates_3Tools_NoLoopAndCompletes() {
	suffix := time.Now().UnixNano()
	client, tenantID := s.authedClient(fmt.Sprintf("alice-nested-%d@example.com", suffix))

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

	mkAsTool := func(id, name string) map[string]any {
		return map[string]any{
			"id":       id,
			"type":     "http_request",
			"position": map[string]any{"x": 600, "y": 0},
			"data": map[string]any{
				"name":   name,
				"url":    mock.URL,
				"method": "GET",
				"as_tool": map[string]any{
					"enabled":      true,
					"name":         name,
					"description":  name,
					"input_schema": map[string]any{"type": "object"},
				},
				"require_node_approval": true,
			},
		}
	}

	wfID := fmt.Sprintf("wf-nested-%d", suffix)
	wfPayload := map[string]any{
		"name":   "nested-gates 3-tool",
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
					"llm_connection_id":      connID,
					"system_prompt":          "you are a 3-tool sequential agent",
					"user_input":             "fetch, format, publish",
					"max_iterations":         6,
					"max_tool_calls_per_iter": 2,
					"require_approval":       true,
				},
			},
			mkAsTool("get_weather", "get_weather"),
			mkAsTool("format_weather", "format_weather"),
			mkAsTool("publish_weather", "publish_weather"),
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "agent-1", "sourceHandle": "success"},
			{"id": "e2", "source": "agent-1", "target": "get_weather", "sourceHandle": "tool"},
			{"id": "e3", "source": "agent-1", "target": "format_weather", "sourceHandle": "tool"},
			{"id": "e4", "source": "agent-1", "target": "publish_weather", "sourceHandle": "tool"},
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

	// Three tools, each gated twice (per-tool then node-level).
	// Walk all six gates; assert kind + identifier sequence.
	expected := []struct {
		kind  string
		ident string
	}{
		{"tool_call", "get_weather"},
		{"node", "get_weather"},
		{"tool_call", "format_weather"},
		{"node", "format_weather"},
		{"tool_call", "publish_weather"},
		{"node", "publish_weather"},
	}

	for i, want := range expected {
		kind, ident := s.pollForPending(client, runID, 8*time.Second)
		s.Equal(want.kind, kind, "gate #%d: expected kind=%q got %q (ident=%q)", i+1, want.kind, kind, ident)
		s.Equal(want.ident, ident, "gate #%d: expected ident=%q got %q", i+1, want.ident, ident)
		s.approve(client, runID)
	}

	finalStatus := s.pollForStatus(client, runID, []string{"success", "error"}, 8*time.Second)
	s.Equal("success", finalStatus, "all 6 gates approved → run should land success")

	// Chat call accounting:
	//   - Initial: stub.Chat → tool_use(get_weather)        [1]
	//   - After both gates on get_weather + tool_result fed: stub.Chat → tool_use(format_weather) [2]
	//   - After both gates on format_weather: stub.Chat → tool_use(publish_weather) [3]
	//   - After both gates on publish_weather: stub.Chat → end_turn [4]
	// Pre-fix the loop would balloon this past 4 with the run never
	// completing.
	s.Equal(int32(4), s.stubProvider.chatCalls.Load(),
		"exactly 4 Chat calls (one per fresh iter); higher count = regression to per-tool ↔ node-level loop")
}

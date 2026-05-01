//go:build integration

// Agent tool-error stop-mode integration tests — guards the
// `stop_on_tool_error` flag + the sandbox-infra force-stop path.
//
// Three flavours:
//   - stop_on_tool_error=true → first tool error aborts the agent →
//     run lands status=error (matches user expectation: "error
//     should stop the workflow + show error on the badge").
//   - stop_on_tool_error=false + non-infra tool error → LLM gets the
//     tool_result(is_error=true) and pivots to end_turn → run lands
//     status=success (legacy behaviour preserved for resilient
//     agents that wrap flaky upstream APIs).
//   - stop_on_tool_error=false + sandbox/* prefixed tool error →
//     forced terminal regardless of flag because the LLM has no
//     plausible recovery from infra failures.
//
// Compiled only under `-tags=integration`.

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
	"sync"
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

// toolErrorStubProvider emits a tool_use unless the most-recent user
// message carries a tool_result block — at which point it switches
// to end_turn. Lets the same stub serve the "agent retried
// successfully" case (call 2 sees the error tool_result and pivots)
// AND the "agent aborted" case (call 1 only).
type toolErrorStubProvider struct {
	chatCalls atomic.Int32
	toolName  string
}

func (p *toolErrorStubProvider) Name() string { return "anthropic" }

func (p *toolErrorStubProvider) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.ChatEvent, error) {
	return nil, errors.New("toolErrorStubProvider: streaming not implemented")
}

func (p *toolErrorStubProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.chatCalls.Add(1)
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != llm.RoleUser {
			continue
		}
		for _, c := range m.Content {
			if c.Type == llm.ContentTypeToolResult {
				return &llm.ChatResponse{
					StopReason: llm.StopReasonEndTurn,
					Content:    []llm.Content{llm.TextBlock("ok, gave up after the tool error")},
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
			llm.ToolUseBlock("call_1", p.toolName, json.RawMessage(`{}`)),
		},
		Usage: llm.Usage{InputTokens: 5, OutputTokens: 5, TotalTokens: 10},
		Model: "stub-model",
	}, nil
}

type AgentToolErrorIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	redis       *rediss.Client
	httpSrv     *httptest.Server
	connRepo    *mongodb.ConnectionRepository

	stubProvider *toolErrorStubProvider

	// Mock target for as_tool http_requests. Per-test handler swap
	// via mockHandler so each scenario can return different status.
	mockSrv     *httptest.Server
	mockHandler http.HandlerFunc
	mockMu      sync.RWMutex

	workerCancel context.CancelFunc
	workerDone   chan struct{}
}

func TestAgentToolErrorIntegrationSuite(t *testing.T) {
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
	suite.Run(t, new(AgentToolErrorIntegrationSuite))
}

func (s *AgentToolErrorIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")
	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("burrow_test_agent_tool_err_%d", time.Now().UnixNano())
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

	s.stubProvider = &toolErrorStubProvider{toolName: "fail_tool"}
	llm.Default.Replace("anthropic", func(cfg map[string]string) (llm.Provider, error) {
		return s.stubProvider, nil
	})

	// Mock HTTP target — handler swappable per test.
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
		runInTestExecutorWorker(workerCtx, "test-executor-tool-err", runRepo, wfRepo, wfExec)
	}()
}

func (s *AgentToolErrorIntegrationSuite) TearDownSuite() {
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
	llm.Default.Replace("anthropic", func(cfg map[string]string) (llm.Provider, error) {
		return nil, fmt.Errorf("anthropic factory torn down by AgentToolErrorIntegrationSuite — should not be called from another suite")
	})
}

func (s *AgentToolErrorIntegrationSuite) SetupTest() {
	for _, col := range []string{"users", "tenants", "tenant_members", "workflows", "workflow_runs", "audit_log", "api_keys", "connections"} {
		_, _ = s.db.Collection(col).DeleteMany(context.Background(), map[string]any{})
	}
	for _, name := range []string{"register", "login", "password_reset", "oauth_start"} {
		_, _ = s.redis.Del(context.Background(), "ratelimit:auth:"+name+":127.0.0.1")
	}
	s.stubProvider.chatCalls.Store(0)
	s.mockMu.Lock()
	s.mockHandler = nil
	s.mockMu.Unlock()
}

func (s *AgentToolErrorIntegrationSuite) TearDownTest() {}

func (s *AgentToolErrorIntegrationSuite) authedClient(emailAddr string) (*http.Client, string) {
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

// putAgentToolWorkflow PUTs a 3-node workflow:
//
//	trigger → ai_agent → http_request (`as_tool`, name=fail_tool)
//
// `stopOnToolError` toggles the new flag. Returns the workflow ID.
func (s *AgentToolErrorIntegrationSuite) putAgentToolWorkflow(client *http.Client, tenantID string, stopOnToolError bool) (wfID, connID string) {
	suffix := time.Now().UnixNano()
	connID = fmt.Sprintf("anthropic-toolerr-%d", suffix)
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

	wfID = fmt.Sprintf("wf-toolerr-%d", suffix)
	wfPayload := map[string]any{
		"name":   "tool error stop mode",
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
					"llm_connection_id":  connID,
					"system_prompt":      "you are a test agent",
					"user_input":         "do the thing",
					"max_iterations":     3,
					"stop_on_tool_error": stopOnToolError,
				},
			},
			{
				"id":       "fail_tool",
				"type":     "http_request",
				"position": map[string]any{"x": 400, "y": 0},
				"data": map[string]any{
					"name":   "fail_tool",
					"url":    s.mockSrv.URL,
					"method": "GET",
					"as_tool": map[string]any{
						"enabled":      true,
						"name":         "fail_tool",
						"description":  "always fails",
						"input_schema": map[string]any{"type": "object"},
					},
				},
			},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "agent-1", "sourceHandle": "success"},
			{"id": "e2", "source": "agent-1", "target": "fail_tool", "sourceHandle": "tool"},
		},
	}
	body, _ := json.Marshal(wfPayload)
	req, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+wfID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	return wfID, connID
}

func (s *AgentToolErrorIntegrationSuite) dispatchAndPoll(client *http.Client, wfID string, terminal []string) (string, string) {
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
	last := ""
	for time.Now().Before(deadline) {
		dResp, derr := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + dispatch.RunID)
		s.Require().NoError(derr)
		var detail map[string]any
		_ = json.NewDecoder(dResp.Body).Decode(&detail)
		_ = dResp.Body.Close()
		if run, ok := detail["run"].(map[string]any); ok {
			if st, _ := run["status"].(string); st != "" {
				last = st
				for _, t := range terminal {
					if st == t {
						return dispatch.RunID, st
					}
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.FailNow("dispatchAndPoll timeout", "run %s never reached terminal in 8s (last=%q)", dispatch.RunID, last)
	return dispatch.RunID, last
}

// TestAgent_StopOnToolError_True_ToolErrors_RunStatusError verifies
// the new flag's strict behaviour: the http_request as_tool returns
// a 500, the agent's catalog handler bubbles the err, the agent
// loop aborts, BFS marks the agent's StepResult.Error, RunFromCheckpoint
// promotes status to error.
func (s *AgentToolErrorIntegrationSuite) TestAgent_StopOnToolError_True_ToolErrors_RunStatusError() {
	client, tenantID := s.authedClient(fmt.Sprintf("alice-stopon-%d@example.com", time.Now().UnixNano()))
	s.mockMu.Lock()
	s.mockHandler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"err":"upstream blew up"}`))
	}
	s.mockMu.Unlock()
	wfID, _ := s.putAgentToolWorkflow(client, tenantID, true)

	_, status := s.dispatchAndPoll(client, wfID, []string{"success", "error", "cancelled"})
	s.Equal("error", status, "stop_on_tool_error=true should flip run to error on the first tool error")
	s.Equal(int32(1), s.stubProvider.chatCalls.Load(), "agent must abort after the first tool error — model is not re-prompted")
}

// TestAgent_StopOnToolError_False_ToolErrors_LLMRetriesAndCompletes
// preserves legacy behaviour: a non-infra tool error is fed back to
// the LLM as a tool_result, which lets the model pivot. Stub emits
// end_turn on call 2 → run lands success. Validates we didn't
// regress the default path.
func (s *AgentToolErrorIntegrationSuite) TestAgent_StopOnToolError_False_ToolErrors_LLMRetriesAndCompletes() {
	client, tenantID := s.authedClient(fmt.Sprintf("alice-stopoff-%d@example.com", time.Now().UnixNano()))
	s.mockMu.Lock()
	s.mockHandler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"err":"transient"}`))
	}
	s.mockMu.Unlock()
	wfID, _ := s.putAgentToolWorkflow(client, tenantID, false)

	_, status := s.dispatchAndPoll(client, wfID, []string{"success", "error", "cancelled"})
	s.Equal("success", status, "stop_on_tool_error=false (default) should let the LLM observe the tool error and finish via end_turn")
	s.Equal(int32(2), s.stubProvider.chatCalls.Load(), "agent should re-prompt once after the tool error before the model emits end_turn")
}

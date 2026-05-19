//go:build integration

// Webhook trigger integration tests — exercises POST /api/v1/webhooks/:slug
// against a wired-up Server.Handler() with real Mongo + a real
// WorkflowExecutor. Asserts slug routing, JSON body decoding, HMAC
// SHA-256 signature verification (valid, invalid, missing headers),
// and the unknown-slug 404 path. Compiled only under
// `-tags=integration`.

package handler_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

type WebhookIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	redis       *rediss.Client
	httpSrv     *httptest.Server

	// Lease-worker harness — the webhook handler now dispatches via
	// the lease path (PR 3.4b), so a wait=true test that doesn't have
	// a worker would hang until ctx timeout.
	workerCancel context.CancelFunc
	workerDone   chan struct{}
}

func TestWebhookIntegrationSuite(t *testing.T) {
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
	suite.Run(t, new(WebhookIntegrationSuite))
}

func (s *WebhookIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")
	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("burrow_test_webhook_%d", time.Now().UnixNano())
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

	wfExec := &workflow.WorkflowExecutor{
		AllowPrivateHTTPHosts: true, // test httptest mockSrv lives on 127.0.0.1
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

	// In-test workflow-executor worker so dispatched runs (the
	// webhook handler now persists queued + publishes wakeup, then
	// — on ?wait=true — polls for terminal). Without the worker the
	// wait-true tests would hang until ctx timeout. Cancelled in
	// TearDownSuite.
	workerCtx, workerCancel := context.WithCancel(context.Background())
	s.workerCancel = workerCancel
	s.workerDone = make(chan struct{})
	go func() {
		defer close(s.workerDone)
		runInTestExecutorWorker(workerCtx, "webhook-test-executor", runRepo, wfRepo, wfExec)
	}()
}

func (s *WebhookIntegrationSuite) TearDownSuite() {
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

func (s *WebhookIntegrationSuite) SetupTest() {
	for _, col := range []string{"users", "tenants", "tenant_members", "workflows", "workflow_runs", "audit_log", "api_keys", "connections"} {
		_, _ = s.db.Collection(col).DeleteMany(context.Background(), map[string]any{})
	}
	for _, name := range []string{"register", "login", "password_reset", "oauth_start"} {
		_, _ = s.redis.Del(context.Background(), "ratelimit:auth:"+name+":127.0.0.1")
	}
}

func (s *WebhookIntegrationSuite) TearDownTest() {}

// authedClient registers a fresh user + returns a cookie-jar client.
func (s *WebhookIntegrationSuite) authedClient(emailAddr string) *http.Client {
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

// putWebhookWorkflow PUTs a workflow with a webhook trigger node.
// secret is empty for the unsigned-webhook case; non-empty enables
// the HMAC verification path.
func (s *WebhookIntegrationSuite) putWebhookWorkflow(client *http.Client, wfID, slug, secret string) {
	triggerData := map[string]any{
		"trigger_type": "webhook",
		"webhook_slug": slug,
	}
	if secret != "" {
		triggerData["webhook_secret"] = secret
	}
	payload := map[string]any{
		"name":   "webhook test wf",
		"params": map[string]any{},
		"nodes": []map[string]any{
			{
				"id":       "trigger-1",
				"type":     "trigger",
				"position": map[string]any{"x": 0, "y": 0},
				"data":     triggerData,
			},
		},
		"edges": []any{},
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPut, s.httpSrv.URL+"/api/v1/workflows/"+wfID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ---- tests ----

// TestWebhook_NoSecret_JSONBody_Returns202Accepted verifies the
// unsigned webhook path: a workflow with `webhook_slug` but no
// `webhook_secret` accepts JSON bodies without a signature header
// and the dispatcher returns 202 Accepted with a run_id (default
// async path; `?wait=true` returns 200 when the run finishes).
func (s *WebhookIntegrationSuite) TestWebhook_NoSecret_JSONBody_Returns202Accepted() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-wh-noskey-%d@example.com", suffix))
	slug := fmt.Sprintf("hook-noskey-%d", suffix)
	s.putWebhookWorkflow(client, fmt.Sprintf("wf-wh-noskey-%d", suffix), slug, "")

	body := []byte(`{"event":"test","id":42}`)
	resp, err := http.Post(s.httpSrv.URL+"/api/v1/webhooks/"+slug, "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusAccepted, resp.StatusCode)
	var out struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&out))
	s.NotEmpty(out.RunID, "dispatch must return a run_id")
}

// TestWebhook_ValidSignature_WaitTrue_Returns200 verifies the signed
// + synchronous webhook path: a request carrying the correct HMAC
// signature AND `?wait=true` blocks until the run finishes and
// returns 200 with the run outcome.
func (s *WebhookIntegrationSuite) TestWebhook_ValidSignature_WaitTrue_Returns200() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-wh-valid-%d@example.com", suffix))
	slug := fmt.Sprintf("hook-valid-%d", suffix)
	secret := "shared-secret-for-test"
	s.putWebhookWorkflow(client, fmt.Sprintf("wf-wh-valid-%d", suffix), slug, secret)

	body := []byte(`{"event":"signed-payload"}`)
	req, _ := http.NewRequest(http.MethodPost, s.httpSrv.URL+"/api/v1/webhooks/"+slug+"?wait=true", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Signature", "sha256="+sign(secret, body))
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusOK, resp.StatusCode)
}

// TestWebhook_InvalidSignature_Returns401 verifies HMAC mismatch is
// rejected — wrong signature triggers 401, the workflow is NOT run.
func (s *WebhookIntegrationSuite) TestWebhook_InvalidSignature_Returns401() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-wh-bad-%d@example.com", suffix))
	slug := fmt.Sprintf("hook-bad-%d", suffix)
	s.putWebhookWorkflow(client, fmt.Sprintf("wf-wh-bad-%d", suffix), slug, "shared-secret")

	body := []byte(`{"event":"tampered"}`)
	req, _ := http.NewRequest(http.MethodPost, s.httpSrv.URL+"/api/v1/webhooks/"+slug, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Signature", "sha256=deadbeef")
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
}

// TestWebhook_MissingSignature_WhenSecretConfigured_Returns401
// verifies a signed-webhook workflow refuses unsigned requests —
// secret configured server-side but no header on the request.
func (s *WebhookIntegrationSuite) TestWebhook_MissingSignature_WhenSecretConfigured_Returns401() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-wh-miss-%d@example.com", suffix))
	slug := fmt.Sprintf("hook-miss-%d", suffix)
	s.putWebhookWorkflow(client, fmt.Sprintf("wf-wh-miss-%d", suffix), slug, "shared-secret")

	body := []byte(`{}`)
	resp, err := http.Post(s.httpSrv.URL+"/api/v1/webhooks/"+slug, "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
}

// TestWebhook_UnknownSlug_Returns404 verifies a request to a slug
// that no workflow claims returns 404 (not 500, not auto-create).
func (s *WebhookIntegrationSuite) TestWebhook_UnknownSlug_Returns404() {
	resp, err := http.Post(s.httpSrv.URL+"/api/v1/webhooks/no-such-slug",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusNotFound, resp.StatusCode)
}

// TestWebhook_DefaultAsync_DispatchesViaLeaseAndCompletes is the
// PR 3.4b regression test for the async path: the handler must
// persist the run as `queued`, return 202 + run_id immediately, and
// the lease worker must claim + run it to terminal `success`. Pre-
// 3.4b the handler ran the workflow inline via RunResumable and the
// lease worker never saw the record.
func (s *WebhookIntegrationSuite) TestWebhook_DefaultAsync_DispatchesViaLeaseAndCompletes() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-wh-async-%d@example.com", suffix))
	slug := fmt.Sprintf("hook-async-%d", suffix)
	s.putWebhookWorkflow(client, fmt.Sprintf("wf-wh-async-%d", suffix), slug, "")

	body := []byte(`{"event":"async-test"}`)
	resp, err := http.Post(s.httpSrv.URL+"/api/v1/webhooks/"+slug,
		"application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusAccepted, resp.StatusCode, "default path is async — expect 202")
	var out struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&out))
	s.Require().NotEmpty(out.RunID)
	s.Equal("queued", out.Status, "run record persists with status=queued; ClaimLease promotes to running on first claim")

	// Poll the run record until terminal — the in-test executor
	// worker must claim + run + complete.
	deadline := time.Now().Add(5 * time.Second)
	var lastStatus string
	for time.Now().Before(deadline) {
		getResp, gerr := client.Get(s.httpSrv.URL + "/api/v1/workflow_runs/" + out.RunID)
		if gerr == nil && getResp.StatusCode == http.StatusOK {
			var detail struct {
				Run struct {
					Status string `json:"status"`
				} `json:"run"`
			}
			_ = json.NewDecoder(getResp.Body).Decode(&detail)
			_ = getResp.Body.Close()
			lastStatus = detail.Run.Status
			if lastStatus == "success" {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.FailNowf("run never reached terminal", "run %s last status %q", out.RunID, lastStatus)
}

// TestWebhook_WaitTrue_BlocksUntilTerminalAndReturnsOutcome verifies
// the synchronous opt-in: `?wait=true` makes the handler poll until
// the lease worker drives the run terminal, then returns 200 with
// the final status. Previously this was implemented inline via
// RunResumable; PR 3.4b reroutes through the lease worker without
// changing the response contract.
func (s *WebhookIntegrationSuite) TestWebhook_WaitTrue_BlocksUntilTerminalAndReturnsOutcome() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-wh-wait-%d@example.com", suffix))
	slug := fmt.Sprintf("hook-wait-%d", suffix)
	s.putWebhookWorkflow(client, fmt.Sprintf("wf-wh-wait-%d", suffix), slug, "")

	body := []byte(`{"event":"sync-test"}`)
	req, _ := http.NewRequest(http.MethodPost,
		s.httpSrv.URL+"/api/v1/webhooks/"+slug+"?wait=true", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusOK, resp.StatusCode, "wait=true blocks until terminal; expect 200")

	var out struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&out))
	s.NotEmpty(out.RunID)
	s.Equal("success", out.Status, "trigger-only workflow lands success — sync response carries the terminal status")
	s.Empty(out.Error)
}

// TestWebhook_WaitFalseExplicit_Returns202 is the redundant-but-
// document-it variant: explicitly setting `?wait=false` matches the
// no-query default. Lets clients lock in the async contract on the
// URL even when the default is what they want.
func (s *WebhookIntegrationSuite) TestWebhook_WaitFalseExplicit_Returns202() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-wh-wfalse-%d@example.com", suffix))
	slug := fmt.Sprintf("hook-wfalse-%d", suffix)
	s.putWebhookWorkflow(client, fmt.Sprintf("wf-wh-wfalse-%d", suffix), slug, "")

	body := []byte(`{}`)
	resp, err := http.Post(s.httpSrv.URL+"/api/v1/webhooks/"+slug+"?wait=false",
		"application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusAccepted, resp.StatusCode)
}

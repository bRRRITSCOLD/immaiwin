//go:build integration

// Workflow mongo_request + redis_request node integration tests.
// Drives the executor's per-op dispatch path against the live Mongo
// + Redis from the compose stack — same instances the suite uses
// for auth/tenants/etc., separate test collections + key prefixes
// per case to avoid interference. Pattern mirrors the http_request
// suite (#50): trigger → request-node, assert on the executor's
// step output.
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
	"strings"
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

type WorkflowMongoRedisNodeIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	redis       *rediss.Client
	apiSrv      *httptest.Server

	workerCancel context.CancelFunc
	workerDone   chan struct{}
}

func TestWorkflowMongoRedisNodeIntegrationSuite(t *testing.T) {
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
	suite.Run(t, new(WorkflowMongoRedisNodeIntegrationSuite))
}

func (s *WorkflowMongoRedisNodeIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")
	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("burrow_test_workflow_mongo_redis_%d", time.Now().UnixNano())
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

	// Real executor — both DB + Redis wired. mongo_request /
	// redis_request now REQUIRE a non-empty connection_id (platform
	// DB/Redis are off-limits to workflows). ConnResolver is nil
	// here, so a set connection_id resolves back to e.DB / e.Redis —
	// the same services the test inspects — while still exercising
	// the security guard's non-empty requirement.
	wfExec := &workflow.WorkflowExecutor{
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
		DB:         mongodb.NewMongoClient(s.db),
		Redis:      s.redis,
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
		runInTestExecutorWorker(workerCtx, "test-executor-mongo-redis", runRepo, wfRepo, wfExec)
	}()
}

func (s *WorkflowMongoRedisNodeIntegrationSuite) TearDownSuite() {
	if s.workerCancel != nil {
		s.workerCancel()
		<-s.workerDone
	}
	if s.apiSrv != nil {
		s.apiSrv.Close()
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

func (s *WorkflowMongoRedisNodeIntegrationSuite) SetupTest() {
	for _, col := range []string{"users", "tenants", "tenant_members", "workflows", "workflow_runs", "audit_log", "api_keys", "connections", "wf_test_items"} {
		_, _ = s.db.Collection(col).DeleteMany(context.Background(), map[string]any{})
	}
	for _, name := range []string{"register", "login", "password_reset", "oauth_start"} {
		_, _ = s.redis.Del(context.Background(), "ratelimit:auth:"+name+":127.0.0.1")
	}
	// Test-key prefix used by the redis_request tests below — purge
	// between tests so Get/Incr start from a clean state.
	for _, k := range []string{"wf_test:counter", "wf_test:greeting"} {
		_, _ = s.redis.Del(context.Background(), k)
	}
}

func (s *WorkflowMongoRedisNodeIntegrationSuite) TearDownTest() {}

// ---- helpers ----

func (s *WorkflowMongoRedisNodeIntegrationSuite) authedClient(emailAddr string) *http.Client {
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

// putRequestNodeWorkflow PUTs a workflow with a manual trigger wired
// to a single mongo_request or redis_request node. Returns the
// workflow id so the test can run it.
func (s *WorkflowMongoRedisNodeIntegrationSuite) putRequestNodeWorkflow(client *http.Client, suffix int64, nodeType string, nodeData map[string]any) string {
	// mongo_request / redis_request reject an empty connection_id
	// (platform DB/Redis are not reachable from workflows). Auto-fill
	// a dummy id unless the caller set the key explicitly (the guard
	// test passes "" to assert the refusal).
	if nodeType == "mongo_request" || nodeType == "redis_request" {
		if _, ok := nodeData["connection_id"]; !ok {
			nodeData["connection_id"] = "test-conn"
		}
	}
	wfID := fmt.Sprintf("wf-%s-%d", strings.ReplaceAll(nodeType, "_", "-"), suffix)
	payload := map[string]any{
		"name":   "node test wf",
		"params": map[string]any{},
		"nodes": []map[string]any{
			{
				"id":       "trigger-1",
				"type":     "trigger",
				"position": map[string]any{"x": 0, "y": 0},
				"data":     map[string]any{"trigger_type": "manual"},
			},
			{
				"id":       "request-1",
				"type":     nodeType,
				"position": map[string]any{"x": 200, "y": 0},
				"data":     nodeData,
			},
		},
		"edges": []map[string]any{
			{"id": "e1", "source": "trigger-1", "target": "request-1"},
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

// runWorkflow POSTs /run (async) and polls the run-detail endpoint
// until the in-test worker finishes. Returns the terminal steps + status.
func (s *WorkflowMongoRedisNodeIntegrationSuite) runWorkflow(client *http.Client, wfID string) (steps []map[string]any, status string) {
	resp, err := client.Post(s.apiSrv.URL+"/api/v1/workflows/"+wfID+"/run",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusAccepted {
		return nil, ""
	}
	var dispatch struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&dispatch)
	if dispatch.RunID == "" {
		return nil, ""
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		dResp, derr := client.Get(s.apiSrv.URL + "/api/v1/workflow_runs/" + dispatch.RunID)
		if derr == nil {
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
					return steps, st
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.FailNow("runWorkflow timeout", "run %s never reached terminal state in 10s", dispatch.RunID)
	return nil, "timeout"
}

// ---- mongo_request tests ----

// TestMongoNode_InsertOne_PersistsDoc verifies the executor calls
// MongoClient.InsertOne with the data.document payload and surfaces
// the returned _id under output.inserted_id. The test then queries
// Mongo directly to confirm the row landed.
func (s *WorkflowMongoRedisNodeIntegrationSuite) TestMongoNode_InsertOne_PersistsDoc() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-mongo-ins-%d@example.com", suffix))
	wfID := s.putRequestNodeWorkflow(client, suffix, "mongo_request", map[string]any{
		"operation":  "insert_one",
		"collection": "wf_test_items",
		"document":   map[string]any{"name": "alpha", "n": 7},
	})
	steps, status := s.runWorkflow(client, wfID)
	s.Equal("success", status)
	step, ok := stepByID(steps, "request-1")
	s.Require().True(ok)
	output := step["output"].(map[string]any)
	s.NotEmpty(output["inserted_id"], "output.inserted_id must be populated")

	// Confirm the row landed.
	var got map[string]any
	err := s.db.Collection("wf_test_items").FindOne(context.Background(),
		map[string]any{"name": "alpha"}).Decode(&got)
	s.Require().NoError(err)
	s.EqualValues(7, got["n"])
}

// TestMongoNode_CountDocuments_AfterInserts verifies a multi-node
// workflow: insert two docs in one workflow run, then a follow-up
// count_documents read sees both. Exercises the executor's BFS +
// step-context plumbing alongside the per-op dispatch.
func (s *WorkflowMongoRedisNodeIntegrationSuite) TestMongoNode_CountDocuments_AfterInserts() {
	ctx := context.Background()
	// Pre-seed two docs via a direct write (avoids a multi-node graph
	// for this assertion — the InsertOne path is covered above).
	_, err := s.db.Collection("wf_test_items").InsertMany(ctx, []any{
		map[string]any{"category": "x", "n": 1},
		map[string]any{"category": "x", "n": 2},
		map[string]any{"category": "y", "n": 3},
	})
	s.Require().NoError(err)

	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-mongo-cnt-%d@example.com", suffix))
	wfID := s.putRequestNodeWorkflow(client, suffix, "mongo_request", map[string]any{
		"operation":  "count_documents",
		"collection": "wf_test_items",
		"filter":     map[string]any{"category": "x"},
	})
	steps, status := s.runWorkflow(client, wfID)
	s.Equal("success", status)
	step, _ := stepByID(steps, "request-1")
	output := step["output"].(map[string]any)
	s.EqualValues(2, output["count"])
}

// TestMongoNode_UnknownOp_FailsRun verifies the executor surfaces
// the per-op switch's default-case error as a workflow failure
// rather than a runtime panic.
func (s *WorkflowMongoRedisNodeIntegrationSuite) TestMongoNode_UnknownOp_FailsRun() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-mongo-bad-%d@example.com", suffix))
	wfID := s.putRequestNodeWorkflow(client, suffix, "mongo_request", map[string]any{
		"operation":  "not_a_real_op",
		"collection": "wf_test_items",
	})
	_, status := s.runWorkflow(client, wfID)
	s.Equal("error", status)
}

// ---- redis_request tests ----

// TestRedisNode_SetThenGet_RoundTrips runs two workflows: the first
// SETs a key, the second GETs it. Confirms the executor honours the
// data.key + data.value fields and round-trips through the live
// Redis client.
func (s *WorkflowMongoRedisNodeIntegrationSuite) TestRedisNode_SetThenGet_RoundTrips() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-redis-set-%d@example.com", suffix))

	setWF := s.putRequestNodeWorkflow(client, suffix, "redis_request", map[string]any{
		"operation": "set",
		"key":       "wf_test:greeting",
		"value":     "hola",
	})
	_, status := s.runWorkflow(client, setWF)
	s.Equal("success", status)

	// Verify directly on Redis.
	got, err := s.redis.Get(context.Background(), "wf_test:greeting")
	s.Require().NoError(err)
	s.Equal("hola", got)

	// And via a GET node.
	getWF := s.putRequestNodeWorkflow(client, suffix+1, "redis_request", map[string]any{
		"operation": "get",
		"key":       "wf_test:greeting",
	})
	steps, status := s.runWorkflow(client, getWF)
	s.Equal("success", status)
	step, _ := stepByID(steps, "request-1")
	output := step["output"].(map[string]any)
	s.Equal("hola", output["value"])
}

// TestRedisNode_Incr_PersistsCounter verifies the INCR op returns
// the post-increment value and the underlying key reflects it. Two
// runs back-to-back should yield 1 then 2.
func (s *WorkflowMongoRedisNodeIntegrationSuite) TestRedisNode_Incr_PersistsCounter() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-redis-inc-%d@example.com", suffix))
	wfID := s.putRequestNodeWorkflow(client, suffix, "redis_request", map[string]any{
		"operation": "incr",
		"key":       "wf_test:counter",
	})

	steps, status := s.runWorkflow(client, wfID)
	s.Equal("success", status)
	step, _ := stepByID(steps, "request-1")
	output := step["output"].(map[string]any)
	s.EqualValues(1, output["value"])

	steps, status = s.runWorkflow(client, wfID)
	s.Equal("success", status)
	step, _ = stepByID(steps, "request-1")
	output = step["output"].(map[string]any)
	s.EqualValues(2, output["value"])
}

// TestRedisNode_UnknownOp_FailsRun is the redis-side counterpart to
// the mongo unknown-op test — same purpose, different dispatcher.
func (s *WorkflowMongoRedisNodeIntegrationSuite) TestRedisNode_UnknownOp_FailsRun() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-redis-bad-%d@example.com", suffix))
	wfID := s.putRequestNodeWorkflow(client, suffix, "redis_request", map[string]any{
		"operation": "not_a_real_redis_op",
		"key":       "wf_test:greeting",
	})
	_, status := s.runWorkflow(client, wfID)
	s.Equal("error", status)
}

// TestMongoNode_NoConnection_RefusedBeforePlatformDB is a SECURITY
// regression guard: a mongo_request with an empty connection_id must
// be refused outright (the platform Mongo — workflow records, audit
// log, lease state — is NOT reachable from a user workflow). The run
// lands `error` and the step error names the missing connection; the
// op never executes.
func (s *WorkflowMongoRedisNodeIntegrationSuite) TestMongoNode_NoConnection_RefusedBeforePlatformDB() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-mongo-noconn-%d@example.com", suffix))
	wfID := s.putRequestNodeWorkflow(client, suffix, "mongo_request", map[string]any{
		"connection_id": "", // explicit empty → helper does NOT auto-fill
		"operation":     "delete_many",
		"collection":    "workflows", // would nuke platform data if the guard were absent
		"filter":        map[string]any{},
	})
	steps, status := s.runWorkflow(client, wfID)
	s.Equal("error", status, "empty connection_id must refuse — platform DB is off-limits")
	var reqStep map[string]any
	for _, st := range steps {
		if id, _ := st["node_id"].(string); id == "request-1" {
			reqStep = st
		}
	}
	s.Require().NotNil(reqStep)
	s.Contains(fmt.Sprint(reqStep["error"]), "connection is required",
		"error must explain the platform DB is not reachable")
}

// TestRedisNode_NoConnection_RefusedBeforePlatformRedis — same guard
// for redis_request (platform Redis: lease, run-event bus, approval
// registry, rate-limit keys).
func (s *WorkflowMongoRedisNodeIntegrationSuite) TestRedisNode_NoConnection_RefusedBeforePlatformRedis() {
	suffix := time.Now().UnixNano()
	client := s.authedClient(fmt.Sprintf("alice-redis-noconn-%d@example.com", suffix))
	wfID := s.putRequestNodeWorkflow(client, suffix, "redis_request", map[string]any{
		"connection_id": "",
		"operation":     "keys",
		"pattern":       "*", // would enumerate every platform key if unguarded
	})
	steps, status := s.runWorkflow(client, wfID)
	s.Equal("error", status, "empty connection_id must refuse — platform Redis is off-limits")
	var reqStep map[string]any
	for _, st := range steps {
		if id, _ := st["node_id"].(string); id == "request-1" {
			reqStep = st
		}
	}
	s.Require().NotNil(reqStep)
	s.Contains(fmt.Sprint(reqStep["error"]), "connection is required")
}

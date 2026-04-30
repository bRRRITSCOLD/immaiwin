//go:build integration

// API key integration tests — exercises the full create / list /
// revoke flow plus the Bearer-auth middleware path that uses a
// minted key as a session token. The middleware accepts cookie OR
// Bearer header; this suite covers the Bearer side end-to-end so
// programmatic clients (CLI, CI bots, MCP consumers) stay covered.
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
	"testing"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/api"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/api/handler"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/email"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/v2/mongo"
	driveroptions "go.mongodb.org/mongo-driver/v2/mongo/options"
)

type APIKeyIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	redis       *rediss.Client
	httpSrv     *httptest.Server
}

func TestAPIKeyIntegrationSuite(t *testing.T) {
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
	suite.Run(t, new(APIKeyIntegrationSuite))
}

func (s *APIKeyIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")
	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("immaiwin_test_apikeys_%d", time.Now().UnixNano())
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

	apiCfg := config.APIConfig{Host: "127.0.0.1", Port: 0, BaseURL: "http://127.0.0.1"}
	authCfg := config.AuthConfig{
		JWTSecret:         "integration_test_secret_dev_only_0123456789abcdef",
		JWTTTL:            "1h",
		AllowRegistration: true,
		UIBaseURL:         "http://127.0.0.1",
	}
	srv := api.NewServer(
		apiCfg, authCfg, s.redis,
		wfRepo, runRepo, (*workflow.WorkflowExecutor)(nil),
		connRepo, nil, nil, handler.EvalDeps{},
		users, tenants, apiKeys,
		nil, nil, auditRepo,
		email.NewLogSender(),
		s.db,
		nil,
	)
	s.httpSrv = httptest.NewServer(srv.Handler())
}

func (s *APIKeyIntegrationSuite) TearDownSuite() {
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

func (s *APIKeyIntegrationSuite) SetupTest() {
	for _, col := range []string{"users", "tenants", "tenant_members", "api_keys", "audit_log"} {
		_, _ = s.db.Collection(col).DeleteMany(context.Background(), map[string]any{})
	}
	for _, name := range []string{"register", "login", "password_reset", "oauth_start"} {
		_, _ = s.redis.Del(context.Background(), "ratelimit:auth:"+name+":127.0.0.1")
	}
}

func (s *APIKeyIntegrationSuite) TearDownTest() {}

// ---- helpers ----

func (s *APIKeyIntegrationSuite) authedClient(emailAddr string) *http.Client {
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

// createKey returns the raw value that subsequent Bearer-auth calls
// must carry. The id is needed for the revoke path.
func (s *APIKeyIntegrationSuite) createKey(client *http.Client, name string) (id, raw string) {
	body, _ := json.Marshal(map[string]string{"name": name})
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/api_keys", "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	var out struct {
		Key struct{ ID string } `json:"key"`
		Raw string              `json:"raw"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&out))
	s.Require().NotEmpty(out.Key.ID)
	s.Require().NotEmpty(out.Raw)
	return out.Key.ID, out.Raw
}

// bearerGet issues a GET with the raw key as a Bearer token. Used to
// verify the Bearer-auth middleware path independently of cookies.
func (s *APIKeyIntegrationSuite) bearerGet(rawKey, path string) *http.Response {
	req, _ := http.NewRequest(http.MethodGet, s.httpSrv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	return resp
}

// ---- tests ----

// TestCreate_ReturnsRawKey_OncePopulatedListShowsPrefixOnly verifies
// the create endpoint returns the raw value once and the list endpoint
// returns the same key by ID without exposing the raw value again.
func (s *APIKeyIntegrationSuite) TestCreate_ReturnsRawKey_OncePopulatedListShowsPrefixOnly() {
	client := s.authedClient(fmt.Sprintf("alice-create-%d@example.com", time.Now().UnixNano()))
	id, raw := s.createKey(client, "ci-bot")
	s.NotEmpty(id)
	s.NotEmpty(raw)

	// List exposes prefix metadata, not the raw value.
	resp, err := client.Get(s.httpSrv.URL + "/api/v1/api_keys")
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	var keys []map[string]any
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&keys))
	s.Len(keys, 1)
	s.Equal(id, keys[0]["id"])
	s.Equal("ci-bot", keys[0]["name"])
	s.NotContains(fmt.Sprint(keys[0]), raw, "list must never echo the raw key value")
}

// TestBearerAuth_ValidKey_ReturnsMe verifies the Bearer-auth path:
// a freshly-minted key carried in `Authorization: Bearer` resolves
// to the owner's user + tenant ctx — `/auth/me` then returns them.
func (s *APIKeyIntegrationSuite) TestBearerAuth_ValidKey_ReturnsMe() {
	emailAddr := fmt.Sprintf("alice-bearer-%d@example.com", time.Now().UnixNano())
	client := s.authedClient(emailAddr)
	_, raw := s.createKey(client, "ci-bot")

	resp := s.bearerGet(raw, "/api/v1/auth/me")
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	var me struct {
		User struct{ Email string } `json:"user"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&me))
	s.Equal(emailAddr, me.User.Email)
}

// TestBearerAuth_InvalidKey_Returns401 verifies a wrong-prefix key
// is rejected. `imk_` prefix is required + the key must lookup; a
// random string with the prefix but no DB row should 401.
func (s *APIKeyIntegrationSuite) TestBearerAuth_InvalidKey_Returns401() {
	resp := s.bearerGet("imk_does_not_exist_in_db", "/api/v1/auth/me")
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
}

// TestRevoke_BearerWithRevokedKey_Returns401 verifies revocation
// invalidates the key immediately — once DELETE returns the same key
// can no longer authenticate.
func (s *APIKeyIntegrationSuite) TestRevoke_BearerWithRevokedKey_Returns401() {
	client := s.authedClient(fmt.Sprintf("alice-revoke-%d@example.com", time.Now().UnixNano()))
	id, raw := s.createKey(client, "ci-bot")

	// Pre-revoke: Bearer works.
	pre := s.bearerGet(raw, "/api/v1/auth/me")
	_ = pre.Body.Close()
	s.Require().Equal(http.StatusOK, pre.StatusCode)

	// Revoke.
	req, _ := http.NewRequest(http.MethodDelete, s.httpSrv.URL+"/api/v1/api_keys/"+id, nil)
	resp, err := client.Do(req)
	s.Require().NoError(err)
	_ = resp.Body.Close()
	s.Require().Contains([]int{http.StatusOK, http.StatusNoContent}, resp.StatusCode)

	// Post-revoke: Bearer fails 401.
	post := s.bearerGet(raw, "/api/v1/auth/me")
	defer post.Body.Close() //nolint:errcheck
	s.Equal(http.StatusUnauthorized, post.StatusCode)
}

// TestList_NoKeys_ReturnsEmptyArray verifies the list endpoint
// returns a JSON array (not null) for users with no keys yet — UI
// renders a list-shaped value, so null would break the empty-state.
func (s *APIKeyIntegrationSuite) TestList_NoKeys_ReturnsEmptyArray() {
	client := s.authedClient(fmt.Sprintf("alice-empty-%d@example.com", time.Now().UnixNano()))
	resp, err := client.Get(s.httpSrv.URL + "/api/v1/api_keys")
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	var keys []map[string]any
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&keys))
	s.Empty(keys)
}

// TestCreate_NoSession_Returns401 verifies the create endpoint is
// behind RequireAuth — anonymous calls cannot mint keys.
func (s *APIKeyIntegrationSuite) TestCreate_NoSession_Returns401() {
	body, _ := json.Marshal(map[string]string{"name": "rogue"})
	resp, err := http.Post(s.httpSrv.URL+"/api/v1/api_keys", "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
}

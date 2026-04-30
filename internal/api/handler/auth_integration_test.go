// Auth + multi-tenancy integration tests — exercises the full
// register → login → /me → switch_tenant → logout flow against a
// wired-up Server.Handler() with real Mongo + Redis. Replaces the
// shell-driven verification that lived in the local-only smoke
// scripts; this is what other contributors run.

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

type AuthIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	redis       *rediss.Client
	httpSrv     *httptest.Server
}

func TestAuthIntegrationSuite(t *testing.T) {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer probeCancel()

	mc, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Skipf("mongo connect failed (skipping integration suite): %v", err)
		return
	}
	if err := mc.Ping(probeCtx, nil); err != nil {
		_ = mc.Disconnect(context.Background())
		t.Skipf("mongo unreachable at %s (skipping integration suite): %v", mongoURI, err)
		return
	}
	_ = mc.Disconnect(context.Background())
	suite.Run(t, new(AuthIntegrationSuite))
}

func (s *AuthIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")

	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("immaiwin_test_auth_%d", time.Now().UnixNano())
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

func (s *AuthIntegrationSuite) TearDownSuite() {
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

func (s *AuthIntegrationSuite) SetupTest() {
	for _, col := range []string{"users", "tenants", "tenant_members", "audit_log", "api_keys"} {
		_, _ = s.db.Collection(col).DeleteMany(context.Background(), map[string]any{})
	}
	for _, name := range []string{"register", "login", "password_reset", "oauth_start"} {
		_, _ = s.redis.Del(context.Background(), "ratelimit:auth:"+name+":127.0.0.1")
	}
}

func (s *AuthIntegrationSuite) TearDownTest() {}

// ---- helpers ----

func (s *AuthIntegrationSuite) newJarClient() *http.Client {
	jar, err := cookiejar.New(nil)
	s.Require().NoError(err)
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

func (s *AuthIntegrationSuite) registerNew(client *http.Client, emailAddr string) (userID, tenantID string) {
	body, _ := json.Marshal(map[string]string{"email": emailAddr, "password": "password1234"})
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	var out struct {
		User     struct{ ID string } `json:"user"`
		TenantID string              `json:"tenant_id"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&out))
	return out.User.ID, out.TenantID
}

// ---- tests ----

// TestRegister_Login_RoundTrip_Returns200WithCookieAndUser verifies
// the email/password sign-up + sign-in flow: register stamps a user
// + personal tenant, login with the same creds returns 200 and sets
// the auth cookie, and `/auth/me` then returns the populated user.
func (s *AuthIntegrationSuite) TestRegister_Login_RoundTrip_Returns200WithCookieAndUser() {
	emailAddr := fmt.Sprintf("alice-login-%d@example.com", time.Now().UnixNano())

	// Register on its own client.
	regClient := s.newJarClient()
	userID, tenantID := s.registerNew(regClient, emailAddr)
	s.NotEmpty(userID)
	s.NotEmpty(tenantID)

	// Fresh client (no cookies) — exercise the login path independently.
	loginClient := s.newJarClient()
	body, _ := json.Marshal(map[string]string{"email": emailAddr, "password": "password1234"})
	resp, err := loginClient.Post(s.httpSrv.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	// /me returns the same user.
	meResp, err := loginClient.Get(s.httpSrv.URL + "/api/v1/auth/me")
	s.Require().NoError(err)
	defer meResp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, meResp.StatusCode)
	var me struct {
		User struct{ ID, Email string } `json:"user"`
	}
	s.Require().NoError(json.NewDecoder(meResp.Body).Decode(&me))
	s.Equal(userID, me.User.ID)
	s.Equal(emailAddr, me.User.Email)
}

// TestLogin_WrongPassword_Returns401 verifies bad creds don't leak
// auth — login with a nonexistent password returns 401, no cookie.
func (s *AuthIntegrationSuite) TestLogin_WrongPassword_Returns401() {
	emailAddr := fmt.Sprintf("alice-bad-%d@example.com", time.Now().UnixNano())
	s.registerNew(s.newJarClient(), emailAddr)

	body, _ := json.Marshal(map[string]string{"email": emailAddr, "password": "wrong"})
	resp, err := http.Post(s.httpSrv.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
}

// TestMe_NoCookie_Returns401 verifies /auth/me requires a valid
// session — RequireAuth middleware rejects unauthenticated requests.
func (s *AuthIntegrationSuite) TestMe_NoCookie_Returns401() {
	resp, err := http.Get(s.httpSrv.URL + "/api/v1/auth/me")
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
}

// TestSwitchTenant_NonMember_Returns403 verifies a user can't switch
// into a tenant they don't belong to. Alice and Bob each get their
// own personal tenant on register; alice's request to switch into
// bob's tenant must 403.
func (s *AuthIntegrationSuite) TestSwitchTenant_NonMember_Returns403() {
	suffix := time.Now().UnixNano()
	alice := s.newJarClient()
	s.registerNew(alice, fmt.Sprintf("alice-switch-%d@example.com", suffix))
	bob := s.newJarClient()
	_, bobTenant := s.registerNew(bob, fmt.Sprintf("bob-switch-%d@example.com", suffix))

	body, _ := json.Marshal(map[string]string{"tenant_id": bobTenant})
	req, _ := http.NewRequest(http.MethodPost, s.httpSrv.URL+"/api/v1/auth/switch_tenant", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := alice.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusForbidden, resp.StatusCode)
}

// TestLogout_ClearsCookie_SubsequentMeReturns401 verifies logout
// invalidates the session — POST /auth/logout returns 200 + clears
// the cookie, and a follow-up /auth/me on the same client (now with
// no auth) returns 401.
func (s *AuthIntegrationSuite) TestLogout_ClearsCookie_SubsequentMeReturns401() {
	client := s.newJarClient()
	s.registerNew(client, fmt.Sprintf("alice-logout-%d@example.com", time.Now().UnixNano()))

	// /me works while logged in.
	meResp, err := client.Get(s.httpSrv.URL + "/api/v1/auth/me")
	s.Require().NoError(err)
	_ = meResp.Body.Close()
	s.Require().Equal(http.StatusOK, meResp.StatusCode)

	// Logout.
	logoutResp, err := client.Post(s.httpSrv.URL+"/api/v1/auth/logout", "application/json", bytes.NewReader([]byte("{}")))
	s.Require().NoError(err)
	_ = logoutResp.Body.Close()
	s.Require().Equal(http.StatusOK, logoutResp.StatusCode)

	// /me now 401.
	me2, err := client.Get(s.httpSrv.URL + "/api/v1/auth/me")
	s.Require().NoError(err)
	_ = me2.Body.Close()
	s.Equal(http.StatusUnauthorized, me2.StatusCode)
}

// TestRegister_DuplicateEmail_Returns409 verifies the unique-email
// invariant is enforced at the API boundary — second registration
// with the same address returns 409 conflict.
func (s *AuthIntegrationSuite) TestRegister_DuplicateEmail_Returns409() {
	emailAddr := fmt.Sprintf("alice-dup-%d@example.com", time.Now().UnixNano())
	body, _ := json.Marshal(map[string]string{"email": emailAddr, "password": "password1234"})

	first, err := http.Post(s.httpSrv.URL+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	_ = first.Body.Close()
	s.Require().Equal(http.StatusOK, first.StatusCode)

	dup, err := http.Post(s.httpSrv.URL+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer dup.Body.Close() //nolint:errcheck
	s.Equal(http.StatusConflict, dup.StatusCode)
}

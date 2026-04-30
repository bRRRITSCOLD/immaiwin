//go:build integration

// Tenant management integration tests — invite create/accept/revoke,
// member list/remove, ownership transfer. Exercises the full invite
// flow against a wired-up Server.Handler() with a real Mongo + Redis
// + InviteRepository so single-use enforcement, role gates, and
// audit-log writes all run through the production code paths.

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

type TenantIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	redis       *rediss.Client
	httpSrv     *httptest.Server
}

func TestTenantIntegrationSuite(t *testing.T) {
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
	suite.Run(t, new(TenantIntegrationSuite))
}

func (s *TenantIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")
	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("burrow_test_tenant_%d", time.Now().UnixNano())
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
	invitesRepo, err := mongodb.NewInviteRepository(ctx, s.db)
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
		nil, invitesRepo, auditRepo,
		email.NewLogSender(),
		s.db,
		nil,
	)
	s.httpSrv = httptest.NewServer(srv.Handler())
}

func (s *TenantIntegrationSuite) TearDownSuite() {
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

func (s *TenantIntegrationSuite) SetupTest() {
	for _, col := range []string{"users", "tenants", "tenant_members", "tenant_invites", "audit_log", "api_keys"} {
		_, _ = s.db.Collection(col).DeleteMany(context.Background(), map[string]any{})
	}
	for _, name := range []string{"register", "login", "password_reset", "oauth_start"} {
		_, _ = s.redis.Del(context.Background(), "ratelimit:auth:"+name+":127.0.0.1")
	}
}

func (s *TenantIntegrationSuite) TearDownTest() {}

// ---- helpers ----

// regResult is what /auth/register returns. We capture user id +
// tenant id so subsequent tests can target the right rows directly.
type regResult struct {
	UserID   string
	TenantID string
}

func (s *TenantIntegrationSuite) registerNew(client *http.Client, emailAddr string) regResult {
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
	return regResult{UserID: out.User.ID, TenantID: out.TenantID}
}

func (s *TenantIntegrationSuite) newJarClient() *http.Client {
	jar, err := cookiejar.New(nil)
	s.Require().NoError(err)
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

// switchTenant re-issues the JWT cookie with a new active tenant.
// Required when a user registered (got their personal tenant) but
// needs to act on a different tenant they were invited into.
func (s *TenantIntegrationSuite) switchTenant(client *http.Client, tenantID string) {
	body, _ := json.Marshal(map[string]string{"tenant_id": tenantID})
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/auth/switch_tenant",
		"application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
}

// createInvite returns the issued invite token. Caller must be the
// tenant's owner or admin.
func (s *TenantIntegrationSuite) createInvite(client *http.Client, email, role string) string {
	body, _ := json.Marshal(map[string]string{"email": email, "role": role})
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/tenants/invites",
		"application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	var out struct {
		URL string `json:"url"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&out))
	// URL is `<UIBaseURL>/invite/<token>`. Extract the trailing token.
	idx := strings.LastIndex(out.URL, "/")
	s.Require().GreaterOrEqual(idx, 0, "invite URL %s lacks /token", out.URL)
	return out.URL[idx+1:]
}

// ---- tests ----

// TestInviteFlow_BobAcceptsAlicesInvite_GainsAdminRole verifies the
// full invite happy path: alice (owner) creates an admin-role invite
// for bob's email, bob registers, bob accepts the token, and bob's
// /auth/me now lists alice's tenant in his memberships with admin
// role.
func (s *TenantIntegrationSuite) TestInviteFlow_BobAcceptsAlicesInvite_GainsAdminRole() {
	suffix := time.Now().UnixNano()
	alice := s.newJarClient()
	bob := s.newJarClient()

	aliceReg := s.registerNew(alice, fmt.Sprintf("alice-flow-%d@example.com", suffix))
	bobEmail := fmt.Sprintf("bob-flow-%d@example.com", suffix)
	s.registerNew(bob, bobEmail)

	token := s.createInvite(alice, bobEmail, "admin")
	s.NotEmpty(token)

	// Bob accepts.
	resp, err := bob.Post(s.httpSrv.URL+"/api/v1/invites/"+token+"/accept",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	var acc struct {
		TenantID string `json:"tenant_id"`
		Role     string `json:"role"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&acc))
	s.Equal(aliceReg.TenantID, acc.TenantID)
	s.Equal("admin", acc.Role)

	// /me reflects two memberships — bob's personal + alice's tenant.
	meResp, err := bob.Get(s.httpSrv.URL + "/api/v1/auth/me")
	s.Require().NoError(err)
	defer meResp.Body.Close() //nolint:errcheck
	var me struct {
		Memberships []struct {
			Tenant struct{ ID string }
			Role   string
		} `json:"memberships"`
	}
	s.Require().NoError(json.NewDecoder(meResp.Body).Decode(&me))
	var foundAdmin bool
	for _, m := range me.Memberships {
		if m.Tenant.ID == aliceReg.TenantID && m.Role == "admin" {
			foundAdmin = true
		}
	}
	s.True(foundAdmin, "bob /me must include alice's tenant w/ admin role; got %+v", me.Memberships)
}

// TestInvite_ReAcceptSameToken_Returns410 verifies single-use:
// re-accepting an already-redeemed token returns 410 Gone.
func (s *TenantIntegrationSuite) TestInvite_ReAcceptSameToken_Returns410() {
	suffix := time.Now().UnixNano()
	alice := s.newJarClient()
	bob := s.newJarClient()
	s.registerNew(alice, fmt.Sprintf("alice-reuse-%d@example.com", suffix))
	bobEmail := fmt.Sprintf("bob-reuse-%d@example.com", suffix)
	s.registerNew(bob, bobEmail)
	token := s.createInvite(alice, bobEmail, "member")

	// First accept: 200.
	r1, err := bob.Post(s.httpSrv.URL+"/api/v1/invites/"+token+"/accept",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	_ = r1.Body.Close()
	s.Require().Equal(http.StatusOK, r1.StatusCode)

	// Second accept: 410.
	r2, err := bob.Post(s.httpSrv.URL+"/api/v1/invites/"+token+"/accept",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer r2.Body.Close() //nolint:errcheck
	s.Equal(http.StatusGone, r2.StatusCode)
}

// TestInvite_NonAdminCaller_Returns403 verifies that a plain member
// cannot create invites — only owner / admin roles may.
func (s *TenantIntegrationSuite) TestInvite_NonAdminCaller_Returns403() {
	suffix := time.Now().UnixNano()
	alice := s.newJarClient()
	bob := s.newJarClient()
	aliceReg := s.registerNew(alice, fmt.Sprintf("alice-rbac-%d@example.com", suffix))
	bobEmail := fmt.Sprintf("bob-rbac-%d@example.com", suffix)
	s.registerNew(bob, bobEmail)

	// Alice invites bob as a plain member.
	memberToken := s.createInvite(alice, bobEmail, "member")
	r, err := bob.Post(s.httpSrv.URL+"/api/v1/invites/"+memberToken+"/accept",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	_ = r.Body.Close()
	s.Require().Equal(http.StatusOK, r.StatusCode)

	// Bob's JWT still claims his personal tenant — switch to alice's
	// where his role is `member`.
	s.switchTenant(bob, aliceReg.TenantID)

	// Bob (member) tries to create an invite — must 403.
	body, _ := json.Marshal(map[string]string{
		"email": fmt.Sprintf("charlie-%d@example.com", suffix),
		"role":  "member",
	})
	resp, err := bob.Post(s.httpSrv.URL+"/api/v1/tenants/invites",
		"application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusForbidden, resp.StatusCode)
}

// TestOwnershipTransfer_HappyPath_FlipsRolesAndAuditLogs verifies
// the full transfer flow: alice (owner) promotes bob (admin) to
// owner, the member list reflects the swap, and the audit log
// records `tenant_ownership_transferred`.
func (s *TenantIntegrationSuite) TestOwnershipTransfer_HappyPath_FlipsRolesAndAuditLogs() {
	suffix := time.Now().UnixNano()
	alice := s.newJarClient()
	bob := s.newJarClient()

	aliceReg := s.registerNew(alice, fmt.Sprintf("alice-xfer-%d@example.com", suffix))
	bobEmail := fmt.Sprintf("bob-xfer-%d@example.com", suffix)
	bobReg := s.registerNew(bob, bobEmail)

	// Promote bob to admin via invite + accept, then bob switches
	// active tenant. Transfer requires alice=owner (default) and
	// bob=admin/member of alice's tenant.
	token := s.createInvite(alice, bobEmail, "admin")
	r, err := bob.Post(s.httpSrv.URL+"/api/v1/invites/"+token+"/accept",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	_ = r.Body.Close()

	// Transfer.
	body, _ := json.Marshal(map[string]string{"to_user_id": bobReg.UserID})
	resp, err := alice.Post(s.httpSrv.URL+"/api/v1/tenants/transfer",
		"application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	// Verify roles flipped — query members via bob (he's now owner).
	s.switchTenant(bob, aliceReg.TenantID)
	mResp, err := bob.Get(s.httpSrv.URL + "/api/v1/tenants/members")
	s.Require().NoError(err)
	defer mResp.Body.Close() //nolint:errcheck
	var members struct {
		Members []map[string]any `json:"members"`
	}
	s.Require().NoError(json.NewDecoder(mResp.Body).Decode(&members))
	rolesByID := map[string]string{}
	for _, m := range members.Members {
		rolesByID[fmt.Sprint(m["user_id"])] = fmt.Sprint(m["role"])
	}
	s.Equal("owner", rolesByID[bobReg.UserID])
	s.Equal("admin", rolesByID[aliceReg.UserID])

	// Audit log records the transfer (owner-only read; alice still
	// admin so still authorized).
	aResp, err := alice.Get(s.httpSrv.URL + "/api/v1/audit_log?action=tenant_ownership_transferred&limit=10")
	s.Require().NoError(err)
	defer aResp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, aResp.StatusCode)
	var audit struct {
		Entries []map[string]any `json:"entries"`
	}
	s.Require().NoError(json.NewDecoder(aResp.Body).Decode(&audit))
	s.NotEmpty(audit.Entries, "audit log must record the ownership transfer")
}

// TestOwnershipTransfer_SelfAsTarget_Returns400 verifies the API
// rejects a no-op transfer where source == target.
func (s *TenantIntegrationSuite) TestOwnershipTransfer_SelfAsTarget_Returns400() {
	alice := s.newJarClient()
	aliceReg := s.registerNew(alice, fmt.Sprintf("alice-self-%d@example.com", time.Now().UnixNano()))
	body, _ := json.Marshal(map[string]string{"to_user_id": aliceReg.UserID})
	resp, err := alice.Post(s.httpSrv.URL+"/api/v1/tenants/transfer",
		"application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusBadRequest, resp.StatusCode)
}

// TestOwnershipTransfer_NonMember_Returns400 verifies the target
// must already be a member — transferring to a random user id is
// rejected.
func (s *TenantIntegrationSuite) TestOwnershipTransfer_NonMember_Returns400() {
	alice := s.newJarClient()
	s.registerNew(alice, fmt.Sprintf("alice-nonmem-%d@example.com", time.Now().UnixNano()))
	body, _ := json.Marshal(map[string]string{"to_user_id": "not-a-member"})
	resp, err := alice.Post(s.httpSrv.URL+"/api/v1/tenants/transfer",
		"application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusBadRequest, resp.StatusCode)
}

// TestOwnershipTransfer_AdminCaller_Returns403 verifies that only
// the owner can initiate transfer — an admin trying it gets 403.
// (Admin can do most other privileged ops; transfer is owner-only.)
func (s *TenantIntegrationSuite) TestOwnershipTransfer_AdminCaller_Returns403() {
	suffix := time.Now().UnixNano()
	alice := s.newJarClient()
	bob := s.newJarClient()
	aliceReg := s.registerNew(alice, fmt.Sprintf("alice-adm-%d@example.com", suffix))
	bobEmail := fmt.Sprintf("bob-adm-%d@example.com", suffix)
	s.registerNew(bob, bobEmail)
	token := s.createInvite(alice, bobEmail, "admin")
	r, err := bob.Post(s.httpSrv.URL+"/api/v1/invites/"+token+"/accept",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	_ = r.Body.Close()
	s.switchTenant(bob, aliceReg.TenantID)

	// Bob (admin) tries to transfer to himself; even with valid args
	// the role gate fires first.
	body, _ := json.Marshal(map[string]string{"to_user_id": aliceReg.UserID})
	resp, err := bob.Post(s.httpSrv.URL+"/api/v1/tenants/transfer",
		"application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusForbidden, resp.StatusCode)
}


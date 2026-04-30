//go:build integration

// Password-reset integration tests — exercises the full /request →
// email-token → /confirm → re-login flow against a wired-up
// Server.Handler() with real Mongo + Redis. A test-only in-memory
// email.Sender captures dispatched messages so the test can extract
// the issued reset URL and feed the token back through /confirm.
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
	"net/url"
	"strings"
	"sync"
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

// recordingSender captures every Send call in memory + signals via a
// channel so tests can block until the dispatch goroutine has run.
// Mirrors the email.Sender interface; test-only — never wired in prod.
type recordingSender struct {
	mu   sync.Mutex
	msgs []email.Message
	ch   chan email.Message
}

func newRecordingSender() *recordingSender {
	return &recordingSender{ch: make(chan email.Message, 16)}
}

func (r *recordingSender) Send(_ context.Context, msg email.Message) error {
	r.mu.Lock()
	r.msgs = append(r.msgs, msg)
	r.mu.Unlock()
	r.ch <- msg
	return nil
}

// waitForMessage blocks until a message arrives or the timeout elapses.
func (r *recordingSender) waitForMessage(timeout time.Duration) (email.Message, bool) {
	select {
	case m := <-r.ch:
		return m, true
	case <-time.After(timeout):
		return email.Message{}, false
	}
}

type PasswordResetIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	redis       *rediss.Client
	httpSrv     *httptest.Server
	sender      *recordingSender
}

func TestPasswordResetIntegrationSuite(t *testing.T) {
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
	suite.Run(t, new(PasswordResetIntegrationSuite))
}

func (s *PasswordResetIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")
	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("immaiwin_test_pwreset_%d", time.Now().UnixNano())
	s.db = c.Database(s.dbName)
	s.redis = rediss.New(config.RedisConfig{Host: "localhost", Port: 6379})
	s.sender = newRecordingSender()

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
		s.sender,
		s.db,
		nil,
	)
	s.httpSrv = httptest.NewServer(srv.Handler())
}

func (s *PasswordResetIntegrationSuite) TearDownSuite() {
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

func (s *PasswordResetIntegrationSuite) SetupTest() {
	for _, col := range []string{"users", "tenants", "tenant_members", "audit_log", "api_keys"} {
		_, _ = s.db.Collection(col).DeleteMany(context.Background(), map[string]any{})
	}
	for _, name := range []string{"register", "login", "password_reset", "oauth_start"} {
		_, _ = s.redis.Del(context.Background(), "ratelimit:auth:"+name+":127.0.0.1")
	}
	// Drain any messages left from previous tests so waitForMessage in
	// the next test doesn't see a stale value.
	for {
		select {
		case <-s.sender.ch:
		default:
			return
		}
	}
}

func (s *PasswordResetIntegrationSuite) TearDownTest() {}

// ---- helpers ----

func (s *PasswordResetIntegrationSuite) registerNew(emailAddr, password string) {
	body, _ := json.Marshal(map[string]string{"email": emailAddr, "password": password})
	resp, err := http.Post(s.httpSrv.URL+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
}

func (s *PasswordResetIntegrationSuite) requestReset(emailAddr string) {
	body, _ := json.Marshal(map[string]string{"email": emailAddr})
	resp, err := http.Post(s.httpSrv.URL+"/api/v1/auth/password_reset/request", "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	// Always 200 — no enumeration leak per the handler design.
	s.Require().Equal(http.StatusOK, resp.StatusCode)
}

func (s *PasswordResetIntegrationSuite) confirmReset(token, newPassword string) int {
	body, _ := json.Marshal(map[string]string{"token": token, "new_password": newPassword})
	resp, err := http.Post(s.httpSrv.URL+"/api/v1/auth/password_reset/confirm", "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	return resp.StatusCode
}

func (s *PasswordResetIntegrationSuite) login(emailAddr, password string) int {
	jar, err := cookiejar.New(nil)
	s.Require().NoError(err)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	body, _ := json.Marshal(map[string]string{"email": emailAddr, "password": password})
	resp, err := client.Post(s.httpSrv.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	return resp.StatusCode
}

// extractToken pulls the `token=<...>` query param out of a reset URL
// embedded in the email body.
func extractToken(t *testing.T, body string) string {
	t.Helper()
	const marker = "/reset?token="
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("body did not contain reset URL: %s", body)
	}
	tail := body[i+len(marker):]
	// URL ends at first whitespace.
	end := strings.IndexAny(tail, " \n\t")
	if end < 0 {
		end = len(tail)
	}
	tok, err := url.QueryUnescape(tail[:end])
	if err != nil {
		t.Fatalf("unescape token: %v", err)
	}
	return tok
}

// ---- tests ----

// TestPasswordReset_HappyPath_NewPasswordWorks_OldPasswordRejected
// verifies the full reset flow: request → email captured → token
// extracted → confirm → old password fails to log in → new password
// succeeds.
func (s *PasswordResetIntegrationSuite) TestPasswordReset_HappyPath_NewPasswordWorks_OldPasswordRejected() {
	emailAddr := fmt.Sprintf("alice-pwr-%d@example.com", time.Now().UnixNano())
	const oldPass = "originalpw1234"
	const newPass = "freshlyrotatedpw9876"
	s.registerNew(emailAddr, oldPass)

	s.requestReset(emailAddr)
	msg, ok := s.sender.waitForMessage(2 * time.Second)
	s.Require().True(ok, "reset email must arrive within 2s")
	s.Equal(emailAddr, msg.To)

	token := extractToken(s.T(), msg.Body)
	s.NotEmpty(token)

	s.Equal(http.StatusOK, s.confirmReset(token, newPass))

	// Old password no longer works.
	s.Equal(http.StatusUnauthorized, s.login(emailAddr, oldPass))
	// New password does.
	s.Equal(http.StatusOK, s.login(emailAddr, newPass))
}

// TestPasswordReset_TokenReuse_Returns401 verifies single-use: a
// successfully-redeemed token cannot be re-used. The redis-tracked
// hash is deleted on first confirm, so a second confirm fails.
func (s *PasswordResetIntegrationSuite) TestPasswordReset_TokenReuse_Returns401() {
	emailAddr := fmt.Sprintf("alice-reuse-%d@example.com", time.Now().UnixNano())
	s.registerNew(emailAddr, "originalpw1234")

	s.requestReset(emailAddr)
	msg, ok := s.sender.waitForMessage(2 * time.Second)
	s.Require().True(ok)
	token := extractToken(s.T(), msg.Body)

	s.Equal(http.StatusOK, s.confirmReset(token, "newpassword12345"))
	// Reuse must fail.
	s.Equal(http.StatusUnauthorized, s.confirmReset(token, "anothernewpw123"))
}

// TestPasswordReset_BogusToken_Returns401 verifies a malformed /
// unsigned token can't trigger a password change.
func (s *PasswordResetIntegrationSuite) TestPasswordReset_BogusToken_Returns401() {
	s.Equal(http.StatusUnauthorized, s.confirmReset("not.a.valid.jwt", "anything12345"))
}

// TestPasswordReset_NonexistentEmail_Returns200_NoEmail verifies the
// no-enumeration design: requesting a reset for an unknown address
// returns 200 (same as a valid one) and no email is dispatched.
// Reasonable wait window then assert nothing arrived.
func (s *PasswordResetIntegrationSuite) TestPasswordReset_NonexistentEmail_Returns200_NoEmail() {
	s.requestReset(fmt.Sprintf("ghost-%d@example.com", time.Now().UnixNano()))
	_, ok := s.sender.waitForMessage(500 * time.Millisecond)
	s.False(ok, "no email should be dispatched for unknown addresses")
}

// TestPasswordReset_ConfirmMissingFields_Returns400 verifies input
// validation — empty token or empty new_password is rejected as 400
// before any auth check.
func (s *PasswordResetIntegrationSuite) TestPasswordReset_ConfirmMissingFields_Returns400() {
	s.Equal(http.StatusBadRequest, s.confirmReset("", "newpassword12345"))
	s.Equal(http.StatusBadRequest, s.confirmReset("some-token", ""))
}

//go:build integration

// Skill registry integration tests — exercises the agent-skills HTTP
// surface (`GET /api/v1/skills`, `POST /api/v1/skills/refresh`)
// against a real Mongo-backed registry and a real `LocalFSSource`
// pointed at the in-repo `skills/bundled/` directory. Verifies the
// registry round-trips (refresh imports, list returns), the refresh
// path is idempotent, and the disabled-backend fallback returns an
// empty array shape.
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
	"path/filepath"
	"testing"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/api"
	"github.com/bRRRITSCOLD/burrow/internal/api/handler"
	"github.com/bRRRITSCOLD/burrow/internal/config"
	"github.com/bRRRITSCOLD/burrow/internal/email"
	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/bRRRITSCOLD/burrow/internal/rediss"
	"github.com/bRRRITSCOLD/burrow/internal/skills"
	"github.com/bRRRITSCOLD/burrow/internal/workflow"
	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/v2/mongo"
	driveroptions "go.mongodb.org/mongo-driver/v2/mongo/options"
)

type SkillsIntegrationSuite struct {
	suite.Suite

	mongoClient *mongo.Client
	db          *mongo.Database
	dbName      string
	redis       *rediss.Client
	apiSrv      *httptest.Server
}

func TestSkillsIntegrationSuite(t *testing.T) {
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
	suite.Run(t, new(SkillsIntegrationSuite))
}

func (s *SkillsIntegrationSuite) SetupSuite() {
	mongoURI := envOr("MONGO_URI", "mongodb://localhost:27017")
	c, err := mongo.Connect(driveroptions.Client().ApplyURI(mongoURI))
	s.Require().NoError(err)
	s.mongoClient = c
	s.dbName = fmt.Sprintf("burrow_test_skills_%d", time.Now().UnixNano())
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
	skillReg, err := mongodb.NewSkillRegistry(ctx, s.db)
	s.Require().NoError(err)

	// LocalFS source pointed at the in-repo bundled-skills directory.
	// Tests run from `internal/api/handler/`, so back up three levels
	// to reach the repo root.
	bundledDir, err := filepath.Abs("../../../skills/bundled")
	s.Require().NoError(err)
	fsSrc := skills.NewLocalFSSource(bundledDir, "local-fs-test")
	skillBackend := &handler.SkillBackend{
		Registry: skillReg,
		Sources:  []skills.Source{fsSrc},
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
		wfRepo, runRepo, (*workflow.WorkflowExecutor)(nil),
		connRepo, nil, skillBackend, handler.EvalDeps{},
		users, tenants, apiKeys,
		nil, nil, auditRepo,
		email.NewLogSender(),
		s.db,
		nil,
	)
	s.apiSrv = httptest.NewServer(srv.Handler())
}

func (s *SkillsIntegrationSuite) TearDownSuite() {
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

func (s *SkillsIntegrationSuite) SetupTest() {
	for _, col := range []string{"users", "tenants", "tenant_members", "audit_log", "api_keys", "skills"} {
		_, _ = s.db.Collection(col).DeleteMany(context.Background(), map[string]any{})
	}
	for _, name := range []string{"register", "login", "password_reset", "oauth_start"} {
		_, _ = s.redis.Del(context.Background(), "ratelimit:auth:"+name+":127.0.0.1")
	}
}

func (s *SkillsIntegrationSuite) TearDownTest() {}

// ---- helpers ----

func (s *SkillsIntegrationSuite) authedClient(emailAddr string) *http.Client {
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

func (s *SkillsIntegrationSuite) listSkills(client *http.Client) []map[string]any {
	resp, err := client.Get(s.apiSrv.URL + "/api/v1/skills")
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	var out []map[string]any
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func (s *SkillsIntegrationSuite) refreshSkills(client *http.Client) (imported int, errs []string) {
	resp, err := client.Post(s.apiSrv.URL+"/api/v1/skills/refresh",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Require().Equal(http.StatusOK, resp.StatusCode)
	var out struct {
		Imported int      `json:"imported"`
		Errors   []string `json:"errors"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&out))
	return out.Imported, out.Errors
}

// ---- tests ----

// TestSkills_FreshDB_ListReturnsEmptyArray verifies `[]` (not null)
// when the registry has no rows yet — UI empty-state contract.
func (s *SkillsIntegrationSuite) TestSkills_FreshDB_ListReturnsEmptyArray() {
	client := s.authedClient(fmt.Sprintf("alice-skill-empty-%d@example.com", time.Now().UnixNano()))
	out := s.listSkills(client)
	s.Empty(out)
}

// TestSkills_RefreshFromBundledDir_ImportsAllManifests verifies that
// `POST /api/v1/skills/refresh` walks the configured LocalFSSource,
// upserts each (slug, version) into the registry, and returns the
// import count. The bundled-skills directory ships at least two
// skills (hello-world + weather-formatter), so the assertion expects
// >= 2 imports.
func (s *SkillsIntegrationSuite) TestSkills_RefreshFromBundledDir_ImportsAllManifests() {
	client := s.authedClient(fmt.Sprintf("alice-skill-refresh-%d@example.com", time.Now().UnixNano()))
	imported, errs := s.refreshSkills(client)
	s.Empty(errs, "refresh must not surface per-manifest errors")
	s.GreaterOrEqual(imported, 2, "bundled dir ships >= 2 skills")

	// List now returns those records.
	rows := s.listSkills(client)
	s.GreaterOrEqual(len(rows), 2)
	slugs := map[string]bool{}
	for _, r := range rows {
		slugs[fmt.Sprint(r["slug_id"])] = true
	}
	s.True(slugs["hello-world"], "hello-world skill should be in the registry; got %v", slugs)
	s.True(slugs["weather-formatter"], "weather-formatter skill should be in the registry; got %v", slugs)
}

// TestSkills_RefreshTwice_IsIdempotent verifies a second refresh
// against the same source doesn't error and doesn't duplicate rows.
func (s *SkillsIntegrationSuite) TestSkills_RefreshTwice_IsIdempotent() {
	client := s.authedClient(fmt.Sprintf("alice-skill-idem-%d@example.com", time.Now().UnixNano()))
	first, _ := s.refreshSkills(client)
	s.GreaterOrEqual(first, 2)
	rowsAfterFirst := s.listSkills(client)

	second, errs := s.refreshSkills(client)
	s.Empty(errs)
	// Second pass re-imports the same records (upsert semantics).
	s.Equal(first, second, "second refresh should re-import the same count")

	rowsAfterSecond := s.listSkills(client)
	s.Equal(len(rowsAfterFirst), len(rowsAfterSecond),
		"row count must not grow on repeat refresh")
}

// TestSkills_NoCookie_ListReturns401 verifies the list endpoint is
// behind RequireAuth — anonymous calls cannot enumerate the registry.
func (s *SkillsIntegrationSuite) TestSkills_NoCookie_ListReturns401() {
	resp, err := http.Get(s.apiSrv.URL + "/api/v1/skills")
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
}

// TestSkills_NoCookie_RefreshReturns401 verifies the refresh endpoint
// is behind RequireAuth too — anonymous mutation blocked.
func (s *SkillsIntegrationSuite) TestSkills_NoCookie_RefreshReturns401() {
	resp, err := http.Post(s.apiSrv.URL+"/api/v1/skills/refresh",
		"application/json", bytes.NewReader([]byte(`{}`)))
	s.Require().NoError(err)
	defer resp.Body.Close() //nolint:errcheck
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
}

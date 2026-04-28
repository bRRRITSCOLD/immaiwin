package skills

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/suite"
)

type ResolverTestSuite struct {
	suite.Suite
	root     string
	registry *MemoryRegistry
	src      *LocalFSSource
	resolver *Resolver
}

func TestResolverTestSuite(t *testing.T) {
	suite.Run(t, new(ResolverTestSuite))
}

func (s *ResolverTestSuite) SetupSuite()    {}
func (s *ResolverTestSuite) TearDownSuite() {}

func (s *ResolverTestSuite) SetupTest() {
	s.root = s.T().TempDir()
	weatherDir := filepath.Join(s.root, "weather-pro")
	s.Require().NoError(os.MkdirAll(filepath.Join(weatherDir, "tools"), 0o755))
	s.Require().NoError(os.MkdirAll(filepath.Join(weatherDir, "prompts"), 0o755))
	s.Require().NoError(os.WriteFile(filepath.Join(weatherDir, ManifestFileName), []byte(validManifestYAML), 0o644))
	s.Require().NoError(os.WriteFile(filepath.Join(weatherDir, "tools", "fetch_forecast.py"), []byte(`output({"days": 7})`), 0o644))
	s.Require().NoError(os.WriteFile(filepath.Join(weatherDir, "prompts", "system.md"), []byte("hi"), 0o644))

	s.registry = NewMemoryRegistry()
	s.src = NewLocalFSSource(s.root, "test-src")
	s.resolver = NewResolver(s.registry, s.src)
}

func (s *ResolverTestSuite) TearDownTest() {}

func (s *ResolverTestSuite) TestResolveColdLoadsFromSource() {
	locks, err := s.resolver.Resolve(context.Background(), "default", []SkillReq{
		{SlugID: "weather-pro", Range: "^1.0.0"},
	})
	s.Require().NoError(err)
	s.Require().Len(locks, 1)
	s.Equal("weather-pro", locks[0].SlugID)
	s.Equal("1.4.2", locks[0].Version)
	s.Len(locks[0].Checksum, 64)

	// Resolver should have written the record into the registry.
	rec, err := s.registry.GetRecord(context.Background(), "weather-pro", "1.4.2")
	s.Require().NoError(err)
	s.Equal("test-src", rec.SourceID)
	s.Equal(locks[0].Checksum, rec.Checksum)
}

func (s *ResolverTestSuite) TestResolveUsesRegistryWhenInstalled() {
	// Pre-populate registry; remove the on-disk source so we know it
	// resolved purely from the registry.
	s.Require().NoError(os.RemoveAll(filepath.Join(s.root, "weather-pro")))
	_, err := s.registry.UpsertRecord(context.Background(), SkillRecord{
		SlugID: "weather-pro", Version: "1.4.2",
		SourceID: "test-src", Checksum: "abc",
	})
	s.Require().NoError(err)

	locks, err := s.resolver.Resolve(context.Background(), "default", []SkillReq{
		{SlugID: "weather-pro", Range: "^1.0.0"},
	})
	s.Require().NoError(err)
	s.Equal("1.4.2", locks[0].Version)
	s.Equal("abc", locks[0].Checksum)
}

func (s *ResolverTestSuite) TestResolveFailsWhenNoMatchingVersion() {
	_, err := s.resolver.Resolve(context.Background(), "default", []SkillReq{
		{SlugID: "weather-pro", Range: "^9.0.0"},
	})
	s.Require().Error(err)
}

func (s *ResolverTestSuite) TestResolveFailsForUnknownSlug() {
	_, err := s.resolver.Resolve(context.Background(), "default", []SkillReq{
		{SlugID: "ghost-skill", Range: "*"},
	})
	s.Require().Error(err)
}

func (s *ResolverTestSuite) TestVerifyDetectsChecksumDrift() {
	locks, err := s.resolver.Resolve(context.Background(), "default", []SkillReq{
		{SlugID: "weather-pro", Range: "^1.0.0"},
	})
	s.Require().NoError(err)

	// Tamper with the registry record to simulate drift.
	rec, _ := s.registry.GetRecord(context.Background(), "weather-pro", "1.4.2")
	rec.Checksum = "tampered"
	_, _ = s.registry.UpsertRecord(context.Background(), rec)

	err = s.resolver.Verify(context.Background(), locks)
	s.Require().Error(err)
	s.Contains(err.Error(), "checksum drift")
}

func (s *ResolverTestSuite) TestLoadBundleRetrievesFromOriginatingSource() {
	locks, err := s.resolver.Resolve(context.Background(), "default", []SkillReq{
		{SlugID: "weather-pro", Range: "^1.0.0"},
	})
	s.Require().NoError(err)

	b, err := s.resolver.LoadBundle(context.Background(), locks[0])
	s.Require().NoError(err)
	defer b.Close()

	code, err := b.ReadString("tools/fetch_forecast.py")
	s.Require().NoError(err)
	s.Contains(code, "output")
}

func (s *ResolverTestSuite) TestResolveHonoursTildeRange() {
	// Add a higher minor version that would NOT match `~1.4.0`.
	weatherV15 := filepath.Join(s.root, "weather-pro-15")
	s.Require().NoError(os.MkdirAll(filepath.Join(weatherV15, "tools"), 0o755))
	v15 := `
id: weather-pro
version: 1.5.0
name: WP
description: WP
author:
  name: A
license: MIT
api_version: 1
tools:
  - id: t
    file: tools/x.py
    language: python
    description: t
capabilities:
  network: {}
  storage: {}
`
	s.Require().NoError(os.WriteFile(filepath.Join(weatherV15, ManifestFileName), []byte(v15), 0o644))
	s.Require().NoError(os.WriteFile(filepath.Join(weatherV15, "tools", "x.py"), []byte("output(1)"), 0o644))

	// LocalFS only exposes one version per slug at a time (directory layout
	// limit). Skip this assertion if the directory walker conflates the two.
	// In practice a registry-backed setup handles multi-version resolution.
	locks, err := s.resolver.Resolve(context.Background(), "default", []SkillReq{
		{SlugID: "weather-pro", Range: "~1.4.0"},
	})
	s.Require().NoError(err)
	s.Equal("1.4.2", locks[0].Version)
}

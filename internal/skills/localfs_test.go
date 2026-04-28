package skills

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/suite"
)

type LocalFSTestSuite struct {
	suite.Suite
	root string
}

func TestLocalFSTestSuite(t *testing.T) {
	suite.Run(t, new(LocalFSTestSuite))
}

func (s *LocalFSTestSuite) SetupSuite() {}
func (s *LocalFSTestSuite) TearDownSuite() {}

func (s *LocalFSTestSuite) SetupTest() {
	s.root = s.T().TempDir()

	// weather-pro v1.4.2
	weatherDir := filepath.Join(s.root, "weather-pro")
	s.Require().NoError(os.MkdirAll(filepath.Join(weatherDir, "tools"), 0o755))
	s.Require().NoError(os.MkdirAll(filepath.Join(weatherDir, "prompts"), 0o755))
	s.Require().NoError(os.WriteFile(filepath.Join(weatherDir, ManifestFileName), []byte(validManifestYAML), 0o644))
	s.Require().NoError(os.WriteFile(filepath.Join(weatherDir, "tools", "fetch_forecast.py"), []byte(`output({"days": 7})`), 0o644))
	s.Require().NoError(os.WriteFile(filepath.Join(weatherDir, "prompts", "system.md"), []byte("Mention forecasts only when asked."), 0o644))

	// data-utils v2.5.0
	utilsDir := filepath.Join(s.root, "data-utils")
	s.Require().NoError(os.MkdirAll(filepath.Join(utilsDir, "tools"), 0o755))
	utilsManifest := `
id: data-utils
version: 2.5.0
name: "Data Utils"
description: "Misc helpers"
author:
  name: "Acme"
license: MIT
api_version: 1
tools:
  - id: parse_csv
    file: tools/parse_csv.py
    language: python
    description: "Parse CSV"
capabilities:
  network: {}
  storage: {}
`
	s.Require().NoError(os.WriteFile(filepath.Join(utilsDir, ManifestFileName), []byte(utilsManifest), 0o644))
	s.Require().NoError(os.WriteFile(filepath.Join(utilsDir, "tools", "parse_csv.py"), []byte(`output([])`), 0o644))
}

func (s *LocalFSTestSuite) TearDownTest() {}

func (s *LocalFSTestSuite) TestListReturnsAllValidBundles() {
	src := NewLocalFSSource(s.root, "test")
	manifests, err := src.List(context.Background())
	s.Require().NoError(err)
	s.Len(manifests, 2)
	// Sorted by ID alphabetically.
	s.Equal("data-utils", manifests[0].ID)
	s.Equal("weather-pro", manifests[1].ID)
}

func (s *LocalFSTestSuite) TestListEmptyRootReturnsNil() {
	src := NewLocalFSSource(filepath.Join(s.root, "does-not-exist"), "test")
	manifests, err := src.List(context.Background())
	s.Require().NoError(err)
	s.Empty(manifests)
}

func (s *LocalFSTestSuite) TestLoadReturnsBundle() {
	src := NewLocalFSSource(s.root, "test")
	b, err := src.Load(context.Background(), "weather-pro", "1.4.2")
	s.Require().NoError(err)
	defer b.Close()

	s.Equal("weather-pro", b.Manifest().ID)
	s.NotEmpty(b.Files())

	code, err := b.ReadString("tools/fetch_forecast.py")
	s.Require().NoError(err)
	s.Contains(code, "output")
}

func (s *LocalFSTestSuite) TestLoadWrongVersionFails() {
	src := NewLocalFSSource(s.root, "test")
	_, err := src.Load(context.Background(), "weather-pro", "9.9.9")
	s.Require().Error(err)
}

func (s *LocalFSTestSuite) TestBundleRefusesPathTraversal() {
	src := NewLocalFSSource(s.root, "test")
	b, err := src.Load(context.Background(), "weather-pro", "1.4.2")
	s.Require().NoError(err)
	defer b.Close()

	_, err = b.Open("../data-utils/tools/parse_csv.py")
	s.Require().Error(err)
}

func (s *LocalFSTestSuite) TestVerifyChecksumStableAcrossCalls() {
	src := NewLocalFSSource(s.root, "test")
	c1, err := src.Verify(context.Background(), "weather-pro", "1.4.2")
	s.Require().NoError(err)
	c2, err := src.Verify(context.Background(), "weather-pro", "1.4.2")
	s.Require().NoError(err)
	s.Equal(c1, c2)
	s.Len(c1, 64) // hex-encoded sha256
}

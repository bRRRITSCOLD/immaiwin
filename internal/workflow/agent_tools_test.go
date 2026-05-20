package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bRRRITSCOLD/burrow/internal/llm"
	"github.com/stretchr/testify/suite"
)

type ToolCatalogFilterSuite struct {
	suite.Suite
}

func (s *ToolCatalogFilterSuite) SetupSuite()    {}
func (s *ToolCatalogFilterSuite) TearDownSuite() {}
func (s *ToolCatalogFilterSuite) SetupTest()     {}
func (s *ToolCatalogFilterSuite) TearDownTest()  {}

// TestToolCatalogFilterSuite is the test entrypoint for the per-agent
// `allowed_tools` authorization policy applied via ToolCatalog.FilterAllowed.
func TestToolCatalogFilterSuite(t *testing.T) {
	suite.Run(t, new(ToolCatalogFilterSuite))
}

func (s *ToolCatalogFilterSuite) buildCatalog(names ...string) *ToolCatalog {
	cat := NewToolCatalog()
	for _, n := range names {
		def := llm.ToolDef{Name: n, Description: "test " + n, InputSchema: json.RawMessage(`{"type":"object"}`)}
		s.Require().NoError(cat.Add(def, func(_ context.Context, _ json.RawMessage) (string, error) {
			return "ran:" + n, nil
		}))
	}
	return cat
}

// TestFilterAllowed_EmptyList_IsNoop verifies the open-default
// behaviour: missing/empty `allowed_tools` leaves every tool in the
// catalog so workflows authored before the policy existed keep
// working unchanged.
func (s *ToolCatalogFilterSuite) TestFilterAllowed_EmptyList_IsNoop() {
	cat := s.buildCatalog("code_execute", "http_request", "publish_weather")
	cat.FilterAllowed(nil)
	s.Len(cat.Defs(), 3)
	cat.FilterAllowed([]string{})
	s.Len(cat.Defs(), 3)
	cat.FilterAllowed([]string{"   ", ""}) // whitespace-only list collapses to empty
	s.Len(cat.Defs(), 3)
}

// TestFilterAllowed_KeepsOnlyAllowedNames verifies the core contract:
// the catalog is reduced to the named subset, registration order is
// preserved (so the LLM's tool-list ordering stays stable), and the
// dropped tools' handlers + validators are evicted too.
func (s *ToolCatalogFilterSuite) TestFilterAllowed_KeepsOnlyAllowedNames() {
	cat := s.buildCatalog("code_execute", "http_request", "publish_weather")

	cat.FilterAllowed([]string{"http_request", "publish_weather"})

	got := []string{}
	for _, d := range cat.Defs() {
		got = append(got, d.Name)
	}
	s.Equal([]string{"http_request", "publish_weather"}, got)

	// Filtered-out tool's handler MUST be gone from the dispatch map —
	// otherwise a hallucinated call could still execute.
	_, _, err := callExecute(cat, "code_execute")
	s.Require().Error(err)
	s.True(strings.Contains(err.Error(), "unknown tool"), "expected unknown-tool error; got %v", err) //nolint:staticcheck // strings.Contains is fine here

}

// TestFilterAllowed_UnknownNameInList_IsIgnored verifies the lenient
// "ignore unknown" posture: passing a name that isn't in the
// catalog doesn't crash and doesn't add a phantom entry.
func (s *ToolCatalogFilterSuite) TestFilterAllowed_UnknownNameInList_IsIgnored() {
	cat := s.buildCatalog("http_request")
	cat.FilterAllowed([]string{"http_request", "no_such_tool"})
	s.Len(cat.Defs(), 1)
	s.Equal("http_request", cat.Defs()[0].Name)
}

// TestFilterAllowed_DeniedToolCallReturnsUnknownTool verifies the
// defense-in-depth path: even if the LLM hallucinates a call to a
// filtered-out tool, Execute returns the standard unknown-tool
// error (with the still-available tools listed so the model can
// self-correct) instead of dispatching the handler.
func (s *ToolCatalogFilterSuite) TestFilterAllowed_DeniedToolCallReturnsUnknownTool() {
	cat := s.buildCatalog("code_execute", "http_request")
	cat.FilterAllowed([]string{"http_request"})

	out, _, err := callExecute(cat, "code_execute")
	s.Require().Error(err)
	s.Empty(out, "denied tool MUST NOT dispatch its handler")
}

// callExecute is a tiny helper to avoid threading json.RawMessage
// boilerplate through every assertion.
func callExecute(cat *ToolCatalog, name string) (string, string, error) {
	out, err := cat.Execute(context.Background(), name, json.RawMessage(`{}`))
	return out, name, err
}

package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/suite"
)

type AgentAsToolSuite struct {
	suite.Suite
}

func (s *AgentAsToolSuite) SetupSuite()    {}
func (s *AgentAsToolSuite) TearDownSuite() {}
func (s *AgentAsToolSuite) SetupTest()     {}
func (s *AgentAsToolSuite) TearDownTest()  {}

// TestAgentAsToolSuite covers the cycle/depth guards on the
// agent-as-tool dispatch path. The full agent loop (runAIAgent)
// requires provider + env wiring tested at the integration tier;
// these unit tests focus the ancestor-chain primitives so a
// regression in the cycle/depth math fails fast.
func TestAgentAsToolSuite(t *testing.T) {
	suite.Run(t, new(AgentAsToolSuite))
}

// TestAgentAncestors_NoChain_ReturnsNil verifies the default ctx
// (no agent on the stack) returns an empty ancestor slice so top-
// level agent runs pass through with zero overhead and no false
// cycle hits.
func (s *AgentAsToolSuite) TestAgentAncestors_NoChain_ReturnsNil() {
	got := agentAncestors(context.Background())
	s.Nil(got)
}

// TestWithAgentAncestor_Appends_PreservesPriorChain verifies the
// caller's ID lands at the end of the chain and prior callers stay
// in order. Order matters for the depth + cycle math.
func (s *AgentAsToolSuite) TestWithAgentAncestor_Appends_PreservesPriorChain() {
	ctx := withAgentAncestor(context.Background(), "agent-a")
	ctx = withAgentAncestor(ctx, "agent-b")
	ctx = withAgentAncestor(ctx, "agent-c")
	got := agentAncestors(ctx)
	s.Equal([]string{"agent-a", "agent-b", "agent-c"}, got)
}

// TestAgentAncestors_ReturnsCopy_NotSharedSlice verifies callers
// can append to the returned slice without corrupting the ctx
// value. Without the defensive copy, two sibling sub-agents would
// see each other's appends and the cycle check would false-positive.
func (s *AgentAsToolSuite) TestAgentAncestors_ReturnsCopy_NotSharedSlice() {
	ctx := withAgentAncestor(context.Background(), "agent-a")
	first := agentAncestors(ctx)
	// Mutate position 0 — if agentAncestors returned the ctx's
	// underlying slice directly, this write would corrupt the chain
	// for every subsequent reader.
	first[0] = "leaked"
	s.NotContains(agentAncestors(ctx), "leaked",
		"agentAncestors must return a defensive copy so caller mutations don't bleed into ctx")
}

// TestAgentNestingDepthCap_AtLimit_BlocksNext verifies the cap
// pre-condition: a chain at exactly maxAgentNestingDepth refuses
// to admit one more. The real refusal lives in runAIAgent; this
// test locks the threshold so a future tweak doesn't silently
// loosen the cap.
func (s *AgentAsToolSuite) TestAgentNestingDepthCap_AtLimit_BlocksNext() {
	s.Equal(5, maxAgentNestingDepth, "depth cap drift — review impact on integration tests + roadmap copy")
}

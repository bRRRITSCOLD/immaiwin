package workflow

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

type ForEachParallelSuite struct {
	suite.Suite
}

func (s *ForEachParallelSuite) SetupSuite()    {}
func (s *ForEachParallelSuite) TearDownSuite() {}
func (s *ForEachParallelSuite) SetupTest()     {}
func (s *ForEachParallelSuite) TearDownTest()  {}

// TestForEachParallelSuite locks the `data.parallelism` knob on the
// for_each node — read, clamp, default. The actual concurrent
// dispatch path is covered by the engine integration tier (it needs
// a full BFS + body chain); these unit tests focus the input parsing
// so a future tweak can't silently widen the cap or drop the back-
// compat default.
func TestForEachParallelSuite(t *testing.T) {
	suite.Run(t, new(ForEachParallelSuite))
}

// TestForEachParallelism_AbsentDefaultsToOne verifies the missing-key
// case stays sequential — the whole back-compat story rests on this
// (any pre-existing workflow without the field keeps current behavior).
func (s *ForEachParallelSuite) TestForEachParallelism_AbsentDefaultsToOne() {
	s.Equal(1, forEachParallelism(map[string]any{}))
}

// TestForEachParallelism_BelowOne_CoercesToOne — author saves 0 / -1
// shouldn't kill the run; coerce up to sequential.
func (s *ForEachParallelSuite) TestForEachParallelism_BelowOne_CoercesToOne() {
	s.Equal(1, forEachParallelism(map[string]any{"parallelism": float64(0)}))
	s.Equal(1, forEachParallelism(map[string]any{"parallelism": float64(-3)}))
	s.Equal(1, forEachParallelism(map[string]any{"parallelism": 0}))
}

// TestForEachParallelism_AboveMax_Clamps — a misconfigured workflow
// can't spawn unbounded goroutines. Cap holds.
func (s *ForEachParallelSuite) TestForEachParallelism_AboveMax_Clamps() {
	s.Equal(32, forEachParallelism(map[string]any{"parallelism": float64(100)}))
	s.Equal(32, forEachParallelism(map[string]any{"parallelism": 9999}))
}

// TestForEachParallelism_ValidRangeRoundTrips verifies typical
// author values pass through.
func (s *ForEachParallelSuite) TestForEachParallelism_ValidRangeRoundTrips() {
	s.Equal(2, forEachParallelism(map[string]any{"parallelism": float64(2)}))
	s.Equal(5, forEachParallelism(map[string]any{"parallelism": 5}))
	s.Equal(32, forEachParallelism(map[string]any{"parallelism": float64(32)}))
}

// TestForEachParallelism_NonNumericFallsBackToOne — defensive: a
// string or nil where number expected shouldn't crash. Coerce to
// sequential.
func (s *ForEachParallelSuite) TestForEachParallelism_NonNumericFallsBackToOne() {
	s.Equal(1, forEachParallelism(map[string]any{"parallelism": "5"}))
	s.Equal(1, forEachParallelism(map[string]any{"parallelism": nil}))
	s.Equal(1, forEachParallelism(map[string]any{"parallelism": true}))
}

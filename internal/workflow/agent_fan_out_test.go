package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/llm"
	"github.com/stretchr/testify/suite"
)

type FanOutSuite struct {
	suite.Suite
}

func (s *FanOutSuite) SetupSuite()    {}
func (s *FanOutSuite) TearDownSuite() {}
func (s *FanOutSuite) SetupTest()     {}
func (s *FanOutSuite) TearDownTest()  {}

// TestFanOutSuite locks the parallel-dispatch primitive behind the
// agent's `fan_out` built-in tool: ordered results, sibling-error
// tolerance, parallelism cap, default cap. The agent loop integration
// is exercised at the integration tier; these unit tests focus the
// closure-internal logic so a regression in concurrency / ordering /
// error-aggregation fails fast.
func TestFanOutSuite(t *testing.T) {
	suite.Run(t, new(FanOutSuite))
}

// newFanOutCatalog wires a minimal ToolCatalog containing `echo` (returns
// its `task` arg back) and `fail` (always errors). Same pattern the
// real agent loop uses: built-ins + fan_out share one catalog.
func newFanOutCatalog() (*ToolCatalog, *atomic.Int64, *int64) {
	cat := NewToolCatalog()

	var inFlight atomic.Int64
	var peak int64
	var peakMu sync.Mutex

	echoDef := llm.ToolDef{
		Name:        "echo",
		Description: "Returns input back; for tests only.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"task":{"type":"string"}}}`),
	}
	echoHandler := func(_ context.Context, args json.RawMessage) (string, error) {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		// Track peak concurrency to verify the semaphore caps.
		peakMu.Lock()
		if cur > peak {
			peak = cur
		}
		peakMu.Unlock()
		time.Sleep(20 * time.Millisecond) // give the cap something to bite on
		var in struct {
			Task string `json:"task"`
		}
		_ = json.Unmarshal(args, &in)
		return fmt.Sprintf(`{"task":"%s"}`, in.Task), nil
	}
	_ = cat.Add(echoDef, echoHandler)

	failDef := llm.ToolDef{
		Name:        "fail",
		Description: "Always errors; for tests only.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
	failHandler := func(_ context.Context, _ json.RawMessage) (string, error) {
		return "", fmt.Errorf("intentional failure")
	}
	_ = cat.Add(failDef, failHandler)

	fanDef, fanHandler := builtinFanOutTool(cat)
	_ = cat.Add(fanDef, fanHandler)

	return cat, &inFlight, &peak
}

// TestFanOut_OrderedResults_MatchInputOrder verifies the
// per-input-index slot in the results array even when goroutines
// finish out of order. Without index-into-pre-sized-slice writes the
// LLM would see jumbled correlations.
func (s *FanOutSuite) TestFanOut_OrderedResults_MatchInputOrder() {
	cat, _, _ := newFanOutCatalog()
	args := json.RawMessage(`{"calls":[
		{"tool":"echo","args":{"task":"a"}},
		{"tool":"echo","args":{"task":"b"}},
		{"tool":"echo","args":{"task":"c"}}
	],"parallelism":3}`)
	out, err := cat.Execute(context.Background(), "fan_out", args)
	s.Require().NoError(err)
	var resp struct {
		Results []struct {
			Output map[string]any `json:"output"`
			Error  string         `json:"error"`
		} `json:"results"`
	}
	s.Require().NoError(json.Unmarshal([]byte(out), &resp))
	s.Require().Len(resp.Results, 3)
	s.Equal("a", resp.Results[0].Output["task"])
	s.Equal("b", resp.Results[1].Output["task"])
	s.Equal("c", resp.Results[2].Output["task"])
}

// TestFanOut_SiblingErrorDoesNotAbort verifies a single tool failure
// surfaces in its result slot WITHOUT cancelling the other in-flight
// calls — the LLM gets the partial-success array and decides whether
// to retry.
func (s *FanOutSuite) TestFanOut_SiblingErrorDoesNotAbort() {
	cat, _, _ := newFanOutCatalog()
	args := json.RawMessage(`{"calls":[
		{"tool":"echo","args":{"task":"ok"}},
		{"tool":"fail","args":{}},
		{"tool":"echo","args":{"task":"also-ok"}}
	]}`)
	out, err := cat.Execute(context.Background(), "fan_out", args)
	s.Require().NoError(err)
	var resp struct {
		Results []struct {
			Output any    `json:"output"`
			Error  string `json:"error"`
		} `json:"results"`
	}
	s.Require().NoError(json.Unmarshal([]byte(out), &resp))
	s.Require().Len(resp.Results, 3)
	s.Empty(resp.Results[0].Error)
	s.NotEmpty(resp.Results[1].Error)
	s.Contains(resp.Results[1].Error, "intentional failure")
	s.Empty(resp.Results[2].Error)
}

// TestFanOut_ParallelismCap_BoundsInFlight verifies the semaphore
// channel respects the `parallelism` arg. With cap=2 + 5 calls each
// taking ~20ms, no more than 2 should ever be in-flight concurrently.
func (s *FanOutSuite) TestFanOut_ParallelismCap_BoundsInFlight() {
	cat, _, peak := newFanOutCatalog()
	calls := strings.Join([]string{
		`{"tool":"echo","args":{"task":"a"}}`,
		`{"tool":"echo","args":{"task":"b"}}`,
		`{"tool":"echo","args":{"task":"c"}}`,
		`{"tool":"echo","args":{"task":"d"}}`,
		`{"tool":"echo","args":{"task":"e"}}`,
	}, ",")
	args := json.RawMessage(`{"calls":[` + calls + `],"parallelism":2}`)
	_, err := cat.Execute(context.Background(), "fan_out", args)
	s.Require().NoError(err)
	s.LessOrEqual(*peak, int64(2), "parallelism cap=2 must bound in-flight; observed peak=%d", *peak)
}

// TestFanOut_DefaultParallelism_AppliedWhenOmitted verifies that
// omitting the `parallelism` arg falls back to fanOutDefaultParallelism
// rather than serial execution.
func (s *FanOutSuite) TestFanOut_DefaultParallelism_AppliedWhenOmitted() {
	cat, _, peak := newFanOutCatalog()
	calls := []string{}
	for i := range 6 {
		calls = append(calls, fmt.Sprintf(`{"tool":"echo","args":{"task":"t%d"}}`, i))
	}
	args := json.RawMessage(`{"calls":[` + strings.Join(calls, ",") + `]}`)
	_, err := cat.Execute(context.Background(), "fan_out", args)
	s.Require().NoError(err)
	s.Greater(*peak, int64(1), "default parallelism (%d) should produce >1 concurrent call; observed peak=%d", fanOutDefaultParallelism, *peak)
	s.LessOrEqual(*peak, int64(fanOutDefaultParallelism), "default cap must hold; observed peak=%d", *peak)
}

// TestFanOut_EmptyCalls_ReturnsError verifies the schema-level
// minItems isn't the only enforcement — the handler itself refuses an
// empty call list so a non-validating LLM still gets a clear error.
func (s *FanOutSuite) TestFanOut_EmptyCalls_ReturnsError() {
	cat, _, _ := newFanOutCatalog()
	args := json.RawMessage(`{"calls":[]}`)
	_, err := cat.Execute(context.Background(), "fan_out", args)
	s.Require().Error(err)
}

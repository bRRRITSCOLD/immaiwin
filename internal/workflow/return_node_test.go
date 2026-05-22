package workflow

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/suite"
)

type ReturnNodeSuite struct {
	suite.Suite
}

func (s *ReturnNodeSuite) SetupSuite()    {}
func (s *ReturnNodeSuite) TearDownSuite() {}
func (s *ReturnNodeSuite) SetupTest()     {}
func (s *ReturnNodeSuite) TearDownTest()  {}

// TestReturnNodeSuite is the test entrypoint for `return` node
// behaviour: template resolution, deep-walk, OutputSchema validation.
func TestReturnNodeSuite(t *testing.T) {
	suite.Run(t, new(ReturnNodeSuite))
}

// TestRunReturn_NilPayload_PassesThroughInput verifies the default:
// no `payload` field declared → return node passes its upstream
// input through unchanged. Lets authors place a bare return node
// after a single producer to expose its output.
func (s *ReturnNodeSuite) TestRunReturn_NilPayload_PassesThroughInput() {
	out, err := runReturn(map[string]any{}, map[string]any{"x": 1}, nil)
	s.Require().NoError(err)
	s.Equal(map[string]any{"x": 1}, out)
}

// TestRunReturn_TemplatePayload_ResolvesContextRefs verifies the
// composition use case: payload references named-step outputs +
// input fields and gets a fully resolved structured value back.
func (s *ReturnNodeSuite) TestRunReturn_TemplatePayload_ResolvesContextRefs() {
	wfCtx := runCtx{
		"weather": StepContext{Output: map[string]any{"summary": "sunny", "temp_f": 75}},
		"user":    StepContext{Output: map[string]any{"name": "alice"}},
	}
	payload := map[string]any{
		"weather_summary": "{{context.weather.output.summary}}",
		"who":             "{{context.user.output.name}}",
		"echo_city":       "{{input.city}}",
	}
	out, err := runReturn(map[string]any{"payload": payload}, map[string]any{"city": "Boise"}, wfCtx)
	s.Require().NoError(err)
	got, ok := out.(map[string]any)
	s.Require().True(ok)
	s.Equal("sunny", got["weather_summary"])
	s.Equal("alice", got["who"])
	s.Equal("Boise", got["echo_city"])
}

// TestRunReturn_LoneTokenPreservesType verifies the type-preserving
// path: a lone `{{context.find.output}}` string resolves to the
// underlying slice / object, not its stringified form. Critical
// for sub_workflow consumers that expect typed JSON returns.
func (s *ReturnNodeSuite) TestRunReturn_LoneTokenPreservesType() {
	wfCtx := runCtx{
		"find": StepContext{Output: []any{map[string]any{"id": "1"}, map[string]any{"id": "2"}}},
	}
	payload := map[string]any{
		"docs": "{{context.find.output}}",
	}
	out, err := runReturn(map[string]any{"payload": payload}, nil, wfCtx)
	s.Require().NoError(err)
	got := out.(map[string]any)
	docs, ok := got["docs"].([]any)
	s.Require().True(ok, "lone-token slice ref must preserve []any type, got %T", got["docs"])
	s.Len(docs, 2)
}

// TestRunReturn_RunInputNamespace_ResolvesFromAnyDepth verifies
// the `run_input.X` namespace: a return node placed AFTER an
// intermediate step (whose output replaces `input`) can still
// reach the workflow's initial run input via `run_input.X`. This
// is the canvas Run-dialog input + sub_workflow caller-arg
// pattern — without it, deep return nodes can't see the original
// payload.
func (s *ReturnNodeSuite) TestRunReturn_RunInputNamespace_ResolvesFromAnyDepth() {
	// Simulate the engine's wfCtx-stash of run input under the
	// reserved key. Real RunWithEvents does this once per run.
	wfCtx := runCtx{
		runInputCtxKey:  StepContext{Output: map[string]any{"city": "des moines", "format": "%C,%t"}, Input: map[string]any{"city": "des moines"}},
		"weatherStep":   StepContext{Output: map[string]any{"summary": "Sunny, 75F"}},
	}
	// Return's immediate `input` is the upstream step's output —
	// no `city` field. The template must reach over it via
	// run_input.
	payload := map[string]any{
		"city":    "{{run_input.city}}",
		"format":  "{{run_input.format}}",
		"summary": "{{context.weatherStep.output.summary}}",
	}
	upstreamInput := map[string]any{"summary": "Sunny, 75F"} // no city/format here
	out, err := runReturn(map[string]any{"payload": payload}, upstreamInput, wfCtx)
	s.Require().NoError(err)
	got := out.(map[string]any)
	s.Equal("des moines", got["city"])
	s.Equal("%C,%t", got["format"])
	s.Equal("Sunny, 75F", got["summary"])
}

// TestRunReturn_DeepNested_ResolvesAtEveryLevel verifies the
// recursive walk: templates buried inside nested objects + arrays
// inside the payload all resolve.
func (s *ReturnNodeSuite) TestRunReturn_DeepNested_ResolvesAtEveryLevel() {
	wfCtx := runCtx{
		"a": StepContext{Output: "valueA"},
		"b": StepContext{Output: "valueB"},
	}
	payload := map[string]any{
		"top": "{{context.a.output}}",
		"nested": map[string]any{
			"deep": "{{context.b.output}}",
			"list": []any{"{{context.a.output}}", "literal", "{{context.b.output}}"},
		},
	}
	out, err := runReturn(map[string]any{"payload": payload}, nil, wfCtx)
	s.Require().NoError(err)
	got := out.(map[string]any)
	s.Equal("valueA", got["top"])
	nested := got["nested"].(map[string]any)
	s.Equal("valueB", nested["deep"])
	list := nested["list"].([]any)
	s.Equal("valueA", list[0])
	s.Equal("literal", list[1])
	s.Equal("valueB", list[2])
}

// TestValidateRunOutput_NoSchema_AlwaysPasses verifies back-compat:
// workflows without OutputSchema / OutputSchemaJSON accept any
// return shape — sub_workflows that never declared a return
// contract before this feature keep working.
func (s *ReturnNodeSuite) TestValidateRunOutput_NoSchema_AlwaysPasses() {
	wf := &Workflow{ID: "wf"}
	s.NoError(validateRunOutput(wf, nil))
	s.NoError(validateRunOutput(wf, map[string]any{"any": "shape"}))
	s.NoError(validateRunOutput(wf, []any{1, 2}))
}

// TestValidateRunOutput_TypedSchema_RejectsMissingRequired verifies
// the OutputSchema gate: declared required field absent in payload
// fails with ErrOutputValidation wrap.
func (s *ReturnNodeSuite) TestValidateRunOutput_TypedSchema_RejectsMissingRequired() {
	wf := &Workflow{
		ID: "wf",
		OutputSchema: []SchemaEntry{
			{Name: "summary", Type: "string", Required: true},
		},
	}
	err := validateRunOutput(wf, map[string]any{})
	s.Require().Error(err)
	s.True(errors.Is(err, ErrOutputValidation))
}

// TestValidateRunOutput_RawSchema_WinsOverTypedSchema verifies
// OutputSchemaJSON takes priority same as InputSchemaJSON. Raw
// schema's nested.x is required; typed schema would only require
// summary. Payload with summary fails under raw.
func (s *ReturnNodeSuite) TestValidateRunOutput_RawSchema_WinsOverTypedSchema() {
	wf := &Workflow{
		ID: "wf",
		OutputSchema: []SchemaEntry{
			{Name: "summary", Type: "string", Required: true},
		},
		OutputSchemaJSON: `{"type":"object","properties":{"nested":{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}},"required":["nested"]}`,
	}
	err := validateRunOutput(wf, map[string]any{"summary": "ok"})
	s.Require().Error(err)
	s.True(errors.Is(err, ErrOutputValidation))
}

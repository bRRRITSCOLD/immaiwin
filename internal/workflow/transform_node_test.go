package workflow

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

type TransformNodeSuite struct {
	suite.Suite
}

func (s *TransformNodeSuite) SetupSuite()    {}
func (s *TransformNodeSuite) TearDownSuite() {}
func (s *TransformNodeSuite) SetupTest()     {}
func (s *TransformNodeSuite) TearDownTest()  {}

// TestTransformNodeSuite is the test entrypoint for `transform` node
// behaviour. Transform shares the template-resolution machinery with
// `return`; this suite locks the contract differences (no
// one-per-workflow restriction, mid-graph usage, passthrough on
// missing payload).
func TestTransformNodeSuite(t *testing.T) {
	suite.Run(t, new(TransformNodeSuite))
}

// TestRunTransform_NilPayload_PassesThroughInput verifies an
// unconfigured transform node is a no-op rather than an error. Lets
// authors drop a transform node into the graph and configure it
// later without breaking the run.
func (s *TransformNodeSuite) TestRunTransform_NilPayload_PassesThroughInput() {
	in := map[string]any{"x": 1, "y": 2}
	out, err := runTransform(map[string]any{}, in, nil)
	s.Require().NoError(err)
	s.Equal(in, out)
}

// TestRunTransform_TemplatePayload_ResolvesContextRefs verifies the
// primary use case: trim a large upstream payload down to the keys a
// downstream agent actually needs. Both `{{input.X}}` and
// `{{context.step.output.X}}` references resolve.
func (s *TransformNodeSuite) TestRunTransform_TemplatePayload_ResolvesContextRefs() {
	wfCtx := runCtx{
		"find": StepContext{Output: map[string]any{
			"docs":   []any{map[string]any{"id": "1"}, map[string]any{"id": "2"}},
			"cursor": "bigopaquestring",
			"took":   42,
		}},
	}
	payload := map[string]any{
		"docs":  "{{context.find.output.docs}}",
		"count": "{{context.find.output.took}}",
		"echo":  "{{input.label}}",
	}
	in := map[string]any{"label": "hi"}
	out, err := runTransform(map[string]any{"payload": payload}, in, wfCtx)
	s.Require().NoError(err)
	got, ok := out.(map[string]any)
	s.Require().True(ok)
	docs, ok := got["docs"].([]any)
	s.Require().True(ok, "lone-token slice ref must preserve []any type, got %T", got["docs"])
	s.Len(docs, 2)
	s.Equal("hi", got["echo"])
	// `cursor` field was dropped — the whole point of the node.
	_, hasCursor := got["cursor"]
	s.False(hasCursor)
}

// TestRunTransform_LoneTokenPreservesType verifies a transform node
// can re-wrap a typed value (slice / object) without stringifying it.
// Without this, a transform feeding an agent's input would land a
// stringified array, breaking downstream JSON-shape expectations.
func (s *TransformNodeSuite) TestRunTransform_LoneTokenPreservesType() {
	wfCtx := runCtx{
		"http": StepContext{Output: map[string]any{
			"body": map[string]any{"items": []any{"a", "b", "c"}},
		}},
	}
	payload := map[string]any{
		"items": "{{context.http.output.body.items}}",
	}
	out, err := runTransform(map[string]any{"payload": payload}, nil, wfCtx)
	s.Require().NoError(err)
	got := out.(map[string]any)
	items, ok := got["items"].([]any)
	s.Require().True(ok, "lone-token slice ref must preserve []any type, got %T", got["items"])
	s.Len(items, 3)
}

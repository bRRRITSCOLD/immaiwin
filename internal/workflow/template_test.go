package workflow

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

type TemplateSuite struct {
	suite.Suite
}

func (s *TemplateSuite) SetupSuite()    {}
func (s *TemplateSuite) TearDownSuite() {}
func (s *TemplateSuite) SetupTest()     {}
func (s *TemplateSuite) TearDownTest()  {}

// TestTemplateSuite is the test entrypoint for applyTemplate /
// resolveTemplateValue path resolution.
func TestTemplateSuite(t *testing.T) {
	suite.Run(t, new(TemplateSuite))
}

// TestApplyTemplate_TopLevelInputKey_Substitutes verifies the
// pre-existing flat-key contract still works after the
// dotted-path rewrite.
func (s *TemplateSuite) TestApplyTemplate_TopLevelInputKey_Substitutes() {
	got := applyTemplate("hello {{input.city}}", map[string]any{"city": "Boise"}, nil)
	s.Equal("hello Boise", got)
}

// TestApplyTemplate_NestedInputPath_Substitutes verifies the
// regression fix — `{{input.json.city}}` (used by the WebSocket
// trigger template + every nested payload shape) walks dotted
// paths instead of failing through unsubstituted.
func (s *TemplateSuite) TestApplyTemplate_NestedInputPath_Substitutes() {
	input := map[string]any{
		"json": map[string]any{"city": "Boise", "seq": 42},
		"raw":  `{"city":"Boise","seq":42}`,
	}
	got := applyTemplate("city={{input.json.city}} seq={{input.json.seq}}", input, nil)
	s.Equal("city=Boise seq=42", got)
}

// TestApplyTemplate_UnknownNestedPath_LeavesTokenInPlace verifies
// an unresolved path keeps the literal `{{…}}` token in the string
// so downstream guards can still refuse it (matches pre-fix
// behaviour for unknown top-level keys; URL SSRF + dial-check
// continue to receive the literal token instead of an empty
// string that would silently dial nothing).
func (s *TemplateSuite) TestApplyTemplate_UnknownNestedPath_LeavesTokenInPlace() {
	input := map[string]any{"json": map[string]any{"city": "Boise"}}
	got := applyTemplate("u={{input.json.missing}}", input, nil)
	s.Equal("u={{input.json.missing}}", got)
}

// TestApplyTemplate_ContextNestedPath_Substitutes verifies the same
// dotted-path support reaches `{{context.<step>.<input|output|item>.<…>}}`
// — used everywhere the engine threads cross-step values.
func (s *TemplateSuite) TestApplyTemplate_ContextNestedPath_Substitutes() {
	wfCtx := runCtx{
		"prev": StepContext{
			Output: map[string]any{
				"http_request": map[string]any{
					"status_code": 200,
					"json":        map[string]any{"city": "Reno"},
				},
			},
		},
	}
	got := applyTemplate(
		"city={{context.prev.output.http_request.json.city}} code={{context.prev.output.http_request.status_code}}",
		nil, wfCtx,
	)
	s.Equal("city=Reno code=200", got)
}

// TestResolveTemplateValue_LoneNestedPath_PreservesType verifies the
// lone-token resolver returns the value WITHOUT stringifying — the
// for_each `items` selector relies on this so `{{input.payload.docs}}`
// stays a slice instead of becoming a printf-rendered string.
func (s *TemplateSuite) TestResolveTemplateValue_LoneNestedPath_PreservesType() {
	docs := []any{map[string]any{"id": 1}, map[string]any{"id": 2}}
	input := map[string]any{"payload": map[string]any{"docs": docs}}

	got, ok := resolveTemplateValue("{{input.payload.docs}}", input, nil)
	s.Require().True(ok)
	gotSlice, isSlice := got.([]any)
	s.Require().True(isSlice, "lone-token resolve must keep the slice typed; got %T", got)
	s.Len(gotSlice, 2)
}

// TestResolveTemplateValue_FlatPath_StillWorks verifies the existing
// 2-part path (input.<key>) survives the rewrite — for_each users
// who relied on `{{input.docs}}` shouldn't regress.
func (s *TemplateSuite) TestResolveTemplateValue_FlatPath_StillWorks() {
	docs := []any{map[string]any{"id": 1}}
	input := map[string]any{"docs": docs}

	got, ok := resolveTemplateValue("{{input.docs}}", input, nil)
	s.Require().True(ok)
	gotSlice, isSlice := got.([]any)
	s.Require().True(isSlice)
	s.Len(gotSlice, 1)
}

// TestResolveTemplateValue_UnknownPath_ReturnsNotOK verifies the
// resolver's escape hatch — unknown nested paths short-circuit so
// the caller (e.g. for_each.items) can fall back to its pre-template
// behaviour.
func (s *TemplateSuite) TestResolveTemplateValue_UnknownPath_ReturnsNotOK() {
	_, ok := resolveTemplateValue("{{input.payload.missing.deep}}", map[string]any{"payload": map[string]any{}}, nil)
	s.False(ok)
}

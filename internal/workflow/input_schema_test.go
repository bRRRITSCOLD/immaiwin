package workflow

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/suite"
)

type InputSchemaValidationSuite struct {
	suite.Suite
}

func (s *InputSchemaValidationSuite) SetupSuite()    {}
func (s *InputSchemaValidationSuite) TearDownSuite() {}
func (s *InputSchemaValidationSuite) SetupTest()     {}
func (s *InputSchemaValidationSuite) TearDownTest()  {}

// TestInputSchemaValidationSuite is the test entrypoint for
// workflow-level RUN INPUT schema validation.
func TestInputSchemaValidationSuite(t *testing.T) {
	suite.Run(t, new(InputSchemaValidationSuite))
}

// TestValidateRunInput_NoSchema_AlwaysPasses verifies the
// back-compat contract: workflows without an InputSchema /
// InputSchemaJSON accept any input shape, including nil. Removing
// this test would silently break every workflow authored before
// the feature shipped.
func (s *InputSchemaValidationSuite) TestValidateRunInput_NoSchema_AlwaysPasses() {
	wf := &Workflow{ID: "wf"}
	s.NoError(validateRunInput(wf, nil))
	s.NoError(validateRunInput(wf, map[string]any{"any": "shape"}))
	s.NoError(validateRunInput(wf, []any{1, 2, 3}))
	s.NoError(validateRunInput(wf, "string"))
}

// TestValidateRunInput_TypedSchema_RejectsMissingRequired verifies
// the SchemaEntry[] path: declaring a `Required: true` field fails
// validation when the input omits that key.
func (s *InputSchemaValidationSuite) TestValidateRunInput_TypedSchema_RejectsMissingRequired() {
	wf := &Workflow{
		ID: "wf",
		InputSchema: []SchemaEntry{
			{Name: "city", Type: "string", Required: true},
		},
	}
	err := validateRunInput(wf, map[string]any{})
	s.Require().Error(err)
	s.True(errors.Is(err, ErrInputValidation))
}

// TestValidateRunInput_TypedSchema_AcceptsValidInput verifies the
// happy-path counterpart: input matching the declared shape passes.
func (s *InputSchemaValidationSuite) TestValidateRunInput_TypedSchema_AcceptsValidInput() {
	wf := &Workflow{
		ID: "wf",
		InputSchema: []SchemaEntry{
			{Name: "city", Type: "string", Required: true},
			{Name: "unit", Type: "enum", Enum: []string{"F", "C"}},
		},
	}
	s.NoError(validateRunInput(wf, map[string]any{"city": "Boise"}))
	s.NoError(validateRunInput(wf, map[string]any{"city": "Boise", "unit": "F"}))
}

// TestValidateRunInput_TypedSchema_EnumRefusesOutOfRange verifies
// enum constraints flow through to the generated JSON Schema —
// inputs outside the declared set fail.
func (s *InputSchemaValidationSuite) TestValidateRunInput_TypedSchema_EnumRefusesOutOfRange() {
	wf := &Workflow{
		ID: "wf",
		InputSchema: []SchemaEntry{
			{Name: "unit", Type: "enum", Enum: []string{"F", "C"}, Required: true},
		},
	}
	err := validateRunInput(wf, map[string]any{"unit": "K"})
	s.Require().Error(err)
	s.True(errors.Is(err, ErrInputValidation))
}

// TestValidateRunInput_RawSchema_WinsOverTypedSchema verifies
// InputSchemaJSON takes priority when both are set. The raw schema
// here requires `nested.x`; the typed schema would require only
// `city`. Input with `city` but no `nested` must fail under raw.
func (s *InputSchemaValidationSuite) TestValidateRunInput_RawSchema_WinsOverTypedSchema() {
	wf := &Workflow{
		ID: "wf",
		InputSchema: []SchemaEntry{
			{Name: "city", Type: "string", Required: true},
		},
		InputSchemaJSON: `{"type":"object","properties":{"nested":{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}},"required":["nested"]}`,
	}
	err := validateRunInput(wf, map[string]any{"city": "Boise"})
	s.Require().Error(err)
	s.True(errors.Is(err, ErrInputValidation))
}

// TestValidateRunInput_RawSchema_AcceptsNestedInput verifies the
// raw-JSON-Schema path supports the nested / array structures that
// the flat SchemaEntry editor can't express.
func (s *InputSchemaValidationSuite) TestValidateRunInput_RawSchema_AcceptsNestedInput() {
	wf := &Workflow{
		ID:              "wf",
		InputSchemaJSON: `{"type":"object","properties":{"tags":{"type":"array","items":{"type":"string"}}},"required":["tags"]}`,
	}
	s.NoError(validateRunInput(wf, map[string]any{"tags": []any{"a", "b", "c"}}))
}

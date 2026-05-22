// Package workflow — runtime input-schema validation.
//
// Workflows declare their RUN INPUT contract via either:
//
//   - `Workflow.InputSchemaJSON` — raw JSON Schema, wins when set.
//     Supports nested objects / arrays / $ref / oneOf / anyOf — the
//     full JSON Schema surface.
//
//   - `Workflow.InputSchema` — flat SchemaEntry[]. Converted to a
//     JSON Schema on the fly (same shape sub_workflow nodes derive
//     for their as_tool blocks).
//
// Empty both → legacy free-form, no validation.
//
// Validation fires at dispatch time inside RunWithEvents, before any
// node runs. A failure short-circuits the run with a typed error
// the caller can detect via errors.Is(err, ErrInputValidation) and
// surface appropriately (400 from webhook handler, tool_result
// error in sub_workflow, drop+log in WS / RMQ / Redis subscribe,
// etc.).

package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ErrInputValidation is the sentinel callers detect to differentiate
// "input didn't match the schema" from any other engine error.
// Dispatchers can map it to trigger-specific fail policies
// (HTTP 400 / WS frame drop / RMQ nack / sub_workflow tool error).
var ErrInputValidation = errors.New("workflow: input schema validation failed")

// ErrOutputValidation mirrors ErrInputValidation for the workflow's
// declared return contract. Surfaced from dispatchSubWorkflow when
// the return-node payload doesn't match OutputSchema /
// OutputSchemaJSON.
var ErrOutputValidation = errors.New("workflow: output schema validation failed")

// compileInputSchema returns the compiled JSON Schema for a
// workflow's declared input contract, or (nil, nil) when the
// workflow declares no schema. Priority: InputSchemaJSON (raw) >
// InputSchema (typed SchemaEntry[]).
//
// Compiled per-call — the call sites use it once per dispatch which
// is cheap; if profiling later shows it's hot we can cache by
// workflow id+version.
func compileInputSchema(wf *Workflow) (*jsonschema.Schema, error) {
	if wf == nil {
		return nil, nil
	}
	var raw json.RawMessage
	if wf.InputSchemaJSON != "" {
		raw = json.RawMessage(wf.InputSchemaJSON)
	} else if len(wf.InputSchema) > 0 {
		derived, err := json.Marshal(inputSchemaToJSONSchema(wf.InputSchema))
		if err != nil {
			return nil, fmt.Errorf("derive json schema from input_schema: %w", err)
		}
		raw = derived
	} else {
		return nil, nil
	}
	return compileToolSchema(raw)
}

// inputSchemaToJSONSchema converts the workflow's flat SchemaEntry[]
// declaration into a JSON-Schema object. Mirrors the front-end's
// `inputSchemaToJSON` so the schema the engine validates against
// matches the schema sub_workflow consumers see.
func inputSchemaToJSONSchema(entries []SchemaEntry) map[string]any {
	properties := map[string]any{}
	required := []string{}
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		prop := map[string]any{}
		switch e.Type {
		case "number":
			prop["type"] = "number"
		case "boolean":
			prop["type"] = "boolean"
		case "enum":
			prop["type"] = "string"
			if len(e.Enum) > 0 {
				prop["enum"] = e.Enum
			}
		default:
			prop["type"] = "string"
		}
		if e.Description != "" {
			prop["description"] = e.Description
		}
		properties[e.Name] = prop
		if e.Required {
			required = append(required, e.Name)
		}
	}
	out := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// validateRunInput runs the input through the workflow's compiled
// schema. Returns nil when no schema is declared (legacy
// free-form) OR when input validates. Returns a wrapped
// ErrInputValidation on schema failure.
//
// `input` is the value the executor will inject as the trigger
// node's output — same shape RunWithEvents already accepts as
// `initialInput[0]`. Nil input is validated as `null`, which fails
// any schema with required fields and passes a permissive schema —
// matching JSON-Schema semantics.
//
// Trigger note: cron emits no payload (nil input). A cron-driven
// workflow that declares an input_schema with required fields will
// fail validation every fire. Workflow authors should not declare
// input_schema on cron-only workflows.
func validateRunInput(wf *Workflow, input any) error {
	schema, err := compileInputSchema(wf)
	if err != nil {
		return fmt.Errorf("compile input schema: %w", err)
	}
	if schema == nil {
		return nil
	}
	// jsonschema/v6 wants the input decoded the same way the schema
	// is — use json.Number so number-typed schemas match without
	// float64-precision loss.
	encoded, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("marshal input for validation: %w", err)
	}
	var decoded any
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		return fmt.Errorf("decode input for validation: %w", err)
	}
	if err := schema.Validate(decoded); err != nil {
		return fmt.Errorf("%w: %s", ErrInputValidation, err.Error())
	}
	return nil
}

// compileOutputSchema mirrors compileInputSchema for the OUTPUT
// contract. Priority: OutputSchemaJSON (raw) > OutputSchema
// (typed). Nil/nil when neither is set.
func compileOutputSchema(wf *Workflow) (*jsonschema.Schema, error) {
	if wf == nil {
		return nil, nil
	}
	var raw json.RawMessage
	if wf.OutputSchemaJSON != "" {
		raw = json.RawMessage(wf.OutputSchemaJSON)
	} else if len(wf.OutputSchema) > 0 {
		derived, err := json.Marshal(inputSchemaToJSONSchema(wf.OutputSchema))
		if err != nil {
			return nil, fmt.Errorf("derive json schema from output_schema: %w", err)
		}
		raw = derived
	} else {
		return nil, nil
	}
	return compileToolSchema(raw)
}

// validateRunOutput validates the sub_workflow's resolved return
// payload against the declared OutputSchema. Mirrors
// validateRunInput's semantics: nil schema = no validation; nil
// output validates as `null`.
func validateRunOutput(wf *Workflow, output any) error {
	schema, err := compileOutputSchema(wf)
	if err != nil {
		return fmt.Errorf("compile output schema: %w", err)
	}
	if schema == nil {
		return nil
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return fmt.Errorf("marshal output for validation: %w", err)
	}
	var decoded any
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		return fmt.Errorf("decode output for validation: %w", err)
	}
	if err := schema.Validate(decoded); err != nil {
		return fmt.Errorf("%w: %s", ErrOutputValidation, err.Error())
	}
	return nil
}

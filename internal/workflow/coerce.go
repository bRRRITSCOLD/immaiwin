package workflow

import (
	"reflect"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// mapAny normalises a value loaded from BSON or JSON into a map[string]any.
//
// BSON v2 decodes nested objects inside a `map[string]any` field as `bson.D`
// (an ordered slice of key-value pairs), NOT as `map[string]any`. JSON decode
// (e.g. UI imports straight into a Go struct) yields `map[string]any`
// directly. Top-level Mongo M maps decode to `bson.M`. This helper accepts
// all three forms — call it from anywhere `data["…"]` is expected to be a
// nested object so the same code path works whether the workflow was just
// imported (JSON) or loaded from Mongo (BSON).
//
// See `.private/ai-automation/PROGRESS.md` (Tier C / "BSON nested-map sweep")
// for the broader story; the original miss was `agent_tools.go::asToolDef`,
// which silently dropped the `as_tool` block on every Mongo-loaded workflow.
func mapAny(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case nil:
		return nil, false
	case map[string]any:
		return m, true
	case bson.M:
		return map[string]any(m), true
	case bson.D:
		out := make(map[string]any, len(m))
		for _, el := range m {
			out[el.Key] = el.Value
		}
		return out, true
	}
	return nil, false
}

// sliceAny normalises a value loaded from BSON or JSON into a []any.
//
// BSON `bson.A` is `type A []interface{}` — a NAMED type, not a literal
// `[]any`, so a plain type assertion `v.([]any)` fails for Mongo-decoded
// arrays. This helper handles both forms. Returns ok=false for nil so
// callers can distinguish "missing" from "empty list".
func sliceAny(v any) ([]any, bool) {
	switch a := v.(type) {
	case nil:
		return nil, false
	case []any:
		return a, true
	case bson.A:
		return []any(a), true
	}
	return nil, false
}

// expandSlice flattens any slice/array-kinded value into []any.
//
// Unlike sliceAny (which only knows []any + bson.A) this uses reflection
// so concrete element types survive a round-trip — e.g. mongo_request
// `find` returns its docs as []bson.M, mongo `distinct` as []any of
// scalars, a JSON import as []any. The for_each `items` selector resolves
// to whatever the upstream node produced, so it needs the wide net.
//
// ok=false for nil or any non-slice/array value, letting the caller fall
// back (for_each treats a non-slice as "iterate the single value once",
// matching toSlice's contract).
func expandSlice(v any) ([]any, bool) {
	if v == nil {
		return nil, false
	}
	if s, ok := sliceAny(v); ok {
		return s, true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = rv.Index(i).Interface()
		}
		return out, true
	}
	return nil, false
}

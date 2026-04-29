package workflow

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// evaluateAssertions runs every assertion in `as` against the resolved
// run data and returns the list of failure messages (empty = all pass).
//
// `agentOutput` is the final agent node output (typically the
// `map[string]any{"output": "...", "usage": {...}, ...}` shape from
// `runAIAgent`); `stepsByID` is keyed by node_id to the StepResult that
// node produced. The eval runner builds both from the WorkflowRun
// record before calling this.
func evaluateAssertions(as []Assertion, agentOutput any, stepsByID map[string]StepResult) []string {
	var fails []string
	for i, a := range as {
		target, err := resolveAssertionTarget(a, agentOutput, stepsByID)
		if err != nil {
			fails = append(fails, fmt.Sprintf("assertion[%d] (%s): target unresolved: %v", i, a.Type, err))
			continue
		}
		if msg := runAssertion(a, target); msg != "" {
			fails = append(fails, fmt.Sprintf("assertion[%d] (%s): %s", i, a.Type, msg))
		}
	}
	return fails
}

// resolveAssertionTarget picks the value to assert against based on the
// assertion's `Target` + `NodeID` selector.
func resolveAssertionTarget(a Assertion, agentOutput any, stepsByID map[string]StepResult) (any, error) {
	switch a.Target {
	case "", "agent_output":
		return agentOutput, nil
	case "step":
		if a.NodeID == "" {
			return nil, fmt.Errorf("target=step requires node_id")
		}
		sr, ok := stepsByID[a.NodeID]
		if !ok {
			return nil, fmt.Errorf("no StepResult for node_id %q", a.NodeID)
		}
		return sr.Output, nil
	default:
		return nil, fmt.Errorf("unknown target %q", a.Target)
	}
}

// runAssertion executes one predicate and returns a non-empty failure
// reason on mismatch (empty string = pass). Predicates are deliberately
// small + total-orderable so authors can compose them without a DSL.
func runAssertion(a Assertion, target any) string {
	switch a.Type {
	case "contains":
		text := stringify(target)
		if !strings.Contains(text, a.Value) {
			return fmt.Sprintf("expected text to contain %q; got %q", a.Value, truncate(text, 200))
		}
	case "not_contains":
		text := stringify(target)
		if strings.Contains(text, a.Value) {
			return fmt.Sprintf("expected text NOT to contain %q; got %q", a.Value, truncate(text, 200))
		}
	case "regex":
		text := stringify(target)
		re, err := regexp.Compile(a.Value)
		if err != nil {
			return fmt.Sprintf("invalid regex %q: %v", a.Value, err)
		}
		if !re.MatchString(text) {
			return fmt.Sprintf("expected text to match /%s/; got %q", a.Value, truncate(text, 200))
		}
	case "json_path_eq":
		got, err := jsonPath(target, a.Path)
		if err != nil {
			return err.Error()
		}
		if stringify(got) != a.Value {
			return fmt.Sprintf("expected %s == %q; got %q", a.Path, a.Value, stringify(got))
		}
	case "json_path_exists":
		_, err := jsonPath(target, a.Path)
		if err != nil {
			return fmt.Sprintf("path %s missing: %v", a.Path, err)
		}
	default:
		return fmt.Sprintf("unknown assertion type %q", a.Type)
	}
	return ""
}

// jsonPath walks a dotted path through a value that's been JSON-decoded
// (or BSON-decoded) into nested maps. Uses the existing `mapAny` helper
// so bson.D / bson.M / map[string]any all resolve identically — same
// pitfall the executor sweep covered, applied here.
func jsonPath(v any, path string) (any, error) {
	if path == "" {
		return v, nil
	}
	parts := strings.Split(path, ".")
	cur := v
	for i, p := range parts {
		m, ok := mapAny(cur)
		if !ok {
			return nil, fmt.Errorf("path %s: segment %d (%q) parent is not an object (%T)",
				path, i, p, cur)
		}
		next, ok := m[p]
		if !ok {
			return nil, fmt.Errorf("path %s: segment %d (%q) not found", path, i, p)
		}
		cur = next
	}
	return cur, nil
}

// stringify converts an arbitrary value to a string for text-based
// assertions. Plain strings round-trip unchanged; other shapes get JSON-
// encoded so `contains`/`regex` can match against the encoded form.
func stringify(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// truncate caps a string at n runes for failure-message readability.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

package skills

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// slugPattern matches a skill ID. Two forms accepted:
//
//	weather-pro              → unscoped slug
//	acme/weather-pro         → namespace/slug
//
// Each segment is `[a-z0-9][a-z0-9-]*` (must start with alphanumeric, then
// alphanumeric or dash). Reserved namespaces (e.g. `core`, `internal`) are
// rejected separately by isReservedNamespace below — keeping them out of
// the regex lets us evolve the reservation list without touching the
// pattern.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*(\/[a-z0-9][a-z0-9-]*)?$`)

// semverPattern is a permissive SemVer 2.0 matcher (ignores prerelease /
// build-metadata edge cases that the resolver layer will validate properly).
// Tightened in P1.10 when SemVer ranges land.
var semverPattern = regexp.MustCompile(`^\d+\.\d+\.\d+([-+][0-9A-Za-z.\-]+)?$`)

// reservedNamespaces are skill IDs the platform reserves for itself.
var reservedNamespaces = map[string]struct{}{
	"core":     {},
	"internal": {},
	"system":   {},
	"immaiwin": {},
}

// supportedLanguages mirrors sandbox.Language. Hard-coded here to avoid an
// import cycle (sandbox depends on workflow which will depend on skills).
// Keep in sync with internal/sandbox/types.go.
var supportedLanguages = map[string]struct{}{
	"javascript": {},
	"python":     {},
	"golang":     {},
	"rust":       {},
	"php":        {},
}

// CurrentAPIVersion is the highest manifest API version this build
// implements. Bump when the manifest schema changes in a backwards-
// incompatible way.
const CurrentAPIVersion = 1

// Validate checks a parsed manifest against the rules defined in
// SKILLS-AND-PLUGINS-PLAN.md §1.2. Errors are joined so a misconfigured
// manifest surfaces all problems in one shot rather than playing
// whack-a-mole through repeated load attempts.
//
// On success, ForwardCompat fields populated for v2/v2.5 surfaces emit an
// info-level breadcrumb but do not fail validation — that's the contract
// for forward-compatible manifests on a v1 platform.
func Validate(m *Manifest) error {
	var errs []error

	// --- Identity ---
	if m.ID == "" {
		errs = append(errs, errors.New("id is required"))
	} else {
		if !slugPattern.MatchString(m.ID) {
			errs = append(errs, fmt.Errorf("id %q is not a valid slug (pattern: %s)", m.ID, slugPattern.String()))
		}
		ns, _, hasNS := strings.Cut(m.ID, "/")
		if hasNS {
			if _, reserved := reservedNamespaces[ns]; reserved {
				errs = append(errs, fmt.Errorf("id %q uses reserved namespace %q", m.ID, ns))
			}
		} else {
			if _, reserved := reservedNamespaces[m.ID]; reserved {
				errs = append(errs, fmt.Errorf("id %q is a reserved name", m.ID))
			}
		}
	}

	if m.Version == "" {
		errs = append(errs, errors.New("version is required"))
	} else if !semverPattern.MatchString(m.Version) {
		errs = append(errs, fmt.Errorf("version %q is not valid semver", m.Version))
	}

	if m.Name == "" {
		errs = append(errs, errors.New("name is required"))
	}
	if m.Description == "" {
		errs = append(errs, errors.New("description is required"))
	}
	if m.Author.Name == "" {
		errs = append(errs, errors.New("author.name is required"))
	}
	if m.License == "" {
		errs = append(errs, errors.New("license is required"))
	}

	// --- Compatibility ---
	if m.APIVersion == 0 {
		errs = append(errs, errors.New("api_version is required"))
	} else if m.APIVersion > CurrentAPIVersion {
		errs = append(errs, fmt.Errorf("api_version %d > current %d (skill targets a newer platform)", m.APIVersion, CurrentAPIVersion))
	}
	if m.MinPlatformVersion != "" && !semverPattern.MatchString(m.MinPlatformVersion) {
		errs = append(errs, fmt.Errorf("min_platform_version %q is not valid semver", m.MinPlatformVersion))
	}
	for i, dep := range m.Dependencies {
		if dep.ID == "" {
			errs = append(errs, fmt.Errorf("dependencies[%d].id is required", i))
		} else if !slugPattern.MatchString(dep.ID) {
			errs = append(errs, fmt.Errorf("dependencies[%d].id %q is not a valid slug", i, dep.ID))
		}
		if dep.Range == "" {
			errs = append(errs, fmt.Errorf("dependencies[%d].version (range) is required", i))
		}
	}

	// --- Tools ---
	seenToolIDs := map[string]struct{}{}
	for i, tool := range m.Tools {
		prefix := fmt.Sprintf("tools[%d]", i)
		if tool.ID == "" {
			errs = append(errs, fmt.Errorf("%s.id is required", prefix))
		} else {
			if _, dup := seenToolIDs[tool.ID]; dup {
				errs = append(errs, fmt.Errorf("%s.id %q duplicated within manifest", prefix, tool.ID))
			}
			seenToolIDs[tool.ID] = struct{}{}
		}
		if tool.File == "" {
			errs = append(errs, fmt.Errorf("%s.file is required", prefix))
		} else if strings.Contains(tool.File, "..") || strings.HasPrefix(tool.File, "/") {
			errs = append(errs, fmt.Errorf("%s.file %q must be a relative path inside the bundle", prefix, tool.File))
		}
		if tool.Language == "" {
			errs = append(errs, fmt.Errorf("%s.language is required", prefix))
		} else if _, ok := supportedLanguages[tool.Language]; !ok {
			errs = append(errs, fmt.Errorf("%s.language %q is not a supported sandbox language", prefix, tool.Language))
		}
		if tool.Description == "" {
			errs = append(errs, fmt.Errorf("%s.description is required (LLMs use it to choose tools)", prefix))
		}
		if len(tool.InputSchema) > 0 {
			if err := validateJSONSchema(tool.InputSchema); err != nil {
				errs = append(errs, fmt.Errorf("%s.input_schema invalid: %w", prefix, err))
			}
		}
		if len(tool.OutputSchema) > 0 {
			if err := validateJSONSchema(tool.OutputSchema); err != nil {
				errs = append(errs, fmt.Errorf("%s.output_schema invalid: %w", prefix, err))
			}
		}
		if tool.TimeoutSecs < 0 {
			errs = append(errs, fmt.Errorf("%s.timeout_secs must be >= 0", prefix))
		}
	}

	// --- Capabilities ---
	for i, sec := range m.Capabilities.Secrets {
		if sec.Name == "" {
			errs = append(errs, fmt.Errorf("capabilities.secrets[%d].name is required", i))
		}
		if sec.Type == "" {
			errs = append(errs, fmt.Errorf("capabilities.secrets[%d].type is required", i))
		}
	}

	// --- Config (author-bound knobs) ---
	seenCfgNames := map[string]struct{}{}
	for i, c := range m.Config {
		prefix := fmt.Sprintf("config[%d]", i)
		if c.Name == "" {
			errs = append(errs, fmt.Errorf("%s.name is required", prefix))
		} else {
			if _, dup := seenCfgNames[c.Name]; dup {
				errs = append(errs, fmt.Errorf("%s.name %q duplicated", prefix, c.Name))
			}
			seenCfgNames[c.Name] = struct{}{}
		}
		switch c.Type {
		case "string", "number", "boolean":
			// ok
		case "":
			errs = append(errs, fmt.Errorf("%s.type is required (one of string/number/boolean)", prefix))
		default:
			errs = append(errs, fmt.Errorf("%s.type %q is not supported (use string/number/boolean)", prefix, c.Type))
		}
		if len(c.Enum) > 0 && c.Type != "string" {
			errs = append(errs, fmt.Errorf("%s.enum is only supported for type=string", prefix))
		}
	}

	// --- Forward-compat breadcrumb ---
	if m.Forward.HasForward() {
		slog.Info("skills: manifest declares forward-compat surfaces ignored on this platform",
			"id", m.ID,
			"version", m.Version,
			"tool_policy", m.Forward.ToolPolicy != "",
			"agent_loop", m.Forward.AgentLoop != "",
			"trigger_types", len(m.Forward.TriggerTypes),
			"connection_types", len(m.Forward.ConnectionTypes),
		)
	}

	if len(errs) > 0 {
		return fmt.Errorf("skills: manifest validation failed: %w", errors.Join(errs...))
	}
	return nil
}

// validateJSONSchema confirms that `raw` is itself a valid JSON Schema
// document by attempting to compile it. The compiled schema is discarded
// here; callers that need to validate inputs should call CompileSchema.
func validateJSONSchema(raw json.RawMessage) error {
	c := jsonschema.NewCompiler()
	if err := c.AddResource("schema.json", bytesToReader(raw)); err != nil {
		return err
	}
	if _, err := c.Compile("schema.json"); err != nil {
		return err
	}
	return nil
}

// CompileSchema returns a compiled JSON Schema validator from a raw spec.
// Used at agent-tool-call time to validate args before dispatch.
func CompileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty schema")
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("schema.json", bytesToReader(raw)); err != nil {
		return nil, err
	}
	return c.Compile("schema.json")
}

// bytesToReader converts raw JSON bytes into the `any` shape the v6
// jsonschema compiler accepts via AddResource. The library expects a
// pre-decoded JSON value, not a stream — easier to decode once here than
// have every caller do it.
func bytesToReader(raw json.RawMessage) any {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		// Caller already validates this is non-empty; an unparseable schema
		// becomes a string the compiler will reject — preserves the error
		// path without panicking here.
		return string(raw)
	}
	return v
}

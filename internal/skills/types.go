// Package skills implements the skill bundle system that lets workflow
// agents extend their tool catalog with reusable, distributable units of
// (tools + system-prompt fragments + memory policy + capability declarations).
//
// See `.private/ai-automation/SKILLS-AND-PLUGINS-PLAN.md` for the full design;
// this package implements P1.9 (manifest types) and is the foundation for
// P1.10 (Source / Resolver) and P1.11 (agent integration).
package skills

import (
	"encoding/json"
	"time"
)

// Manifest is the parsed `manifest.yaml` for a skill bundle.
//
// The manifest is the single source of truth for a skill's identity,
// version, contributed tools/prompts/memory policy, and required runtime
// capabilities. Validation is strict: unknown top-level keys that match a
// known v2/v2.5 surface are parsed into `Forward` and ignored at run time
// with an info-level breadcrumb (so a v2 skill degrades cleanly on a v1
// platform); any other unknown key fails the load.
//
// Canonical YAML schema lives in SKILLS-AND-PLUGINS-PLAN.md §1.2. The Go
// struct mirrors that shape one-to-one so YAML round-trips cleanly into
// MongoDB via BSON tags below.
type Manifest struct {
	// --- Identity ------------------------------------------------------------
	ID          string `yaml:"id"          json:"id"          bson:"id"`
	Version     string `yaml:"version"     json:"version"     bson:"version"`
	Name        string `yaml:"name"        json:"name"        bson:"name"`
	Description string `yaml:"description" json:"description" bson:"description"`
	Author      Author `yaml:"author"      json:"author"      bson:"author"`
	License     string `yaml:"license"     json:"license"     bson:"license"`

	// --- Compatibility -------------------------------------------------------
	APIVersion         int          `yaml:"api_version"          json:"api_version"          bson:"api_version"`
	MinPlatformVersion string       `yaml:"min_platform_version" json:"min_platform_version" bson:"min_platform_version,omitempty"`
	Dependencies       []Dependency `yaml:"dependencies"         json:"dependencies"         bson:"dependencies,omitempty"`

	// --- Contributed surface -------------------------------------------------
	Tools  []Tool  `yaml:"tools"  json:"tools"            bson:"tools,omitempty"`
	Prompt *Prompt `yaml:"prompt" json:"prompt,omitempty" bson:"prompt,omitempty"`
	Memory *Memory `yaml:"memory" json:"memory,omitempty" bson:"memory,omitempty"`

	// --- Capabilities (declared, sandbox-enforced) ---------------------------
	Capabilities Capabilities `yaml:"capabilities" json:"capabilities" bson:"capabilities"`

	// --- Author-bound config -------------------------------------------------
	// Skill-level knobs the WORKFLOW AUTHOR sets once per agent (e.g. default
	// tone, locale, max-rows). Distinct from `tools[].input_schema` (which
	// the LLM picks on each call) and from `capabilities.secrets[]` (which
	// resolve through Connection records). Required entries hard-fail at
	// agent load time when the agent's `data.skill_config[<slug>][<name>]`
	// binding is missing — same precedent as secrets.
	//
	// Values flow into the sandbox as `config["<sanitized_slug>__<name>"]`
	// to avoid collisions when multiple skills define same-named config.
	Config []ConfigDef `yaml:"config" json:"config,omitempty" bson:"config,omitempty"`

	// --- Forward-compatible surfaces (v2 / v2.5) ----------------------------
	// Parsed but ignored in P1; preserved so a v2 skill on a v1 platform
	// degrades cleanly. Loader emits an info-level breadcrumb when these are
	// non-empty. Order-preserving capture keeps the canonical YAML around for
	// future versions to read without re-parsing.
	Forward ForwardCompat `yaml:",inline" json:"-" bson:"-"`
}

// Author identifies who published the skill.
type Author struct {
	Name  string `yaml:"name"  json:"name"            bson:"name"`
	Email string `yaml:"email" json:"email,omitempty" bson:"email,omitempty"`
	URL   string `yaml:"url"   json:"url,omitempty"   bson:"url,omitempty"`
}

// Dependency is a load-time requirement on another skill.
// Range is a SemVer range (npm-style: "^1.2.0", "~2.5.0", "3.1.7", "*").
type Dependency struct {
	ID    string `yaml:"id"      json:"id"      bson:"id"`
	Range string `yaml:"version" json:"version" bson:"version"`
}

// Tool is a single LLM-callable unit contributed by the skill.
//
// `File` is a relative path inside the skill bundle pointing at executable
// source; `Language` selects the sandbox runtime image (must be one of
// `sandbox.LangJavaScript` / `LangPython` / `LangGolang` / `LangRust` /
// `LangPHP`).
//
// `InputSchema` is rendered to the LLM as the tool's parameter spec and used
// to validate args before dispatch. `OutputSchema` is optional and gates the
// observation returned to the LLM (validation only — agent loop sees the
// result either way for now).
type Tool struct {
	ID           string          `yaml:"id"            json:"id"                      bson:"id"`
	File         string          `yaml:"file"          json:"file"                    bson:"file"`
	Language     string          `yaml:"language"      json:"language"                bson:"language"`
	Description  string          `yaml:"description"   json:"description"             bson:"description"`
	InputSchema  json.RawMessage `yaml:"input_schema"  json:"input_schema"            bson:"input_schema"`
	OutputSchema json.RawMessage `yaml:"output_schema" json:"output_schema,omitempty" bson:"output_schema,omitempty"`
	TimeoutSecs  int             `yaml:"timeout_secs"  json:"timeout_secs,omitempty" bson:"timeout_secs,omitempty"`
}

// Prompt is a system-prompt fragment appended to the agent's system prompt
// when the skill is enabled.
type Prompt struct {
	Fragment string `yaml:"fragment" json:"fragment" bson:"fragment"`
}

// Memory is the skill's recommended memory policy. Agent-node-level config
// wins when both are set; skill defaults fill in the gaps.
type Memory struct {
	DefaultPolicy *MemoryPolicy `yaml:"default_policy" json:"default_policy,omitempty" bson:"default_policy,omitempty"`
}

// MemoryPolicy is a reusable shape for chat-memory tuning knobs.
type MemoryPolicy struct {
	MaxMessages  int `yaml:"max_messages"  json:"max_messages,omitempty"  bson:"max_messages,omitempty"`
	SummarizeAt  int `yaml:"summarize_at"  json:"summarize_at,omitempty"  bson:"summarize_at,omitempty"`
}

// Capabilities declares what the skill needs at run time. The values are
// enforced by the sandbox layer at pod-create time; declaring less than the
// skill actually needs causes failures, declaring more than needed is
// rejected at install time when policy enforcement is enabled.
type Capabilities struct {
	Network NetworkCapability `yaml:"network" json:"network" bson:"network"`
	Storage StorageCapability `yaml:"storage" json:"storage" bson:"storage"`
	Secrets []SecretRequest   `yaml:"secrets" json:"secrets,omitempty" bson:"secrets,omitempty"`
}

// NetworkCapability declares outbound network requirements.
//
// In P1 enforcement is binary: any non-empty `Egress` list (including the
// wildcard `"*"`) enables the sandbox `network=allow` label. P2 lands the
// per-host allowlist via DNS-aware NetworkPolicy + in-pod proxy.
type NetworkCapability struct {
	Egress []string `yaml:"egress" json:"egress,omitempty" bson:"egress,omitempty"`
}

// Allow returns true when the skill needs any outbound network access.
// Used by the sandbox runtime to set the deny/allow label on the pod.
func (n NetworkCapability) Allow() bool { return len(n.Egress) > 0 }

// StorageCapability declares whether a tenant-scoped tmpfs `/data` should
// be mounted into the sandbox pod.
type StorageCapability struct {
	Read  bool `yaml:"read"  json:"read"  bson:"read"`
	Write bool `yaml:"write" json:"write" bson:"write"`
}

// SecretRequest declares a secret the skill needs at run time. The tenant
// must supply a Connection of the matching `Type` whose ID maps to `Name`;
// the resolved secret value is injected as a sandbox `config[Name]` entry.
type SecretRequest struct {
	Name        string `yaml:"name"        json:"name"        bson:"name"`
	Type        string `yaml:"type"        json:"type"        bson:"type"`
	Description string `yaml:"description" json:"description,omitempty" bson:"description,omitempty"`
}

// ConfigDef declares an author-bound config knob. The workflow author
// supplies a literal value (not a Connection ID) via the agent node's
// `data.skill_config[slug][name]`. Distinct from a tool input_schema —
// these values do NOT vary per-call; the LLM never sees them.
//
// Type semantics (P1, deliberately small):
//   - "string"  → free-form text; constrained by `Enum` when set.
//   - "number"  → JSON number; coerced to a stringified value in sandbox config.
//   - "boolean" → JSON true/false; coerced to "true"/"false" in sandbox config.
//
// All values land in the sandbox `config` map as strings (matching the
// existing `config: map[string]string` shape). Author scripts coerce as
// needed.
type ConfigDef struct {
	Name        string   `yaml:"name"        json:"name"        bson:"name"`
	Type        string   `yaml:"type"        json:"type"        bson:"type"`
	Description string   `yaml:"description" json:"description,omitempty" bson:"description,omitempty"`
	Required    bool     `yaml:"required"    json:"required,omitempty"    bson:"required,omitempty"`
	Default     any      `yaml:"default"     json:"default,omitempty"     bson:"default,omitempty"`
	Enum        []string `yaml:"enum"        json:"enum,omitempty"        bson:"enum,omitempty"`
}

// ForwardCompat captures top-level keys that target a future API surface
// (v2 tool_policy, v2 agent_loop, v2.5 trigger_types, v2.5 connection_types).
// The loader logs a breadcrumb when any of these fields are populated on the
// active platform version but does not fail the load — that lets a skill
// bundle ship one manifest covering multiple platform releases.
type ForwardCompat struct {
	ToolPolicy      string   `yaml:"tool_policy,omitempty"`
	AgentLoop       string   `yaml:"agent_loop,omitempty"`
	TriggerTypes    []string `yaml:"trigger_types,omitempty"`
	ConnectionTypes []string `yaml:"connection_types,omitempty"`
}

// HasForward reports whether any forward-compat surface is non-empty.
func (f ForwardCompat) HasForward() bool {
	return f.ToolPolicy != "" || f.AgentLoop != "" || len(f.TriggerTypes) > 0 || len(f.ConnectionTypes) > 0
}

// --- Mongo persistence shapes (used by registry / install / binding stores in P1.10) ---

// SkillRecord is a row in the `skills_registry` collection — the
// platform-wide library of all known skill versions.
type SkillRecord struct {
	ID          string    `bson:"_id"          json:"id"`
	SlugID      string    `bson:"slug_id"      json:"slug_id"`
	Version     string    `bson:"version"      json:"version"`
	Manifest    Manifest  `bson:"manifest"     json:"manifest"`
	SourceID    string    `bson:"source_id"    json:"source_id"`
	InstalledAt time.Time `bson:"installed_at" json:"installed_at"`
	Checksum    string    `bson:"checksum"     json:"checksum"`
	Path        string    `bson:"path,omitempty" json:"path,omitempty"`
}

// SkillInstall represents a tenant electing to install a particular skill
// version. (P1 ships single-tenant; TenantID is always "default".)
type SkillInstall struct {
	ID          string    `bson:"_id"          json:"id"`
	TenantID    string    `bson:"tenant_id"    json:"tenant_id"`
	SkillID     string    `bson:"skill_id"     json:"skill_id"`
	SlugID      string    `bson:"slug_id"      json:"slug_id"`
	Version     string    `bson:"version"      json:"version"`
	Enabled     bool      `bson:"enabled"      json:"enabled"`
	Private     bool      `bson:"private"      json:"private"`
	InstalledAt time.Time `bson:"installed_at" json:"installed_at"`
}

// AgentSkillBinding ties an agent node to a declarative list of skills and
// caches the resolved lockfile so re-runs use stable versions.
type AgentSkillBinding struct {
	ID       string      `bson:"_id"      json:"id"`
	AgentID  string      `bson:"agent_id" json:"agent_id"`
	Skills   []SkillReq  `bson:"skills"   json:"skills"`
	Lockfile []SkillLock `bson:"lockfile" json:"lockfile"`
}

// SkillReq is a declarative reference: "agent X wants skill Y in semver
// range Z". Resolved → SkillLock.
type SkillReq struct {
	SlugID string `bson:"slug_id" json:"slug_id"`
	Range  string `bson:"range"   json:"range"`
}

// SkillLock is a resolved skill version pinned for an agent run.
type SkillLock struct {
	SlugID   string `bson:"slug_id"  json:"slug_id"`
	Version  string `bson:"version"  json:"version"`
	Checksum string `bson:"checksum" json:"checksum"`
}

package skills

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// ManifestFileName is the canonical filename inside a skill bundle.
const ManifestFileName = "manifest.yaml"

// yamlManifest mirrors Manifest but uses `any` for the JSON-Schema fields so
// YAML's native map shape decodes cleanly. ParseManifest re-marshals each
// schema as JSON before populating the public Manifest type. Two-phase
// decode keeps the public surface aligned with the LLM ToolDef contract
// (which uses json.RawMessage) while still letting authors write idiomatic
// YAML.
type yamlManifest struct {
	ID                 string         `yaml:"id"`
	Version            string         `yaml:"version"`
	Name               string         `yaml:"name"`
	Description        string         `yaml:"description"`
	Author             Author         `yaml:"author"`
	License            string         `yaml:"license"`
	APIVersion         int            `yaml:"api_version"`
	MinPlatformVersion string         `yaml:"min_platform_version"`
	Dependencies       []Dependency   `yaml:"dependencies"`
	Tools              []yamlTool     `yaml:"tools"`
	Prompt             *Prompt        `yaml:"prompt"`
	Memory             *Memory        `yaml:"memory"`
	Capabilities       Capabilities   `yaml:"capabilities"`
	Config             []ConfigDef    `yaml:"config"`
	Forward            ForwardCompat  `yaml:",inline"`
}

type yamlTool struct {
	ID           string `yaml:"id"`
	File         string `yaml:"file"`
	Language     string `yaml:"language"`
	Description  string `yaml:"description"`
	InputSchema  any    `yaml:"input_schema"`
	OutputSchema any    `yaml:"output_schema"`
	TimeoutSecs  int    `yaml:"timeout_secs"`
}

// ParseManifest decodes a YAML manifest from raw bytes and runs the strict
// validation pass (see Validate). Use this everywhere a manifest crosses a
// trust boundary (file load, registry insert, API ingestion).
func ParseManifest(data []byte) (Manifest, error) {
	var ym yamlManifest

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // reject unknown top-level keys
	if err := dec.Decode(&ym); err != nil {
		if errors.Is(err, io.EOF) {
			return Manifest{}, errors.New("skills: manifest is empty")
		}
		return Manifest{}, fmt.Errorf("skills: parse manifest: %w", err)
	}

	m := Manifest{
		ID:                 ym.ID,
		Version:            ym.Version,
		Name:               ym.Name,
		Description:        ym.Description,
		Author:             ym.Author,
		License:            ym.License,
		APIVersion:         ym.APIVersion,
		MinPlatformVersion: ym.MinPlatformVersion,
		Dependencies:       ym.Dependencies,
		Prompt:             ym.Prompt,
		Memory:             ym.Memory,
		Capabilities:       ym.Capabilities,
		Config:             ym.Config,
		Forward:            ym.Forward,
	}

	for _, yt := range ym.Tools {
		t := Tool{
			ID:          yt.ID,
			File:        yt.File,
			Language:    yt.Language,
			Description: yt.Description,
			TimeoutSecs: yt.TimeoutSecs,
		}
		if yt.InputSchema != nil {
			b, err := yamlValueToJSON(yt.InputSchema)
			if err != nil {
				return Manifest{}, fmt.Errorf("skills: tool %q input_schema: %w", yt.ID, err)
			}
			t.InputSchema = b
		}
		if yt.OutputSchema != nil {
			b, err := yamlValueToJSON(yt.OutputSchema)
			if err != nil {
				return Manifest{}, fmt.Errorf("skills: tool %q output_schema: %w", yt.ID, err)
			}
			t.OutputSchema = b
		}
		m.Tools = append(m.Tools, t)
	}

	if err := Validate(&m); err != nil {
		return Manifest{}, err
	}

	// Default any-object schema for tools that omitted `input_schema`,
	// matching agent_tools.go::asToolDef. After Validate so a malformed
	// schema still surfaces a real error.
	for i, tool := range m.Tools {
		if len(tool.InputSchema) == 0 {
			m.Tools[i].InputSchema = json.RawMessage(`{"type":"object"}`)
		}
	}

	return m, nil
}

// LoadManifestFile reads a manifest from disk and parses it.
func LoadManifestFile(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("skills: read manifest %q: %w", path, err)
	}
	return ParseManifest(data)
}

// yamlValueToJSON converts a YAML-decoded `any` (map[string]any /
// []any / scalars) to canonical JSON bytes. YAML map keys are
// `interface{}` in some libraries; yaml.v3 already gives us `string`
// keys for object maps, so a vanilla json.Marshal is sufficient.
func yamlValueToJSON(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/llm"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/sandbox"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/skills"
)

// appendSkillTools loads the skills declared on `agent.Data["skills"]`,
// resolves them through SkillRes, and registers each tool into `cat`.
//
// Returns the set of `prompts/system.md` fragments collected across all
// loaded skills so runAIAgent can append them to the agent's system prompt.
//
// No-op when SkillRes is nil OR data.skills is unset/empty. A failure to
// resolve any individual skill aborts the whole catalog build; we'd rather
// the agent run fail loudly than load a partial tool set.
func (e *WorkflowExecutor) appendSkillTools(agent Node, cat *ToolCatalog, params map[string]string, wfCtx runCtx, input any) ([]string, error) {
	if e.SkillRes == nil {
		return nil, nil
	}
	reqs, ok := readSkillRequests(agent.Data["skills"])
	if !ok || len(reqs) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Read the agent's secret bindings: { secretName: connectionID }. Used
	// to satisfy each skill's `capabilities.secrets[]` declaration. Empty
	// when no skill needs secrets.
	secretBindings := readSecretBindings(agent.Data["skill_secrets"])

	// Read the agent's skill_config map: { slug: { name: value } }. Used to
	// satisfy each skill's `config[]` block. Per-skill, per-name lookup.
	configBindings := readSkillConfigBindings(agent.Data["skill_config"])

	// Single-tenant for now (matches PROGRESS.md). Threading tenant_id
	// through here is the P2 multi-tenancy story.
	locks, err := e.SkillRes.Resolve(ctx, "default", reqs)
	if err != nil {
		return nil, fmt.Errorf("ai_agent: resolve skills: %w", err)
	}

	var fragments []string
	for _, lock := range locks {
		bundle, err := e.SkillRes.LoadBundle(ctx, lock)
		if err != nil {
			return nil, fmt.Errorf("ai_agent: load skill %s@%s: %w", lock.SlugID, lock.Version, err)
		}
		manifest := bundle.Manifest()

		// Append the skill's system-prompt fragment if present.
		if manifest.Prompt != nil && manifest.Prompt.Fragment != "" {
			text, err := bundle.ReadString(manifest.Prompt.Fragment)
			if err != nil {
				_ = bundle.Close()
				return nil, fmt.Errorf("ai_agent: read prompt fragment for %s@%s: %w", lock.SlugID, lock.Version, err)
			}
			text = strings.TrimSpace(text)
			if text != "" {
				fragments = append(fragments, text)
			}
		}

		// Resolve declared secrets to concrete values. Missing bindings
		// fail the load — running a skill without its required secret
		// produces opaque sandbox errors, better to surface here.
		secretValues, err := e.resolveSkillSecrets(ctx, manifest, secretBindings)
		if err != nil {
			_ = bundle.Close()
			return nil, fmt.Errorf("ai_agent: resolve secrets for %s@%s: %w", lock.SlugID, lock.Version, err)
		}

		// Resolve author-bound config. Required entries hard-fail when the
		// agent didn't bind a value; defaults apply when set on the
		// manifest. Returns sandbox-ready params with the namespaced key
		// `<sanitized_slug>__<name>`. String bindings get `{{params.X}}`
		// and `{{input.X}}` substitution so an author can plug workflow
		// params directly into config without copying values.
		configValues, err := resolveSkillConfig(manifest, configBindings, params, wfCtx, input)
		if err != nil {
			_ = bundle.Close()
			return nil, fmt.Errorf("ai_agent: resolve config for %s@%s: %w", lock.SlugID, lock.Version, err)
		}

		// Pre-load every tool's source up-front. The bundle handle is
		// closed before the agent loop runs, so the closure can't reopen
		// it lazily.
		toolSources := make(map[string]string, len(manifest.Tools))
		for _, tool := range manifest.Tools {
			code, err := bundle.ReadString(tool.File)
			if err != nil {
				_ = bundle.Close()
				return nil, fmt.Errorf("ai_agent: read tool %s/%s: %w", manifest.ID, tool.ID, err)
			}
			toolSources[tool.ID] = code
		}
		_ = bundle.Close()

		// Merge secrets + config into a single sandbox params map. Both
		// are author-bound and carry the same trust scope; namespacing on
		// the config side prevents collisions between skills.
		params := mergeSkillParams(secretValues, configValues)

		for _, tool := range manifest.Tools {
			if err := registerSkillTool(cat, manifest, tool, toolSources[tool.ID], params, e.SandboxRT); err != nil {
				slog.Warn("ai_agent: skill tool registration failed",
					"skill", manifest.ID, "tool", tool.ID, "err", err)
				continue
			}
		}

		slog.Info("ai_agent: skill loaded",
			"skill", manifest.ID, "version", manifest.Version,
			"tool_count", len(manifest.Tools), "has_prompt", manifest.Prompt != nil,
			"secret_count", len(secretValues), "config_count", len(configValues),
		)
	}

	return fragments, nil
}

// mergeSkillParams returns a fresh map[string]string combining the secret
// and config maps. Inputs are not mutated. Config values use namespaced
// keys (set by resolveSkillConfig); secrets use their raw declared names.
func mergeSkillParams(secrets, config map[string]string) map[string]string {
	out := make(map[string]string, len(secrets)+len(config))
	for k, v := range secrets {
		out[k] = v
	}
	for k, v := range config {
		out[k] = v
	}
	return out
}

// readSecretBindings parses the agent node's `data.skill_secrets` value
// into a map[secretName]connectionID. Accepts JSON map and BSON-decoded
// shapes. Empty when unset.
func readSecretBindings(v any) map[string]string {
	m, ok := mapAny(v)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		if s, ok := val.(string); ok && s != "" {
			out[k] = s
		}
	}
	return out
}

// readSkillConfigBindings parses the agent node's `data.skill_config` value
// into a nested map: { slug: { configName: value } }. Per-skill scope so
// the same config name can mean different things in different skills.
// Values stay as `any` here; resolveSkillConfig handles type coercion.
func readSkillConfigBindings(v any) map[string]map[string]any {
	outer, ok := mapAny(v)
	if !ok {
		return nil
	}
	out := make(map[string]map[string]any, len(outer))
	for slug, raw := range outer {
		inner, ok := mapAny(raw)
		if !ok {
			continue
		}
		out[slug] = inner
	}
	return out
}

// resolveSkillConfig walks the skill's declared `config[]` and produces a
// sandbox-ready params map with the namespaced key
// `<sanitized_slug>__<name>`.
//
// Resolution order: agent binding → manifest default → ErrSkillNotFound-
// equivalent error when required and neither was set.
func resolveSkillConfig(manifest skills.Manifest, allBindings map[string]map[string]any, params map[string]string, wfCtx runCtx, input any) (map[string]string, error) {
	if len(manifest.Config) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(manifest.Config))
	bindings := allBindings[manifest.ID] // nil-safe (zero-value lookup)

	// Normalise hyphens + slashes to underscores so the sandbox `params`
	// key is a valid Python identifier and easy to grep for in skill source
	// code (`params["weather_formatter__default_style"]`). Tool names keep
	// hyphens — those are LLM-facing where the original slug shape is fine.
	normID := strings.ReplaceAll(strings.ReplaceAll(manifest.ID, "/", "_"), "-", "_")
	prefix := sanitizeToolName(normID)

	for _, cfg := range manifest.Config {
		var raw any
		var have bool
		if bindings != nil {
			raw, have = bindings[cfg.Name]
		}
		if !have && cfg.Default != nil {
			raw = cfg.Default
			have = true
		}
		if !have {
			if cfg.Required {
				return nil, fmt.Errorf("skill %s requires config %q but agent did not bind a value", manifest.ID, cfg.Name)
			}
			continue
		}
		// String values get param/template substitution so authors can
		// reference workflow params (e.g. "{{params.weather_style}}").
		// Non-string types pass through.
		if s, ok := raw.(string); ok {
			s = substituteParams(s, params)
			s = applyTemplate(s, input, wfCtx)
			raw = s
		}
		stringified, err := stringifyConfigValue(cfg, raw)
		if err != nil {
			return nil, fmt.Errorf("skill %s config %q: %w", manifest.ID, cfg.Name, err)
		}
		key := prefix + "__" + cfg.Name
		out[key] = stringified
		slog.Info("ai_agent: resolved skill config",
			"skill", manifest.ID, "name", cfg.Name,
			"raw_value", raw, "stringified", stringified, "key", key,
			"params_count", len(params))
	}
	return out, nil
}

// substituteParams replaces every `{{params.X}}` placeholder in s with the
// matching workflow param value. Mirrors `applyParamsToData` but operates
// on a single string so we can compose with applyTemplate.
func substituteParams(s string, params map[string]string) string {
	for k, v := range params {
		s = strings.ReplaceAll(s, "{{params."+k+"}}", v)
	}
	return s
}

// stringifyConfigValue coerces a config value into a string per the
// declared type. Numbers and booleans get a deterministic stringification
// matching the JSON serialisation an author would expect; strings pass
// through with enum validation.
func stringifyConfigValue(cfg skills.ConfigDef, v any) (string, error) {
	switch cfg.Type {
	case "string":
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("expected string, got %T", v)
		}
		if len(cfg.Enum) > 0 {
			allowed := false
			for _, opt := range cfg.Enum {
				if s == opt {
					allowed = true
					break
				}
			}
			if !allowed {
				return "", fmt.Errorf("value %q not in enum %v", s, cfg.Enum)
			}
		}
		return s, nil
	case "number":
		switch n := v.(type) {
		case float64:
			return strconv.FormatFloat(n, 'f', -1, 64), nil
		case int:
			return strconv.Itoa(n), nil
		case int64:
			return strconv.FormatInt(n, 10), nil
		case json.Number:
			return n.String(), nil
		}
		return "", fmt.Errorf("expected number, got %T", v)
	case "boolean":
		b, ok := v.(bool)
		if !ok {
			return "", fmt.Errorf("expected bool, got %T", v)
		}
		if b {
			return "true", nil
		}
		return "false", nil
	}
	return "", fmt.Errorf("unsupported config type %q", cfg.Type)
}

// resolveSkillSecrets walks the skill's declared secrets and pairs each
// with a value loaded from the bound Connection's config. Lookup order
// for the value: Config[secretName] first, then Config["api_key"] as a
// generic fallback (covers the most common Connection shape).
//
// Required secrets that have no binding produce a hard error so the LLM
// never sees a tool whose call would silently fail with a missing key.
func (e *WorkflowExecutor) resolveSkillSecrets(ctx context.Context, manifest skills.Manifest, bindings map[string]string) (map[string]string, error) {
	if len(manifest.Capabilities.Secrets) == 0 {
		return nil, nil
	}
	if e.ConnResolver == nil {
		return nil, fmt.Errorf("skill %s declares secrets but ConnResolver is not configured", manifest.ID)
	}
	out := make(map[string]string, len(manifest.Capabilities.Secrets))
	for _, sec := range manifest.Capabilities.Secrets {
		connID, ok := bindings[sec.Name]
		if !ok || connID == "" {
			return nil, fmt.Errorf("skill %s requires secret %q but no Connection is bound", manifest.ID, sec.Name)
		}
		conn, err := e.ConnResolver.ResolveConnection(ctx, connID)
		if err != nil {
			return nil, fmt.Errorf("skill %s secret %q: %w", manifest.ID, sec.Name, err)
		}
		val := conn.Config[sec.Name]
		if val == "" {
			val = conn.Config["api_key"]
		}
		if val == "" {
			return nil, fmt.Errorf("skill %s secret %q: connection %q has no value for %q or fallback api_key", manifest.ID, sec.Name, connID, sec.Name)
		}
		out[sec.Name] = val
	}
	return out, nil
}

// readSkillRequests parses the agent node's `data.skills` value into a
// []skills.SkillReq. Accepts both the JSON-shape (map[string]any with
// `slug_id` / `range` keys) and BSON-decoded shapes via mapAny — same
// pitfall as the as_tool block, same fix.
func readSkillRequests(v any) ([]skills.SkillReq, bool) {
	if v == nil {
		return nil, false
	}
	raw, ok := sliceAny(v)
	if !ok {
		return nil, false
	}
	out := make([]skills.SkillReq, 0, len(raw))
	for _, entry := range raw {
		m, ok := mapAny(entry)
		if !ok {
			continue
		}
		req := skills.SkillReq{}
		if s, ok := m["slug_id"].(string); ok {
			req.SlugID = s
		} else if s, ok := m["slug"].(string); ok {
			req.SlugID = s
		}
		if r, ok := m["range"].(string); ok {
			req.Range = r
		} else if r, ok := m["version"].(string); ok {
			req.Range = r
		}
		if req.SlugID == "" {
			continue
		}
		out = append(out, req)
	}
	return out, true
}

// registerSkillTool wraps a skill manifest tool into a ToolDef + ToolHandler
// and adds it to the catalog. Tool name is `<sanitized-slug>__<tool_id>`
// to keep the LLM-facing namespace flat while avoiding cross-skill
// collisions. Handler runs the bundled source in the sandbox runtime,
// passing the LLM-supplied args as the script's `input` global. Secrets
// from the skill's manifest are injected as sandbox `params[secretName]`
// entries; the script reads them by name (`params["weather_api_key"]`).
func registerSkillTool(cat *ToolCatalog, manifest skills.Manifest, tool skills.Tool, source string, secrets map[string]string, rt sandbox.Runtime) error {
	if rt == nil {
		return fmt.Errorf("sandbox runtime not configured (skill tools require it)")
	}

	prefix := sanitizeToolName(strings.ReplaceAll(manifest.ID, "/", "_"))
	name := sanitizeToolName(prefix + "__" + tool.ID)

	def := llm.ToolDef{
		Name:        name,
		Description: tool.Description,
		InputSchema: tool.InputSchema,
	}

	timeout := 30 * time.Second
	if tool.TimeoutSecs > 0 {
		timeout = time.Duration(tool.TimeoutSecs) * time.Second
	}
	network := manifest.Capabilities.Network.Allow()
	language := sandbox.Language(tool.Language)

	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		// Authoritative server-side proof the skill ran. Logged BEFORE the
		// sandbox call so a hung pod still leaves a "started" breadcrumb.
		slog.Info("ai_agent: skill tool invoked",
			"skill", manifest.ID,
			"version", manifest.Version,
			"tool", tool.ID,
			"args_len", len(args),
		)

		var argInput any
		if len(args) > 0 {
			if err := json.Unmarshal(args, &argInput); err != nil {
				return "", fmt.Errorf("skill %s tool %s: bad args: %w", manifest.ID, tool.ID, err)
			}
		}

		// Copy secrets into a fresh map per invocation so concurrent tool
		// calls can't see each other's params. The map is small (~handful
		// of secrets at most), so the copy cost is negligible.
		var paramCopy map[string]string
		if len(secrets) > 0 {
			paramCopy = make(map[string]string, len(secrets))
			for k, v := range secrets {
				paramCopy[k] = v
			}
		}

		req := sandbox.RunRequest{
			Language: language,
			Code:     source,
			Input:    argInput,
			Params:   paramCopy,
			Network:  network,
			Timeout:  timeout,
		}
		runCtx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
		defer cancel()

		start := time.Now()
		result, err := rt.Run(runCtx, req)
		if err != nil {
			return "", fmt.Errorf("skill %s tool %s: %w", manifest.ID, tool.ID, err)
		}
		slog.Info("ai_agent: skill tool finished",
			"skill", manifest.ID,
			"tool", tool.ID,
			"exit_code", result.ExitCode,
			"duration_ms", time.Since(start).Milliseconds(),
		)

		var sb strings.Builder
		if result.Output != nil {
			b, _ := json.Marshal(result.Output)
			sb.WriteString("output: ")
			sb.Write(b)
			sb.WriteString("\n")
		} else if strings.TrimSpace(result.Stdout) != "" {
			sb.WriteString("stdout: ")
			sb.WriteString(result.Stdout)
			sb.WriteString("\n")
		}
		if strings.TrimSpace(result.Stderr) != "" {
			sb.WriteString("stderr: ")
			sb.WriteString(result.Stderr)
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "exit_code: %d\nduration: %s", result.ExitCode, result.Duration)
		if result.ExitCode != 0 {
			return sb.String(), fmt.Errorf("skill tool exit %d", result.ExitCode)
		}
		return sb.String(), nil
	}

	return cat.Add(def, handler)
}

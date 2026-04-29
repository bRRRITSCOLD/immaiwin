// Workflow templates — bundled at compile time so the api binary
// ships with a starter library. The /api/v1/workflow_templates
// endpoint reads from this embedded FS; the UI's "+ New from
// template" picker forks one into a fresh workflow doc.
//
// Add a template by dropping a JSON into `workflows/` with shape:
//
//	{
//	  "name":        "Display name",
//	  "description": "One-paragraph what + why.",
//	  "category":    "AI" | "Webhooks" | "Schedules" | …,
//	  "workflow":    { …Workflow object — same shape the API persists }
//	}
//
// `workflow.id` + timestamps are stripped on fork so the cloned doc
// gets a fresh ULID + created/updated stamps.

package templates

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
)

//go:embed workflows/*.json
var workflowFS embed.FS

// Template is the wire shape for the templates list endpoint. The
// workflow body is included so the picker can preview node count /
// trigger type without a follow-up RPC.
type Template struct {
	Slug        string             `json:"slug"`
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Category    string             `json:"category,omitempty"`
	Workflow    workflow.Workflow  `json:"workflow"`
}

type templateFile struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Category    string             `json:"category"`
	Workflow    workflow.Workflow  `json:"workflow"`
}

// List parses every embedded JSON template + returns them sorted by
// category then name. Slug is derived from the filename (stripped of
// `.json`). Errors on individual files are returned in `errs` rather
// than aborting the whole list — better for diagnosing a broken
// template than silently dropping it.
func List() ([]Template, []error) {
	entries, err := fs.ReadDir(workflowFS, "workflows")
	if err != nil {
		return nil, []error{fmt.Errorf("templates: read embedded dir: %w", err)}
	}
	var out []Template
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, rerr := fs.ReadFile(workflowFS, "workflows/"+e.Name())
		if rerr != nil {
			errs = append(errs, fmt.Errorf("templates: read %s: %w", e.Name(), rerr))
			continue
		}
		var tf templateFile
		if jerr := json.Unmarshal(raw, &tf); jerr != nil {
			errs = append(errs, fmt.Errorf("templates: parse %s: %w", e.Name(), jerr))
			continue
		}
		slug := strings.TrimSuffix(e.Name(), ".json")
		out = append(out, Template{
			Slug:        slug,
			Name:        tf.Name,
			Description: tf.Description,
			Category:    tf.Category,
			Workflow:    tf.Workflow,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].Name < out[j].Name
	})
	return out, errs
}

// Get returns a single template by slug, or an error if none matches.
func Get(slug string) (Template, error) {
	all, _ := List()
	for _, t := range all {
		if t.Slug == slug {
			return t, nil
		}
	}
	return Template{}, fmt.Errorf("templates: no template with slug %q", slug)
}

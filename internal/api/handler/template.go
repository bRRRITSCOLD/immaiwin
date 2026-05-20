// Workflow template endpoints — list bundled templates so the UI's
// "+ New from template" picker has something to render, plus a fork
// endpoint that bypasses the connection validator (templates ship
// with empty `connection_id` / `llm_connection_id` placeholders by
// design — the user fills them in on the canvas BEFORE the next
// save, which still goes through the normal UpsertWorkflow guard).

package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/bRRRITSCOLD/burrow/internal/api/templates"
	"github.com/bRRRITSCOLD/burrow/internal/auth"
	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// ListWorkflowTemplates returns every embedded template. Errors on
// individual files surface in the `errors` array rather than 500ing
// the whole request — one bad template shouldn't black-hole the
// picker.
func ListWorkflowTemplates() gin.HandlerFunc {
	return func(c *gin.Context) {
		list, errs := templates.List()
		if list == nil {
			list = []templates.Template{}
		}
		var errStrs []string
		for _, e := range errs {
			errStrs = append(errStrs, e.Error())
		}
		c.JSON(http.StatusOK, gin.H{
			"templates": list,
			"errors":    errStrs,
		})
	}
}

// ForkWorkflowTemplateDeps wraps the dependencies needed by the
// template-fork handler so we can audit-log the privileged action
// without expanding the existing WorkflowStore signature.
type ForkWorkflowTemplateDeps struct {
	Store WorkflowStore
	Audit *mongodb.AuditRepository
}

// ForkWorkflowTemplate clones a bundled template into a fresh
// workflow doc in the caller's tenant. ID = fresh ULID (or caller-
// supplied via body.new_id), name carries a " (copy)" suffix to
// keep the picker UX consistent with `DuplicateWorkflow`.
//
// IMPORTANT: this endpoint deliberately bypasses the connection
// validator used by `UpsertWorkflow`. Templates ship with empty
// connection placeholders by design — the user wires the right
// connection on the canvas, and the NEXT save runs through the
// normal validator, which surfaces an actionable "missing
// connection on <node>" toast at the right moment.
func ForkWorkflowTemplate(deps ForkWorkflowTemplateDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		slug := c.Param("slug")
		if slug == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "slug required"})
			return
		}

		tenantID, hasTenant := auth.TenantFromCtx(c.Request.Context())
		if !hasTenant {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "auth required"})
			return
		}

		tmpl, err := templates.Get(slug)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "template not found"})
			return
		}

		var body struct {
			NewID string `json:"new_id"`
			Name  string `json:"name"`
		}
		_ = json.NewDecoder(c.Request.Body).Decode(&body)
		newID := body.NewID
		if newID == "" {
			newID = ulid.Make().String()
		}

		// Defensive: refuse new_id collisions outright instead of
		// silently merging into someone else's doc (same posture as
		// DuplicateWorkflow).
		if _, gerr := deps.Store.GetByID(c.Request.Context(), newID); gerr == nil {
			c.JSON(http.StatusConflict, gin.H{"error": "new_id already in use"})
			return
		} else if !errors.Is(gerr, mongo.ErrNoDocuments) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": gerr.Error()})
			return
		}

		dup := tmpl.Workflow
		dup.ID = newID
		if body.Name != "" {
			dup.Name = body.Name
		} else {
			dup.Name = tmpl.Workflow.Name + " (copy)"
		}
		dup.TenantID = tenantID
		dup.Version = 0
		// Default to enabled = true so the workflow boots into the
		// trigger workers' sync-tick once the user wires up the
		// connection — same posture as a brand-new workflow.
		dup.Enabled = true

		saved, err := deps.Store.Upsert(c.Request.Context(), dup)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		recordAudit(c, deps.Audit, mongodb.AuditWorkflowTemplateForked,
			map[string]any{
				"template_slug": slug,
				"workflow_id":   saved.ID,
			},
			map[string]any{"name": saved.Name},
		)

		c.JSON(http.StatusCreated, saved)
	}
}

// Workflow template endpoints — list bundled templates so the UI's
// "+ New from template" picker has something to render. The fork
// flow itself is just `POST /api/v1/workflows/:id` with the
// template's workflow body + a fresh ULID, so no dedicated fork
// endpoint is needed.

package handler

import (
	"net/http"

	"github.com/bRRRITSCOLD/burrow/internal/api/templates"
	"github.com/gin-gonic/gin"
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

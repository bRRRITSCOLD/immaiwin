// Webhook trigger endpoint — kicks off a workflow run when an external
// service POSTs to /api/v1/webhooks/:slug. The slug is configured per
// trigger node (`trigger_type=webhook`, `webhook_slug=<value>`); the
// handler scans workflow docs for a matching slug, decodes the request
// body as the trigger's input, and runs the workflow synchronously
// (returning the run ID and final status). Optional HMAC SHA-256
// auth: when the trigger node has `webhook_secret` set, the request
// must include `X-Webhook-Signature: sha256=<hex>` over the raw body.
//
// Distinct from the workflow run WS endpoint — that one streams events
// to the canvas. Webhooks have no live UI; we run server-side just
// like a cron-driven invocation. Approval gates (`require_node_approval`
// / agent's `require_approval`) flow through the OOB Redis channel.

package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
)

// HandleWebhook handles POST /api/v1/webhooks/:slug. Looks up the
// matching workflow, parses the body (JSON or raw), kicks off
// RunResumable. Returns the run record so callers can poll
// /workflow_runs/:id for status if they need it.
func HandleWebhook(store WorkflowStore, exec *workflow.WorkflowExecutor) gin.HandlerFunc {
	return func(c *gin.Context) {
		slug := c.Param("slug")
		if slug == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "slug required"})
			return
		}
		if exec == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "executor not configured"})
			return
		}

		// Linear scan for a workflow whose trigger node matches the
		// slug. Workflow count is small (10s in dev, 100s in prod);
		// switch to an indexed Mongo query if this becomes hot.
		wfs, err := store.List(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		var matched *workflow.Workflow
		var triggerData map[string]any
		for i := range wfs {
			wf := &wfs[i]
			for _, n := range wf.Nodes {
				if n.Type != workflow.NodeTypeTrigger {
					continue
				}
				tt, _ := n.Data["trigger_type"].(string)
				if tt != "webhook" {
					continue
				}
				ws, _ := n.Data["webhook_slug"].(string)
				if ws == slug {
					matched = wf
					triggerData = n.Data
					break
				}
			}
			if matched != nil {
				break
			}
		}
		if matched == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "no workflow with that webhook slug"})
			return
		}

		// Read body (cap to 10MB to mirror the http_request node's
		// default response cap — keeps us from buffering a hostile
		// payload). io.ReadAll respects the request context.
		const maxBody = 10 * 1024 * 1024
		raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBody+1))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "read body: " + err.Error()})
			return
		}
		if len(raw) > maxBody {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "body exceeds 10MB"})
			return
		}

		// HMAC verification when secret is configured. Header format:
		// `X-Webhook-Signature: sha256=<hex>`. Constant-time compare
		// to avoid leaking the secret over timing.
		if secret, _ := triggerData["webhook_secret"].(string); secret != "" {
			sig := strings.TrimPrefix(c.GetHeader("X-Webhook-Signature"), "sha256=")
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(raw)
			expected := hex.EncodeToString(mac.Sum(nil))
			if !hmac.Equal([]byte(sig), []byte(expected)) {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
				return
			}
		}

		// Decode body as JSON when content-type matches; otherwise
		// pass the raw string through. Mirrors the http_request
		// node's `parse_json` behaviour so trigger-side handling
		// stays predictable.
		var input any
		ct := strings.ToLower(c.GetHeader("Content-Type"))
		if strings.Contains(ct, "application/json") && len(raw) > 0 {
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			if derr := dec.Decode(&input); derr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "decode JSON body: " + derr.Error()})
				return
			}
		} else if len(raw) > 0 {
			input = string(raw)
		}

		// Sync vs async dispatch. Default = async (fire-and-forget,
		// 202 Accepted, run_id only) — matches the GitHub/Stripe/Slack
		// webhook contract where senders have their own short timeouts
		// + retry on non-2xx, and approval gates / long workflows
		// would otherwise deadlock the request. Opt into sync via
		// `?wait=true` for use cases like Slack slash commands or
		// "AI agent reply in same response" patterns.
		wait := strings.EqualFold(c.Query("wait"), "true") || c.Query("wait") == "1"
		if !wait {
			// Pre-allocate run ID so we can return it in the 202
			// response. RunResumable seeds the WorkflowRun record
			// with this ID via opts.PreallocRunID. Detach from
			// request ctx so the run survives after we reply.
			runID := ulid.Make().String()
			runCtx := context.Background()
			wfCopy := *matched
			go func() {
				_, _ = exec.RunResumable(runCtx, wfCopy, workflow.RunOpts{
					Input:         input,
					PreallocRunID: runID,
				})
				// Run record already captures status/error via
				// RunResumable's persist — async clients poll
				// /workflow_runs/:id.
			}()
			c.JSON(http.StatusAccepted, gin.H{
				"run_id": runID,
				"status": "accepted",
				"detail": "Run dispatched async. Poll /api/v1/workflow_runs/:id for status. Use ?wait=true on this endpoint to block until completion instead.",
			})
			return
		}

		outcome, err := exec.RunResumable(c.Request.Context(), *matched, workflow.RunOpts{
			Input: input,
		})
		if err != nil {
			// RunResumable already persists the failed run; return
			// the run_id + the error so the caller can fetch full
			// detail via /workflow_runs/:id.
			c.JSON(http.StatusOK, gin.H{
				"run_id": outcome.RunID,
				"status": string(outcome.Status),
				"error":  err.Error(),
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"run_id": outcome.RunID,
			"status": string(outcome.Status),
		})
	}
}

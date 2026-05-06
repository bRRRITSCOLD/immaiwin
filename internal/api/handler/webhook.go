// Webhook trigger endpoint — kicks off a workflow run when an external
// service POSTs to /api/v1/webhooks/:slug. The slug is configured per
// trigger node (`trigger_type=webhook`, `webhook_slug=<value>`); the
// handler scans workflow docs for a matching slug, decodes the request
// body as the trigger's input, and dispatches the run via the lease
// worker (PR 3.4b — durable execution unification).
//
// Sync vs async response shape is selected by `?wait=`:
//
//	wait=false  (default)  → 202 Accepted + {run_id, status:"queued"}.
//	                         Async clients poll /api/v1/workflow_runs/:id.
//	                         Matches the GitHub / Stripe / Slack webhook
//	                         contract where senders have short timeouts +
//	                         retry on non-2xx, and a long workflow or
//	                         approval gate would otherwise deadlock the
//	                         request.
//	wait=true              → block until the run hits a terminal status
//	                         (success / error / cancelled), then return
//	                         200 + {run_id, status, error?}. Use for
//	                         Slack slash commands or "AI agent reply in
//	                         same response" patterns. Caller's HTTP
//	                         timeout governs how long we wait.
//
// Optional HMAC SHA-256 auth: when the trigger node has
// `webhook_secret` set, the request must include
// `X-Webhook-Signature: sha256=<hex>` over the raw body.
//
// Distinct from the workflow run WS endpoint — that one streams events
// to the canvas. Webhooks have no live UI; the lease worker handles
// execution + the agent's OOB approval gates resolve via
// `POST /api/v1/workflow_runs/:id/approval` exactly like any other
// trigger source.

package handler

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/workflow"
	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
)

const (
	// webhookWaitPollInterval is the poll cadence when the caller
	// asked for synchronous behaviour. 200ms keeps round-trip overhead
	// low for fast workflows (most webhooks finish in <1s) without
	// pounding Mongo for slow ones — a 30s run hits the DB ~150 times.
	webhookWaitPollInterval = 200 * time.Millisecond
)

// HandleWebhook handles POST /api/v1/webhooks/:slug. Looks up the
// matching workflow, parses the body, dispatches via the lease worker
// (queued WorkflowRun record + Redis wakeup), and either returns the
// run_id immediately (async) or polls until terminal (sync).
func HandleWebhook(store WorkflowStore, runStore workflow.WorkflowRunStore, rc workflow.WakeupPublisher) gin.HandlerFunc {
	return func(c *gin.Context) {
		slug := c.Param("slug")
		if slug == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "slug required"})
			return
		}
		if runStore == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "run store not configured"})
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
		// pass the raw string through. Mirrors the http_request node's
		// `parse_json` behaviour so trigger-side handling stays
		// predictable.
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

		// Persist the run rec + publish wakeup. Same contract every
		// other trigger source uses post-PR-3.4: the lease worker
		// claims, ClaimLease atomically promotes queued → running,
		// BFS executes, terminal cleanup writes status.
		runID := ulid.Make().String()
		now := time.Now().UTC()
		rec := workflow.WorkflowRun{
			ID:           runID,
			WorkflowID:   matched.ID,
			TenantID:     matched.TenantID,
			QueuedAt:     now,
			Status:       workflow.RunStatusQueued,
			Params:       matched.Params,
			TriggerInput: input,
		}
		if _, cerr := runStore.Create(c.Request.Context(), rec); cerr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "create run: " + cerr.Error()})
			return
		}
		// Best-effort wakeup; nil rc = no Redis wired = lease worker
		// picks the run up on its next claim tick instead.
		workflow.PublishWakeup(c.Request.Context(), rc)

		wait := strings.EqualFold(c.Query("wait"), "true") || c.Query("wait") == "1"
		if !wait {
			c.JSON(http.StatusAccepted, gin.H{
				"run_id": runID,
				"status": string(workflow.RunStatusQueued),
				"detail": "Run dispatched async. Poll /api/v1/workflow_runs/:id for status. Use ?wait=true on this endpoint to block until completion instead.",
			})
			return
		}

		// Sync path — poll until terminal or the request context
		// dies. The caller's HTTP timeout governs total wait; we
		// don't impose a server-side deadline beyond what gin already
		// enforces. Read-after-write on Mongo is consistent enough
		// for this poll cadence on a single-node deployment.
		ticker := time.NewTicker(webhookWaitPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-c.Request.Context().Done():
				// Caller hung up before terminal; the run keeps
				// running on the worker. We still return what we
				// know so the client can poll later by run_id.
				c.JSON(http.StatusAccepted, gin.H{
					"run_id": runID,
					"status": "accepted",
					"detail": "Caller context cancelled before run reached terminal; run continues on the worker. Poll /api/v1/workflow_runs/:id for final status.",
				})
				return
			case <-ticker.C:
				cur, gerr := runStore.Get(c.Request.Context(), runID)
				if gerr != nil {
					// Run was deleted out from under us, or the
					// reaper flipped it; surface the error and let
					// the caller decide.
					c.JSON(http.StatusInternalServerError, gin.H{
						"run_id": runID,
						"error":  "fetch run: " + gerr.Error(),
					})
					return
				}
				switch cur.Status {
				case workflow.RunStatusSuccess,
					workflow.RunStatusError,
					workflow.RunStatusCancelled:
					body := gin.H{
						"run_id": runID,
						"status": string(cur.Status),
					}
					if cur.Error != "" {
						body["error"] = cur.Error
					}
					c.JSON(http.StatusOK, body)
					return
				}
				// Anything else (queued / running / pending_approval
				// / paused) — keep waiting.
			}
		}
	}
}

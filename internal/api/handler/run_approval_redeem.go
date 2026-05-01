// Magic-link approval redeem endpoint — completes a `pending_approval`
// gate using a short-lived signed token instead of a session cookie.
// Used by the email / Slack approval-link path (Stage 2 of OOB
// approvals): the dispatcher posts a link of the form
//   <ui-base>/approve?run_id=<rid>&token=<jwt>
// to the configured channel; the recipient clicks, picks
// approve/reject, and the UI page POSTs to this endpoint.
//
//	POST /api/v1/workflow_runs/:id/approval/redeem
//	  body: {"token": "<jwt>", "decision": "approve"|"reject", "reason": "..."}
//
// Security: token IS the auth. No cookie or Bearer required. Token
// validations:
//   - HMAC signature must verify against the auth-config JWT secret.
//   - `iss` must equal ApprovalTokenIssuer ("burrow-approval"); a leaked
//     user-session JWT can't replay here.
//   - `exp` enforced by the JWT lib (default 24h TTL on issue).
//   - `sub` (run_id) must equal the path :id parameter.
//   - `jti` (token_id) must equal the persisted PendingApproval.TokenID;
//     once the gate resolves the field is cleared, so a replay against
//     the same run lands on a 410 Gone.
//
// Audit: recorded via recordAuditUnauth — no logged-in user fires this
// endpoint. The metadata captures decision + reason + redacted token
// jti so the privileged action stays traceable.

package handler

import (
	"net/http"

	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/bRRRITSCOLD/burrow/internal/workflow"
	"github.com/gin-gonic/gin"
)

// RunApprovalRedeemDeps wraps the dependencies the magic-link handler
// needs. JWTSecret is the same byte slice the user-session middleware
// uses (`AuthConfig.JWTSecret`); reusing it keeps deployments from
// having to manage a second key.
type RunApprovalRedeemDeps struct {
	Exec      *workflow.WorkflowExecutor
	RunStore  workflow.WorkflowRunStore
	Audit     *mongodb.AuditRepository
	JWTSecret []byte
}

// SubmitRunApprovalByToken redeems a magic-link approval token. Returns
// 200 on success, 401 on bad/expired/wrong-issuer token, 404 when the
// run isn't pending (replay or stale link), 410 Gone when the persisted
// TokenID doesn't match (specifically: gate already resolved).
func SubmitRunApprovalByToken(deps RunApprovalRedeemDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		runID := c.Param("id")
		if runID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}

		var req struct {
			Token    string `json:"token"`
			Decision string `json:"decision"`
			Reason   string `json:"reason,omitempty"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.Token == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "token required"})
			return
		}
		if req.Decision != "approve" && req.Decision != "reject" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "decision must be 'approve' or 'reject'"})
			return
		}

		if deps.Exec == nil || deps.Exec.ApprovalBroker == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "approval broker not configured"})
			return
		}
		if deps.RunStore == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "run store not configured"})
			return
		}
		if len(deps.JWTSecret) == 0 {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "approval token verifier not configured"})
			return
		}

		claims, err := workflow.VerifyApprovalToken(deps.JWTSecret, req.Token)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			return
		}
		if claims.Subject != runID {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "token run_id mismatch"})
			return
		}

		rec, err := deps.RunStore.Get(c.Request.Context(), runID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
			return
		}
		if rec.Status != workflow.RunStatusPendingApproval || rec.PendingApproval == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "no pending approval for this run"})
			return
		}
		// TokenID mismatch ≠ "no approval pending" — the gate may have
		// resolved + a SECOND gate fired with a fresh nonce. Caller
		// holds a stale link; 410 Gone differentiates from 404 so
		// frontend can surface a clearer error.
		if rec.PendingApproval.TokenID == "" || rec.PendingApproval.TokenID != claims.ID {
			c.JSON(http.StatusGone, gin.H{"error": "approval token superseded — gate already resolved or replaced"})
			return
		}

		decision := workflow.ApprovalDecision{
			ToolCallID: rec.PendingApproval.ToolCallID,
			Approved:   req.Decision == "approve",
			Reason:     req.Reason,
		}
		if applied, derr := applyDecisionAcrossPaths(c, deps.Exec, deps.RunStore, rec, decision); derr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": derr.Error()})
			return
		} else if !applied {
			cancelOrphanedApproval(c, deps.RunStore, rec)
			return
		}

		// Best-effort audit. Token holder is anonymous w.r.t. our user
		// table — capture tenant from the run record so the entry lands
		// inside the right tenant's audit view. ActorEmail empty is
		// intentional: the only identity we have is "whoever held the
		// magic link", which is captured by jti.
		recordAuditUnauth(c, deps.Audit, mongodb.AuditApprovalRedeemed,
			"", // actorEmail unknown
			"", // userID unknown
			rec.TenantID,
			map[string]any{
				"run_id":   runID,
				"jti":      claims.ID,
				"decision": req.Decision,
				"kind":     rec.PendingApproval.Kind,
			},
		)

		c.JSON(http.StatusOK, gin.H{
			"ok":       true,
			"decision": req.Decision,
			"run_id":   runID,
		})
	}
}

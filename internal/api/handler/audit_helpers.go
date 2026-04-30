// Audit helpers — thin wrappers so handlers call one line per
// privileged action instead of repeating the user/tenant/IP/UA pull.

package handler

import (
	"context"
	"log/slog"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/auth"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
)

// recordAudit persists one audit entry with caller info pulled from
// the gin context. Best-effort: failures are logged at warn but never
// surface to the user-facing response. Pass (c, repo, action, target,
// metadata) — target/metadata may be nil.
//
// Why detached ctx for the write: the request ctx may be cancelled
// before the audit write finishes (e.g. client disconnects post-200).
// Auditing is the system-of-record for security review — we don't
// want benign disconnects causing audit gaps.
func recordAudit(
	c *gin.Context,
	repo *mongodb.AuditRepository,
	action mongodb.AuditAction,
	target map[string]any,
	metadata map[string]any,
) {
	if repo == nil {
		return
	}
	uctx, _ := auth.UserFromCtx(c.Request.Context())
	tenantID, _ := auth.TenantFromCtx(c.Request.Context())

	entry := mongodb.AuditEntry{
		ID:         ulid.Make().String(),
		TenantID:   tenantID,
		UserID:     uctx.ID,
		ActorEmail: uctx.Email,
		Action:     action,
		Target:     target,
		IP:         c.ClientIP(),
		UserAgent:  c.Request.UserAgent(),
		Metadata:   metadata,
	}

	go func(e mongodb.AuditEntry) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := repo.Record(ctx, e); err != nil {
			slog.Warn("audit: record failed (non-fatal)", "action", e.Action, "err", err)
		}
	}(entry)
}

// recordAuditUnauth is for events that fire WITHOUT an authenticated
// caller (e.g. login_failure, password_reset_request for unknown email,
// or login_success where the ctx isn't yet stamped). Caller passes
// userID + tenantID explicitly when known so the entry lands inside
// the tenant-scoped /audit_log view; pass empty strings when unknown
// (those rows are visible only via cross-tenant admin tooling).
func recordAuditUnauth(
	c *gin.Context,
	repo *mongodb.AuditRepository,
	action mongodb.AuditAction,
	actorEmail, userID, tenantID string,
	metadata map[string]any,
) {
	if repo == nil {
		return
	}
	entry := mongodb.AuditEntry{
		ID:         ulid.Make().String(),
		TenantID:   tenantID,
		UserID:     userID,
		ActorEmail: actorEmail,
		Action:     action,
		IP:         c.ClientIP(),
		UserAgent:  c.Request.UserAgent(),
		Metadata:   metadata,
	}
	go func(e mongodb.AuditEntry) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := repo.Record(ctx, e); err != nil {
			slog.Warn("audit: record failed (non-fatal)", "action", e.Action, "err", err)
		}
	}(entry)
}

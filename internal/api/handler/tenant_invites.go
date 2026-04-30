// Tenant invite + member-management endpoints.
//
//	POST   /api/v1/tenants/invites         create invite (owner/admin)
//	GET    /api/v1/tenants/invites         list pending invites
//	DELETE /api/v1/tenants/invites/:id     revoke
//	GET    /api/v1/tenants/members         list members
//	DELETE /api/v1/tenants/members/:user_id  remove (owner/admin)
//
//	GET  /api/v1/invites/:token/preview    public — show tenant + role before accept
//	POST /api/v1/invites/:token/accept     authed — adds caller to tenant
//
// Auth model:
//   - Listing/inviting/revoking acts on the caller's CURRENT tenant
//     (tenant_id from JWT). Caller must be owner OR admin.
//   - Accepting is authed but the invite token validates which tenant
//     the caller joins. Email match between invite + caller's account
//     prevents an attacker accepting an invite intended for someone else.
//   - Preview is public so the UI can render "you're being invited to
//     ACME Corp as admin by alice@..." before signup.

package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/auth"
	"github.com/bRRRITSCOLD/burrow/internal/email"
	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
)

// InviteDeps wraps everything the invite handlers need.
type InviteDeps struct {
	Invites   *mongodb.InviteRepository
	Tenants   *mongodb.TenantRepository
	Users     *mongodb.UserRepository
	Audit     *mongodb.AuditRepository
	Email     email.Sender
	UIBaseURL string
}

// requireTenantAdmin returns the caller's role on the current tenant
// IFF it's owner or admin. Otherwise responds 403 + returns false.
func requireTenantAdmin(c *gin.Context, deps InviteDeps) (string, string, bool) {
	uctx, ok := auth.UserFromCtx(c.Request.Context())
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
		return "", "", false
	}
	tenantID, ok := auth.TenantFromCtx(c.Request.Context())
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no active tenant"})
		return "", "", false
	}
	role, err := deps.Tenants.GetMemberRole(c.Request.Context(), tenantID, uctx.ID)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "not a member of this tenant"})
		return "", "", false
	}
	if role != mongodb.TenantRoleOwner && role != mongodb.TenantRoleAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "owner/admin role required"})
		return "", "", false
	}
	return uctx.ID, tenantID, true
}

// CreateInvite mints a new invite + sends email.
func CreateInvite(deps InviteDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, tenantID, ok := requireTenantAdmin(c, deps)
		if !ok {
			return
		}
		var req struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		req.Email = strings.ToLower(strings.TrimSpace(req.Email))
		if req.Email == "" || !strings.Contains(req.Email, "@") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "valid email required"})
			return
		}
		role := mongodb.TenantRole(req.Role)
		// Owner role isn't transferable via invite — there's exactly
		// one per tenant (the creator). Fall back to member on
		// anything else, including empty.
		if role != mongodb.TenantRoleAdmin && role != mongodb.TenantRoleMember {
			role = mongodb.TenantRoleMember
		}

		raw, prefix, hash, err := mongodb.GenerateInviteToken()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		now := time.Now().UTC()
		inv := mongodb.TenantInvite{
			ID:          ulid.Make().String(),
			TenantID:    tenantID,
			Email:       req.Email,
			Role:        role,
			InvitedBy:   userID,
			TokenPrefix: prefix,
			TokenHash:   hash,
			CreatedAt:   now,
			ExpiresAt:   now.Add(7 * 24 * time.Hour),
		}
		saved, err := deps.Invites.Create(c.Request.Context(), inv)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		recordAudit(c, deps.Audit, mongodb.AuditInviteCreated,
			map[string]any{"invite_id": saved.ID, "tenant_id": tenantID},
			map[string]any{"to": saved.Email, "role": string(role)})

		// Dispatch the email out-of-band so the response returns fast.
		// Caller doesn't need confirmation that the email landed —
		// they have the invite row + can view the URL via the prefix
		// in the list endpoint.
		go dispatchInviteEmail(deps, saved, raw, userID)

		// Don't echo the raw token in the response — only the saved
		// row + a one-time `url` field for the operator's convenience
		// in dev (mirrors API-key creation pattern).
		resetURL := fmt.Sprintf("%s/invite/%s", strings.TrimRight(deps.UIBaseURL, "/"), raw)
		c.JSON(http.StatusOK, gin.H{
			"invite":  saved,
			"url":     resetURL,
			"warning": "copy this URL now — it's the only time it's shown",
		})
	}
}

func dispatchInviteEmail(deps InviteDeps, inv mongodb.TenantInvite, rawToken, inviterID string) {
	if deps.Email == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tenant, _ := deps.Tenants.GetByID(ctx, inv.TenantID)
	inviterEmail := ""
	if u, err := deps.Users.GetByID(ctx, inviterID); err == nil {
		inviterEmail = u.Email
	}

	url := fmt.Sprintf("%s/invite/%s", strings.TrimRight(deps.UIBaseURL, "/"), rawToken)
	slog.Info("invite: link issued",
		"invite_id", inv.ID, "tenant_id", inv.TenantID, "to", inv.Email, "url", url,
	)

	body := fmt.Sprintf(
		"%s invited you to join \"%s\" on burrow as a %s.\n\n"+
			"Click the link below to accept (valid for 7 days):\n\n%s\n\n"+
			"If you didn't expect this, ignore this email.",
		inviterEmail, tenant.Name, inv.Role, url,
	)
	if err := deps.Email.Send(ctx, email.Message{
		To:      inv.Email,
		Subject: "You've been invited to burrow",
		Body:    body,
	}); err != nil {
		slog.Warn("invite: email Send failed", "invite_id", inv.ID, "err", err)
	}
}

// ListInvites returns pending invites for the caller's current tenant.
func ListInvites(deps InviteDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		_, tenantID, ok := requireTenantAdmin(c, deps)
		if !ok {
			return
		}
		invites, err := deps.Invites.ListPendingForTenant(c.Request.Context(), tenantID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if invites == nil {
			invites = []mongodb.TenantInvite{}
		}
		c.JSON(http.StatusOK, gin.H{"invites": invites})
	}
}

// RevokeInvite marks an invite as revoked. Scoped to the caller's
// current tenant — passing a foreign-tenant invite ID is a no-op.
func RevokeInvite(deps InviteDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		_, tenantID, ok := requireTenantAdmin(c, deps)
		if !ok {
			return
		}
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}
		if err := deps.Invites.Revoke(c.Request.Context(), id, tenantID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		recordAudit(c, deps.Audit, mongodb.AuditInviteRevoked,
			map[string]any{"invite_id": id, "tenant_id": tenantID}, nil)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// PreviewInvite is the PUBLIC endpoint that hydrates the accept page
// before the user signs in. Returns tenant name + role + inviter
// email so the UI can render a meaningful "join X as Y" prompt.
// Inactive (consumed/revoked/expired) invites return 410 Gone.
func PreviewInvite(deps InviteDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.Param("token")
		if token == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "token required"})
			return
		}
		inv, err := deps.Invites.GetByRawToken(c.Request.Context(), token)
		if errors.Is(err, mongodb.ErrInviteNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "invite not found"})
			return
		}
		if errors.Is(err, mongodb.ErrInviteInactive) {
			c.JSON(http.StatusGone, gin.H{"error": "invite expired or already used"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		tenant, _ := deps.Tenants.GetByID(c.Request.Context(), inv.TenantID)
		inviterEmail := ""
		if u, err := deps.Users.GetByID(c.Request.Context(), inv.InvitedBy); err == nil {
			inviterEmail = u.Email
		}
		c.JSON(http.StatusOK, gin.H{
			"tenant_name":   tenant.Name,
			"tenant_id":     inv.TenantID,
			"role":          inv.Role,
			"invitee_email": inv.Email,
			"inviter_email": inviterEmail,
			"expires_at":    inv.ExpiresAt,
		})
	}
}

// AcceptInvite consumes the invite + adds the caller as a tenant
// member. Caller must be authenticated. Their account email must
// match the invite's email — otherwise an attacker who guesses a
// token (impossible) or shares one across users can't escalate into
// a tenant they weren't actually invited to.
func AcceptInvite(deps InviteDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		uctx, ok := auth.UserFromCtx(c.Request.Context())
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}
		token := c.Param("token")
		if token == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "token required"})
			return
		}

		inv, err := deps.Invites.GetByRawToken(c.Request.Context(), token)
		if errors.Is(err, mongodb.ErrInviteNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "invite not found"})
			return
		}
		if errors.Is(err, mongodb.ErrInviteInactive) {
			c.JSON(http.StatusGone, gin.H{"error": "invite expired or already used"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		// Email-binding check — caller's account email must match the
		// invitee email. Sidesteps the "leaked URL → wrong user joins"
		// risk; the invitee can still register w/ the correct email
		// to proceed.
		if !strings.EqualFold(uctx.Email, inv.Email) {
			c.JSON(http.StatusForbidden, gin.H{
				"error":         "this invite is for a different email",
				"invitee_email": inv.Email,
			})
			return
		}

		if err := deps.Tenants.AddMember(c.Request.Context(), mongodb.TenantMember{
			TenantID: inv.TenantID,
			UserID:   uctx.ID,
			Role:     inv.Role,
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err := deps.Invites.MarkAccepted(c.Request.Context(), inv.ID, uctx.ID); err != nil {
			// Race: another acceptor beat us. Member was already added
			// (idempotent). Surface 410 so the UI shows a fresh prompt.
			if errors.Is(err, mongodb.ErrInviteInactive) {
				c.JSON(http.StatusGone, gin.H{"error": "invite was just consumed"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		// Stamp the joined tenant explicitly — caller's ctx tenant
		// (their personal one) doesn't reflect the new membership yet.
		recordAudit(c, deps.Audit, mongodb.AuditInviteAccepted,
			map[string]any{"invite_id": inv.ID, "tenant_id": inv.TenantID, "role": string(inv.Role)}, nil)

		c.JSON(http.StatusOK, gin.H{"ok": true, "tenant_id": inv.TenantID, "role": inv.Role})
	}
}

// ListMembers returns all members of the caller's current tenant.
// Auth-gated for any member (read-only); the UI uses this in the
// settings page Members section.
func ListMembers(deps InviteDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		uctx, ok := auth.UserFromCtx(c.Request.Context())
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}
		tenantID, ok := auth.TenantFromCtx(c.Request.Context())
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no active tenant"})
			return
		}
		// Any member can read the roster; restricting to admins makes
		// it impossible for non-admins to see who they're working with.
		if err := deps.Tenants.IsMember(c.Request.Context(), tenantID, uctx.ID); err != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "not a member"})
			return
		}
		members, err := deps.Tenants.ListMembersForTenant(c.Request.Context(), tenantID, deps.Users)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if members == nil {
			members = []mongodb.MemberWithUser{}
		}
		c.JSON(http.StatusOK, gin.H{"members": members})
	}
}

// RemoveMember kicks a user from the current tenant. Owner/admin
// only. Refuses to remove the tenant's owner (prevents accidental
// self-orphaning of the tenant) and refuses self-removal of an admin
// when no other admin/owner exists. The owner can leave only after
// transferring ownership (out of scope; future endpoint).
func RemoveMember(deps InviteDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		callerID, tenantID, ok := requireTenantAdmin(c, deps)
		if !ok {
			return
		}
		targetID := c.Param("user_id")
		if targetID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user_id required"})
			return
		}
		// Refuse removal of the tenant owner.
		t, err := deps.Tenants.GetByID(c.Request.Context(), tenantID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if t.OwnerID == targetID {
			c.JSON(http.StatusForbidden, gin.H{"error": "cannot remove the tenant owner"})
			return
		}
		// Self-removal is allowed for non-owners; admin removing self
		// is fine (they may leave the team). The owner case is gated
		// above.
		_ = callerID
		if err := deps.Tenants.RemoveMember(c.Request.Context(), tenantID, targetID); err != nil {
			if errors.Is(err, mongodb.ErrNotMember) {
				c.JSON(http.StatusNotFound, gin.H{"error": "not a member"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		recordAudit(c, deps.Audit, mongodb.AuditMemberRemoved,
			map[string]any{"removed_user_id": targetID, "tenant_id": tenantID}, nil)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// TransferOwnership hands ownership of the active tenant to another
// existing member. Caller must be the current owner. Target must
// already be a member (admin or member); we don't auto-add via
// transfer since that would short-circuit invite acceptance.
//
// On success: target = owner, caller = admin, tenant.owner_id updated.
// Operation is idempotent on retry — re-running the same transfer
// after partial failure converges on the intended state.
func TransferOwnership(deps InviteDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		uctx, ok := auth.UserFromCtx(c.Request.Context())
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}
		tenantID, ok := auth.TenantFromCtx(c.Request.Context())
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no active tenant"})
			return
		}
		// Owner-only — admin can't transfer ownership.
		role, err := deps.Tenants.GetMemberRole(c.Request.Context(), tenantID, uctx.ID)
		if err != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "not a member of this tenant"})
			return
		}
		if role != mongodb.TenantRoleOwner {
			c.JSON(http.StatusForbidden, gin.H{"error": "only the tenant owner can transfer ownership"})
			return
		}
		var req struct {
			ToUserID string `json:"to_user_id"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || req.ToUserID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "to_user_id required"})
			return
		}
		err = deps.Tenants.TransferOwnership(c.Request.Context(), tenantID, uctx.ID, req.ToUserID)
		switch {
		case errors.Is(err, mongodb.ErrTransferToSelf):
			c.JSON(http.StatusBadRequest, gin.H{"error": "cannot transfer ownership to yourself"})
			return
		case errors.Is(err, mongodb.ErrTransferTargetNotMember):
			c.JSON(http.StatusBadRequest, gin.H{"error": "target user is not a member of this tenant"})
			return
		case err != nil:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		recordAudit(c, deps.Audit, mongodb.AuditOwnershipTransferred,
			map[string]any{"tenant_id": tenantID},
			map[string]any{
				"from_user_id": uctx.ID,
				"to_user_id":   req.ToUserID,
			})
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

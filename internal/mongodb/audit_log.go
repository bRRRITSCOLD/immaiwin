// Audit log store — append-only ledger of privileged actions.
//
// Goals:
//   - "Who did what when" answerable in constant time per tenant
//   - Survives reset of any other collection (audit is the system-of-
//     record for security review)
//   - Unindexed metadata field carries action-specific context without
//     schema churn (key_id for api_key revoke, provider for oauth unlink, etc.)
//
// Non-goals (deferred):
//   - Tamper-evident hash chain (write-only via single API path is
//     enough until external auditors care)
//   - PII redaction policy (caller decides what to put in metadata)
//   - Cross-tenant aggregation views
//
// Failure mode: Record errors are logged but never block the user-
// facing response. Auditing is observability, not gating — a Mongo
// blip shouldn't refuse a password change.

package mongodb

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// AuditAction is a stable enum of recorded actions. New actions
// extend the list; existing values never re-shape (downstream
// dashboards/alerts depend on these strings).
type AuditAction string

const (
	AuditLoginSuccess         AuditAction = "login_success"
	AuditLoginFailure         AuditAction = "login_failure"
	AuditLogout               AuditAction = "logout"
	AuditPasswordChange       AuditAction = "password_change"
	AuditPasswordResetRequest AuditAction = "password_reset_requested"
	AuditPasswordResetConfirm AuditAction = "password_reset_completed"
	AuditAPIKeyCreated        AuditAction = "api_key_created"
	AuditAPIKeyRevoked        AuditAction = "api_key_revoked"
	AuditOAuthLinked          AuditAction = "oauth_linked"
	AuditOAuthUnlinked        AuditAction = "oauth_unlinked"
	AuditTenantSwitch         AuditAction = "tenant_switch"
	AuditInviteCreated        AuditAction = "invite_created"
	AuditInviteRevoked        AuditAction = "invite_revoked"
	AuditInviteAccepted       AuditAction = "invite_accepted"
	AuditMemberRemoved        AuditAction = "member_removed"
	AuditOwnershipTransferred AuditAction = "tenant_ownership_transferred"
	AuditWorkflowDuplicated   AuditAction = "workflow_duplicated"
	AuditApprovalRedeemed     AuditAction = "approval_redeemed"
)

// AuditEntry is the persisted shape.
type AuditEntry struct {
	ID          string         `bson:"_id"          json:"id"`
	TS          time.Time      `bson:"ts"           json:"ts"`
	TenantID    string         `bson:"tenant_id,omitempty"    json:"tenant_id,omitempty"`
	UserID      string         `bson:"user_id,omitempty"      json:"user_id,omitempty"`
	ActorEmail  string         `bson:"actor_email,omitempty"  json:"actor_email,omitempty"`
	Action      AuditAction    `bson:"action"       json:"action"`
	Target      map[string]any `bson:"target,omitempty"       json:"target,omitempty"`
	IP          string         `bson:"ip,omitempty"           json:"ip,omitempty"`
	UserAgent   string         `bson:"user_agent,omitempty"   json:"user_agent,omitempty"`
	Metadata    map[string]any `bson:"metadata,omitempty"     json:"metadata,omitempty"`
}

// AuditRepository wraps the audit_log collection.
type AuditRepository struct {
	col *mongo.Collection
}

func NewAuditRepository(ctx context.Context, db *mongo.Database) (*AuditRepository, error) {
	col := db.Collection("audit_log")
	_, err := col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		// Primary read pattern: list latest for a tenant.
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "ts", Value: -1}}},
		// "What did this user do" view — defer-build but cheap to add now.
		{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "ts", Value: -1}}},
		// Secondary filter on action — useful for "show me all
		// password_change events tenant-wide".
		{Keys: bson.D{{Key: "action", Value: 1}, {Key: "ts", Value: -1}}},
	})
	if err != nil {
		return nil, err
	}
	return &AuditRepository{col: col}, nil
}

// Record appends an entry. Always sets ts = now (caller-provided ts
// is overwritten — clock skew between processes makes "trust the
// caller" bug-prone). ID minted upstream via ulid.
func (r *AuditRepository) Record(ctx context.Context, e AuditEntry) error {
	if e.Action == "" {
		return errors.New("audit: action required")
	}
	e.TS = time.Now().UTC()
	_, err := r.col.InsertOne(ctx, e)
	return err
}

// AuditFilter is the shape used by the read endpoint. All fields
// optional; empty filter returns the most recent entries within Limit.
type AuditFilter struct {
	TenantID string      // empty = unscoped (admin tooling only)
	UserID   string      // filter to a single actor
	Action   AuditAction // empty = any action
	Since    time.Time   // zero = no lower bound
	Until    time.Time   // zero = no upper bound
	Limit    int         // default 50, max 500
	Skip     int
}

// ListWithFilter returns matching entries sorted by ts descending.
func (r *AuditRepository) ListWithFilter(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q := bson.M{}
	if f.TenantID != "" {
		q["tenant_id"] = f.TenantID
	}
	if f.UserID != "" {
		q["user_id"] = f.UserID
	}
	if f.Action != "" {
		q["action"] = f.Action
	}
	if !f.Since.IsZero() || !f.Until.IsZero() {
		tsQ := bson.M{}
		if !f.Since.IsZero() {
			tsQ["$gte"] = f.Since
		}
		if !f.Until.IsZero() {
			tsQ["$lte"] = f.Until
		}
		q["ts"] = tsQ
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "ts", Value: -1}}).
		SetLimit(int64(limit)).
		SetSkip(int64(f.Skip))
	cur, err := r.col.Find(ctx, q, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close(ctx) }()
	var out []AuditEntry
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

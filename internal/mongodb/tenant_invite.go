// Tenant invite store — backs the team-invite flow.
//
// Storage shape mirrors api_keys: we never persist the raw invite
// token. Only the SHA256 hash + a leading prefix for audit visibility.
// The raw token only travels to the invitee via email/URL.
//
// Lifecycle:
//   - Create: invitee email + role + 7-day expiry + token hash
//   - Accept: caller passes raw token → hash lookup → MarkAccepted
//   - Revoke: owner/admin sets revoked_at; future Accept calls fail
//
// Single-use enforcement is via accepted_at/revoked_at on the doc
// (not Redis like password_reset) because invites have a longer
// lifecycle and a clear "active member row" that supersedes them
// once accepted.

package mongodb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// InvitePrefix marks the leading bytes of a raw invite token. Same
// pattern as APIKeyPrefix: lets eyes spot one in a log.
const InvitePrefix = "inv_"

// TenantInvite is the persisted invite record.
type TenantInvite struct {
	ID          string     `bson:"_id"          json:"id"`
	TenantID    string     `bson:"tenant_id"    json:"tenant_id"`
	Email       string     `bson:"email"        json:"email"` // lowercased; matched against accepter's account
	Role        TenantRole `bson:"role"         json:"role"`  // member | admin (never owner)
	InvitedBy   string     `bson:"invited_by"   json:"invited_by"`
	TokenPrefix string     `bson:"token_prefix" json:"token_prefix"` // first 12 chars; visible in audit views
	TokenHash   string     `bson:"token_hash"   json:"-"`            // sha256 hex; never returned
	CreatedAt   time.Time  `bson:"created_at"   json:"created_at"`
	ExpiresAt   time.Time  `bson:"expires_at"   json:"expires_at"`
	AcceptedAt  *time.Time `bson:"accepted_at,omitempty" json:"accepted_at,omitempty"`
	AcceptedBy  string     `bson:"accepted_by,omitempty" json:"accepted_by,omitempty"`
	RevokedAt   *time.Time `bson:"revoked_at,omitempty"  json:"revoked_at,omitempty"`
}

// IsActive returns true when the invite is still usable (not expired,
// not accepted, not revoked).
func (i TenantInvite) IsActive(now time.Time) bool {
	if i.AcceptedAt != nil || i.RevokedAt != nil {
		return false
	}
	return now.Before(i.ExpiresAt)
}

// ErrInviteNotFound / ErrInviteInactive distinguish "wrong token" from
// "right token but consumed/expired/revoked" so handlers can map cleanly.
var (
	ErrInviteNotFound = errors.New("invite not found")
	ErrInviteInactive = errors.New("invite no longer active")
)

// InviteRepository wraps the tenant_invites collection.
type InviteRepository struct {
	col *mongo.Collection
}

// NewInviteRepository sets up indexes + returns the repo.
func NewInviteRepository(ctx context.Context, db *mongo.Database) (*InviteRepository, error) {
	col := db.Collection("tenant_invites")
	_, err := col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "token_hash", Value: 1}},
			Options: options.Index().SetUnique(true),
		},
		// "List pending invites for tenant" path — bounded scan over
		// (tenant, expires_at) for quick UI rendering of pending rows.
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "created_at", Value: -1}}},
		// Reverse lookup by invitee email — useful for "what invites
		// am I waiting on" UX in the future.
		{Keys: bson.D{{Key: "email", Value: 1}}},
	})
	if err != nil {
		return nil, err
	}
	return &InviteRepository{col: col}, nil
}

// GenerateInviteToken returns a random 32-byte URL-safe token prefixed
// with InvitePrefix. The raw token is shown ONCE — at email-send time
// — and never persisted. Only its hash + prefix live in Mongo.
func GenerateInviteToken() (raw, prefix, hash string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", err
	}
	raw = InvitePrefix + hex.EncodeToString(buf)
	if len(raw) >= 12 {
		prefix = raw[:12]
	} else {
		prefix = raw
	}
	sum := sha256.Sum256([]byte(raw))
	hash = hex.EncodeToString(sum[:])
	return raw, prefix, hash, nil
}

// hashInviteToken matches the constant-time lookup pattern used by
// API keys. Plain SHA256 is collision-safe over 32-byte random
// inputs; bcrypt cost would be wasteful here.
func hashInviteToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Create persists an invite. Caller mints (raw, prefix, hash) via
// GenerateInviteToken and is responsible for emailing the raw value.
func (r *InviteRepository) Create(ctx context.Context, inv TenantInvite) (TenantInvite, error) {
	if inv.ID == "" || inv.TenantID == "" || inv.Email == "" || inv.TokenHash == "" {
		return TenantInvite{}, errors.New("invite: id/tenant/email/token_hash required")
	}
	inv.Email = strings.ToLower(strings.TrimSpace(inv.Email))
	if inv.CreatedAt.IsZero() {
		inv.CreatedAt = time.Now().UTC()
	}
	if inv.ExpiresAt.IsZero() {
		inv.ExpiresAt = inv.CreatedAt.Add(7 * 24 * time.Hour)
	}
	if _, err := r.col.InsertOne(ctx, inv); err != nil {
		return TenantInvite{}, err
	}
	return inv, nil
}

// GetByRawToken hashes the raw input and looks up the invite by its
// stored hash. Returns ErrInviteNotFound on miss, ErrInviteInactive
// when the row exists but is consumed/expired/revoked.
func (r *InviteRepository) GetByRawToken(ctx context.Context, raw string) (TenantInvite, error) {
	if raw == "" {
		return TenantInvite{}, ErrInviteNotFound
	}
	var inv TenantInvite
	err := r.col.FindOne(ctx, bson.M{"token_hash": hashInviteToken(raw)}).Decode(&inv)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return TenantInvite{}, ErrInviteNotFound
	}
	if err != nil {
		return TenantInvite{}, err
	}
	if !inv.IsActive(time.Now().UTC()) {
		return inv, ErrInviteInactive
	}
	return inv, nil
}

// ListPendingForTenant returns active invites (not accepted/revoked
// AND not yet expired) for the given tenant. Used by the settings
// page Members section.
func (r *InviteRepository) ListPendingForTenant(ctx context.Context, tenantID string) ([]TenantInvite, error) {
	now := time.Now().UTC()
	cur, err := r.col.Find(ctx, bson.M{
		"tenant_id":   tenantID,
		"accepted_at": bson.M{"$exists": false},
		"revoked_at":  bson.M{"$exists": false},
		"expires_at":  bson.M{"$gt": now},
	}, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close(ctx) }()
	var out []TenantInvite
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// MarkAccepted records who consumed the invite. Single-use guard:
// fails if AcceptedAt is already set.
func (r *InviteRepository) MarkAccepted(ctx context.Context, inviteID, userID string) error {
	now := time.Now().UTC()
	res, err := r.col.UpdateOne(ctx,
		bson.M{
			"_id":         inviteID,
			"accepted_at": bson.M{"$exists": false},
			"revoked_at":  bson.M{"$exists": false},
		},
		bson.M{"$set": bson.M{
			"accepted_at": now,
			"accepted_by": userID,
		}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		// Row exists but the guard rejected — already accepted/revoked.
		return ErrInviteInactive
	}
	return nil
}

// Revoke marks an invite as revoked. Idempotent — already-accepted
// invites are left alone (they ARE inactive anyway), already-revoked
// invites stay revoked.
func (r *InviteRepository) Revoke(ctx context.Context, inviteID, tenantID string) error {
	now := time.Now().UTC()
	_, err := r.col.UpdateOne(ctx,
		bson.M{
			"_id":         inviteID,
			"tenant_id":   tenantID,
			"accepted_at": bson.M{"$exists": false},
			"revoked_at":  bson.M{"$exists": false},
		},
		bson.M{"$set": bson.M{"revoked_at": now}},
	)
	return err
}

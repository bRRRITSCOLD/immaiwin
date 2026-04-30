// Tenant store — multi-user-per-tenant model. On user registration
// we mint a personal tenant w/ the user as owner; users can be added
// to additional tenants via membership rows (invite flow deferred to
// post-launch backlog — for now, owner adds members directly).
//
// Scoping rule: every per-user resource (workflows, runs, connections,
// evals, chat memory) carries a `tenant_id`. Stores filter by ctx
// tenant. The `OwnerID` on Tenant is informational — actual access
// goes through tenant_members membership rows.

package mongodb

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TenantRole is the membership role within a single tenant.
type TenantRole string

const (
	TenantRoleOwner  TenantRole = "owner"
	TenantRoleAdmin  TenantRole = "admin"
	TenantRoleMember TenantRole = "member"
)

// Tenant is a workspace that owns workflows, runs, connections, etc.
type Tenant struct {
	ID        string    `bson:"_id"        json:"id"`
	Name      string    `bson:"name"       json:"name"`
	OwnerID   string    `bson:"owner_id"   json:"owner_id"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
}

// TenantMember binds a user to a tenant w/ a role. Compound (tenant_id,
// user_id) uniqueness prevents double-membership; both fields indexed
// for fast "list tenants for user" + "list users for tenant".
type TenantMember struct {
	TenantID string     `bson:"tenant_id" json:"tenant_id"`
	UserID   string     `bson:"user_id"   json:"user_id"`
	Role     TenantRole `bson:"role"      json:"role"`
	JoinedAt time.Time  `bson:"joined_at" json:"joined_at"`
}

// ErrTenantNotFound is returned by Get on a missing tenant.
var ErrTenantNotFound = errors.New("tenant not found")

// ErrNotMember is returned by access checks when a user isn't a member.
var ErrNotMember = errors.New("user is not a member of tenant")

// TenantRepository wraps the tenants + tenant_members collections.
type TenantRepository struct {
	tenants *mongo.Collection
	members *mongo.Collection
}

// NewTenantRepository ensures the schema indexes are present + returns
// the repo. Idempotent.
func NewTenantRepository(ctx context.Context, db *mongo.Database) (*TenantRepository, error) {
	tenants := db.Collection("tenants")
	members := db.Collection("tenant_members")
	if _, err := tenants.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "owner_id", Value: 1}}},
	}); err != nil {
		return nil, err
	}
	if _, err := members.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "tenant_id", Value: 1}, {Key: "user_id", Value: 1}},
			Options: options.Index().SetUnique(true),
		},
		// Reverse lookup: which tenants does this user belong to.
		{Keys: bson.D{{Key: "user_id", Value: 1}}},
	}); err != nil {
		return nil, err
	}
	return &TenantRepository{tenants: tenants, members: members}, nil
}

// CreateWithOwner inserts a tenant + adds the given user as owner in
// one go. Returns the persisted tenant.
func (r *TenantRepository) CreateWithOwner(ctx context.Context, t Tenant) (Tenant, error) {
	if t.ID == "" {
		return Tenant{}, errors.New("tenant: id required")
	}
	if t.OwnerID == "" {
		return Tenant{}, errors.New("tenant: owner_id required")
	}
	now := time.Now().UTC()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	if _, err := r.tenants.InsertOne(ctx, t); err != nil {
		return Tenant{}, err
	}
	mem := TenantMember{
		TenantID: t.ID,
		UserID:   t.OwnerID,
		Role:     TenantRoleOwner,
		JoinedAt: now,
	}
	if _, err := r.members.InsertOne(ctx, mem); err != nil {
		return Tenant{}, err
	}
	return t, nil
}

// GetByID looks up a tenant. Returns ErrTenantNotFound when missing.
func (r *TenantRepository) GetByID(ctx context.Context, id string) (Tenant, error) {
	var t Tenant
	err := r.tenants.FindOne(ctx, bson.M{"_id": id}).Decode(&t)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Tenant{}, ErrTenantNotFound
	}
	if err != nil {
		return Tenant{}, err
	}
	return t, nil
}

// ListMembershipsForUser returns every (tenant, role) pair this user
// belongs to. Used by /auth/me to populate the tenant picker + by
// the JWT issuer to pick a default active tenant.
type Membership struct {
	Tenant Tenant     `json:"tenant"`
	Role   TenantRole `json:"role"`
}

func (r *TenantRepository) ListMembershipsForUser(ctx context.Context, userID string) ([]Membership, error) {
	cur, err := r.members.Find(ctx, bson.M{"user_id": userID})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx) //nolint:errcheck
	var rows []TenantMember
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	out := make([]Membership, 0, len(rows))
	for _, m := range rows {
		t, err := r.GetByID(ctx, m.TenantID)
		if err != nil {
			continue // tenant deleted? skip silently
		}
		out = append(out, Membership{Tenant: t, Role: m.Role})
	}
	return out, nil
}

// IsMember verifies access for a (tenant, user) pair. Returns nil on
// match, ErrNotMember otherwise. Used by per-request authorization.
func (r *TenantRepository) IsMember(ctx context.Context, tenantID, userID string) error {
	count, err := r.members.CountDocuments(ctx, bson.M{
		"tenant_id": tenantID,
		"user_id":   userID,
	})
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotMember
	}
	return nil
}

// AddMember inserts a (tenant, user, role) row. Idempotent on duplicate
// — Mongo's unique index returns DuplicateKey which we swallow so
// callers can re-invite without bookkeeping.
func (r *TenantRepository) AddMember(ctx context.Context, m TenantMember) error {
	if m.JoinedAt.IsZero() {
		m.JoinedAt = time.Now().UTC()
	}
	_, err := r.members.InsertOne(ctx, m)
	if err != nil && mongo.IsDuplicateKeyError(err) {
		return nil
	}
	return err
}

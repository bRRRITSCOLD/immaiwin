// User store — backs the auth system.
//
// Users live in their own collection; per-user data (workflows,
// connections, runs, etc.) carries a tenant_id once Phase B lands.
// Email is unique-indexed so a duplicate registration fails fast.

package mongodb

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// User is the persisted shape. PasswordHash is bcrypt; never store
// plaintext. OAuth fields land in Phase E (Google/GitHub link).
type User struct {
	ID           string    `bson:"_id"                    json:"id"`
	Email        string    `bson:"email"                  json:"email"`
	PasswordHash string    `bson:"password_hash,omitempty" json:"-"`
	CreatedAt    time.Time `bson:"created_at"             json:"created_at"`
	UpdatedAt    time.Time `bson:"updated_at"             json:"updated_at"`
	// LastLoginAt updates on every successful login. Useful for the
	// future "abandoned account" cleanup + observability.
	LastLoginAt *time.Time `bson:"last_login_at,omitempty" json:"last_login_at,omitempty"`
	// OAuthProviders carries linked-account info; one entry per
	// provider (google/github/...). Phase E populates this.
	OAuthProviders []OAuthLink `bson:"oauth_providers,omitempty" json:"oauth_providers,omitempty"`
}

// OAuthLink is one (provider, subject) tuple for a user. Subject is
// the provider's stable user ID (e.g. Google "sub" claim).
type OAuthLink struct {
	Provider  string    `bson:"provider"   json:"provider"` // "google" | "github"
	Subject   string    `bson:"subject"    json:"subject"`
	Email     string    `bson:"email,omitempty" json:"email,omitempty"`
	LinkedAt  time.Time `bson:"linked_at"  json:"linked_at"`
}

// ErrUserNotFound is returned when GetByEmail / GetByID can't match.
// Distinct error type so handlers can map cleanly to 404 vs 500.
var ErrUserNotFound = errors.New("user not found")

// ErrUserExists is returned by Create on a duplicate email.
var ErrUserExists = errors.New("user already exists")

// UserRepository wraps the users collection.
type UserRepository struct {
	col *mongo.Collection
}

// NewUserRepository constructs a UserRepository + ensures the unique
// email index. Idempotent — re-creating the index on an existing
// collection is a no-op when the spec matches.
func NewUserRepository(ctx context.Context, db *mongo.Database) (*UserRepository, error) {
	col := db.Collection("users")
	_, err := col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "email", Value: 1}},
			Options: options.Index().SetUnique(true),
		},
		{
			// Sparse index on (provider, subject) so OAuth linking has
			// a fast lookup. Sparse so users without OAuth don't
			// occupy index slots.
			Keys: bson.D{
				{Key: "oauth_providers.provider", Value: 1},
				{Key: "oauth_providers.subject", Value: 1},
			},
			Options: options.Index().SetSparse(true),
		},
	})
	if err != nil {
		return nil, err
	}
	return &UserRepository{col: col}, nil
}

// Create inserts a new user. Returns ErrUserExists on duplicate email.
func (r *UserRepository) Create(ctx context.Context, u User) (User, error) {
	if u.ID == "" {
		return User{}, errors.New("user: id required")
	}
	if u.Email == "" {
		return User{}, errors.New("user: email required")
	}
	now := time.Now().UTC()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	_, err := r.col.InsertOne(ctx, u)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return User{}, ErrUserExists
		}
		return User{}, err
	}
	return u, nil
}

// GetByEmail looks up a user by email (lowercased). Returns
// ErrUserNotFound when missing.
func (r *UserRepository) GetByEmail(ctx context.Context, email string) (User, error) {
	var u User
	err := r.col.FindOne(ctx, bson.M{"email": email}).Decode(&u)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}

// GetByID looks up by user ID. Same error semantics as GetByEmail.
func (r *UserRepository) GetByID(ctx context.Context, id string) (User, error) {
	var u User
	err := r.col.FindOne(ctx, bson.M{"_id": id}).Decode(&u)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}

// TouchLastLogin stamps LastLoginAt on every successful login. Best-
// effort; failures don't block the login response.
func (r *UserRepository) TouchLastLogin(ctx context.Context, id string) error {
	now := time.Now().UTC()
	_, err := r.col.UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"last_login_at": now, "updated_at": now}},
	)
	return err
}

// LinkOAuth appends a (provider, subject, email) tuple to the user's
// OAuthProviders slice IFF that pair isn't already linked. Idempotent —
// re-linking the same Google account from a second sign-in is a no-op.
//
// Why $addToSet over $push: $addToSet dedupes against equal documents,
// but our OAuthLink carries a LinkedAt timestamp that makes "equal"
// brittle. Match-then-update via two-stage filter is more reliable:
// "update only when this provider+subject pair is NOT already in the
// array". A single failed FindOne+InsertOne race could double-link;
// the unique-index-style guarantee here is "at most one link per
// (provider, subject) per user".
func (r *UserRepository) LinkOAuth(ctx context.Context, userID, provider, subject, email string) error {
	if userID == "" || provider == "" || subject == "" {
		return errors.New("LinkOAuth: userID/provider/subject required")
	}
	link := OAuthLink{
		Provider: provider,
		Subject:  subject,
		Email:    email,
		LinkedAt: time.Now().UTC(),
	}
	// Filter requires the user exists AND the (provider, subject) pair
	// does NOT already exist in oauth_providers. Mongo's $not + $elemMatch
	// scopes the negation to the array element rather than top-level.
	filter := bson.M{
		"_id": userID,
		"oauth_providers": bson.M{
			"$not": bson.M{
				"$elemMatch": bson.M{"provider": provider, "subject": subject},
			},
		},
	}
	_, err := r.col.UpdateOne(ctx, filter, bson.M{
		"$push": bson.M{"oauth_providers": link},
		"$set":  bson.M{"updated_at": link.LinkedAt},
	})
	return err
}

// UnlinkOAuth removes a (provider, subject) pair from the user's
// OAuthProviders. No-op when the pair isn't present. Used by the user-
// settings page to revoke a provider connection.
func (r *UserRepository) UnlinkOAuth(ctx context.Context, userID, provider string) error {
	now := time.Now().UTC()
	_, err := r.col.UpdateOne(ctx,
		bson.M{"_id": userID},
		bson.M{
			"$pull": bson.M{"oauth_providers": bson.M{"provider": provider}},
			"$set":  bson.M{"updated_at": now},
		},
	)
	return err
}

// UpdatePasswordHash replaces the user's bcrypt hash. Used by the
// password-change + password-reset paths. Both paths must verify
// caller authority before invoking this — repo-level write doesn't
// re-check.
func (r *UserRepository) UpdatePasswordHash(ctx context.Context, userID, newHash string) error {
	if userID == "" || newHash == "" {
		return errors.New("UpdatePasswordHash: userID + newHash required")
	}
	now := time.Now().UTC()
	_, err := r.col.UpdateOne(ctx,
		bson.M{"_id": userID},
		bson.M{"$set": bson.M{"password_hash": newHash, "updated_at": now}},
	)
	return err
}

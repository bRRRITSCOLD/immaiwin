// API key store. Keys are programmatic-access tokens issued per user
// (scoped to a tenant); a Bearer-bound counterpart to the JWT cookie
// the UI uses. Webhook senders, CLI clients, CI runners, etc. carry an
// API key in `Authorization: Bearer iwk_<...>`.
//
// Storage shape: never persist the raw key. We store
//   - KeyPrefix: first 12 chars (`iwk_xxxxxxxx`) for fast prefix lookup
//   - KeyHash:   sha256(rawKey) hex-encoded — constant-time comparable
//                without bcrypt's per-request cost (API keys hit on
//                every webhook fire, so bcrypt CPU is too heavy here;
//                sha256 of a 32-char random key is collision-safe)
//
// On creation we return the full key ONCE and never again — same
// model as GitHub PATs / Stripe restricted keys. Lost key = revoke +
// create new.

package mongodb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// APIKeyPrefix is the leading 4 chars on every key value so middleware
// can distinguish API keys from JWTs at-a-glance (JWTs start with
// base64 segments, never `iwk_`).
const APIKeyPrefix = "iwk_"

// APIKey is the persisted record. Raw key value is NOT stored — only
// hash + prefix.
type APIKey struct {
	ID         string     `bson:"_id"          json:"id"`
	UserID     string     `bson:"user_id"      json:"user_id"`
	TenantID   string     `bson:"tenant_id"    json:"tenant_id"`
	Name       string     `bson:"name"         json:"name"`
	KeyPrefix  string     `bson:"key_prefix"   json:"key_prefix"`           // shown in list UI: "iwk_abc12345"
	KeyHash    string     `bson:"key_hash"     json:"-"`                    // sha256 hex; never returned
	CreatedAt  time.Time  `bson:"created_at"   json:"created_at"`
	LastUsedAt *time.Time `bson:"last_used_at,omitempty" json:"last_used_at,omitempty"`
}

// ErrAPIKeyNotFound is returned by lookup when the key is wrong /
// revoked. Distinct error so middleware can map to 401 cleanly.
var ErrAPIKeyNotFound = errors.New("api key not found")

// APIKeyRepository wraps the api_keys collection.
type APIKeyRepository struct {
	col *mongo.Collection
}

// NewAPIKeyRepository ensures indexes + returns the repo.
//
// Indexes:
//   - key_prefix (unique): collisions on the 8-char prefix would force
//     a fallback scan; uniqueness keeps lookup O(1).
//   - user_id: list-keys-for-user fast path.
func NewAPIKeyRepository(ctx context.Context, db *mongo.Database) (*APIKeyRepository, error) {
	col := db.Collection("api_keys")
	_, err := col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "key_prefix", Value: 1}},
			Options: options.Index().SetUnique(true),
		},
		{Keys: bson.D{{Key: "user_id", Value: 1}}},
	})
	if err != nil {
		return nil, err
	}
	return &APIKeyRepository{col: col}, nil
}

// GenerateKey mints a fresh raw key. Format: `iwk_<48 hex chars>` =
// 24 random bytes hex-encoded. Plenty of entropy + URL-safe.
func GenerateKey() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("api key: rand: %w", err)
	}
	return APIKeyPrefix + hex.EncodeToString(buf), nil
}

// HashKey returns the sha256 hex of the raw key — what we persist.
func HashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// PrefixOf returns the lookup-prefix slice of the raw key. 12 chars
// (4-char `iwk_` + 8 hex) gives us a unique-per-key bucket while
// staying short enough to display safely in the UI.
func PrefixOf(raw string) string {
	if len(raw) < 12 {
		return raw
	}
	return raw[:12]
}

// Create persists a new key. Caller MUST capture the returned raw
// value + show it once — the DB record holds only the hash.
func (r *APIKeyRepository) Create(ctx context.Context, k APIKey, raw string) (APIKey, error) {
	if k.ID == "" || k.UserID == "" || k.TenantID == "" {
		return APIKey{}, errors.New("api key: id, user_id, tenant_id required")
	}
	k.KeyPrefix = PrefixOf(raw)
	k.KeyHash = HashKey(raw)
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC()
	}
	if _, err := r.col.InsertOne(ctx, k); err != nil {
		return APIKey{}, err
	}
	return k, nil
}

// LookupByRaw matches a raw key against the stored hash. Returns
// ErrAPIKeyNotFound on miss. O(1) — uses key_prefix unique index.
func (r *APIKeyRepository) LookupByRaw(ctx context.Context, raw string) (APIKey, error) {
	prefix := PrefixOf(raw)
	hash := HashKey(raw)
	var k APIKey
	err := r.col.FindOne(ctx, bson.M{"key_prefix": prefix, "key_hash": hash}).Decode(&k)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return APIKey{}, ErrAPIKeyNotFound
	}
	if err != nil {
		return APIKey{}, err
	}
	return k, nil
}

// ListForUser returns every key belonging to a user. KeyHash is
// stripped from the return shape via the json tag (-).
func (r *APIKeyRepository) ListForUser(ctx context.Context, userID string) ([]APIKey, error) {
	cur, err := r.col.Find(ctx, bson.M{"user_id": userID},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx) //nolint:errcheck
	var rows []APIKey
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// Revoke deletes the key by ID, scoped to the owning user (so a
// stolen JWT can't revoke someone else's keys).
func (r *APIKeyRepository) Revoke(ctx context.Context, id, userID string) error {
	res, err := r.col.DeleteOne(ctx, bson.M{"_id": id, "user_id": userID})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return ErrAPIKeyNotFound
	}
	return nil
}

// TouchLastUsed updates last_used_at. Called best-effort on each
// successful API-key auth — failures swallowed so a flaky write
// doesn't reject the request.
func (r *APIKeyRepository) TouchLastUsed(ctx context.Context, id string) {
	now := time.Now().UTC()
	_, _ = r.col.UpdateOne(ctx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"last_used_at": now}},
	)
}

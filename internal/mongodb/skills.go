package mongodb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/skills"
	"github.com/oklog/ulid/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// SkillRegistry persists the platform-wide library of installed skill
// versions in MongoDB. Implements skills.RegistryStore.
//
// Storage shape: one document per (slug_id, version) tuple in the
// `skills_registry` collection. Compound unique index on (slug_id,
// version) prevents duplicates; secondary index on `slug_id` powers fast
// version listing during resolution.
type SkillRegistry struct {
	col *mongo.Collection
}

// NewSkillRegistry constructs the repo + ensures indexes.
func NewSkillRegistry(ctx context.Context, db *mongo.Database) (*SkillRegistry, error) {
	col := db.Collection("skills_registry")
	_, err := col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "slug_id", Value: 1}, {Key: "version", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "slug_id", Value: 1}}},
	})
	if err != nil {
		return nil, err
	}
	return &SkillRegistry{col: col}, nil
}

// Compile-time check that the Mongo impl satisfies the skills contract.
var _ skills.RegistryStore = (*SkillRegistry)(nil)

// UpsertRecord implements skills.RegistryStore.
func (r *SkillRegistry) UpsertRecord(ctx context.Context, rec skills.SkillRecord) (skills.SkillRecord, error) {
	if rec.SlugID == "" || rec.Version == "" {
		return skills.SkillRecord{}, errors.New("mongodb/skills: slug_id and version are required")
	}
	if rec.ID == "" {
		rec.ID = ulid.Make().String()
	}
	if rec.InstalledAt.IsZero() {
		rec.InstalledAt = time.Now().UTC()
	}

	filter := bson.M{"slug_id": rec.SlugID, "version": rec.Version}
	update := bson.M{
		"$set": bson.M{
			"manifest":     rec.Manifest,
			"source_id":    rec.SourceID,
			"checksum":     rec.Checksum,
			"path":         rec.Path,
			"installed_at": rec.InstalledAt,
		},
		"$setOnInsert": bson.M{
			"_id":     rec.ID,
			"slug_id": rec.SlugID,
			"version": rec.Version,
		},
	}
	_, err := r.col.UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return skills.SkillRecord{}, fmt.Errorf("mongodb/skills: upsert: %w", err)
	}

	// Reload so callers receive whatever Mongo persisted (including any
	// server-set $setOnInsert fields).
	return r.GetRecord(ctx, rec.SlugID, rec.Version)
}

// GetRecord implements skills.RegistryStore.
func (r *SkillRegistry) GetRecord(ctx context.Context, slugID, version string) (skills.SkillRecord, error) {
	var rec skills.SkillRecord
	err := r.col.FindOne(ctx, bson.M{"slug_id": slugID, "version": version}).Decode(&rec)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return skills.SkillRecord{}, skills.ErrSkillNotFound
	}
	if err != nil {
		return skills.SkillRecord{}, fmt.Errorf("mongodb/skills: get %s@%s: %w", slugID, version, err)
	}
	return rec, nil
}

// ListVersions implements skills.RegistryStore. Sort ascending by version
// string is good enough for the registry layer — the resolver re-sorts via
// SemVer when picking the highest match.
func (r *SkillRegistry) ListVersions(ctx context.Context, slugID string) ([]skills.SkillRecord, error) {
	cur, err := r.col.Find(ctx,
		bson.M{"slug_id": slugID},
		options.Find().SetSort(bson.D{{Key: "version", Value: 1}}),
	)
	if err != nil {
		return nil, fmt.Errorf("mongodb/skills: list %s: %w", slugID, err)
	}
	defer cur.Close(ctx) //nolint:errcheck

	var out []skills.SkillRecord
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("mongodb/skills: decode list: %w", err)
	}
	return out, nil
}

// ListAll implements skills.RegistryStore.
func (r *SkillRegistry) ListAll(ctx context.Context) ([]skills.SkillRecord, error) {
	cur, err := r.col.Find(ctx,
		bson.M{},
		options.Find().SetSort(bson.D{{Key: "slug_id", Value: 1}, {Key: "version", Value: 1}}),
	)
	if err != nil {
		return nil, fmt.Errorf("mongodb/skills: list all: %w", err)
	}
	defer cur.Close(ctx) //nolint:errcheck

	var out []skills.SkillRecord
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("mongodb/skills: decode list all: %w", err)
	}
	return out, nil
}

// DeleteRecord implements skills.RegistryStore.
func (r *SkillRegistry) DeleteRecord(ctx context.Context, slugID, version string) error {
	_, err := r.col.DeleteOne(ctx, bson.M{"slug_id": slugID, "version": version})
	if err != nil {
		return fmt.Errorf("mongodb/skills: delete %s@%s: %w", slugID, version, err)
	}
	return nil
}

package mongodb

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// RawDB implements workflow.RawUpserter against a *mongo.Database.
type RawDB struct {
	db *mongo.Database
}

// NewRawDB wraps a *mongo.Database to satisfy workflow.RawUpserter.
func NewRawDB(db *mongo.Database) *RawDB {
	return &RawDB{db: db}
}

// UpsertRaw upserts a single document into the named collection.
// Returns (matchedCount, insertedCount, error).
func (r *RawDB) UpsertRaw(ctx context.Context, collection string, filter, update bson.M, upsert bool) (int64, int64, error) {
	res, err := r.db.Collection(collection).UpdateOne(
		ctx,
		filter,
		update,
		options.UpdateOne().SetUpsert(upsert),
	)
	if err != nil {
		return 0, 0, err
	}
	return res.MatchedCount, res.UpsertedCount, nil
}

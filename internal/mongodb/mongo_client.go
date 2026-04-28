package mongodb

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// MongoClient implements workflow.MongoClient against a *mongo.Database.
// One method per Mongo operation exposed by the mongo_request workflow node.
type MongoClient struct {
	db *mongo.Database
}

// NewMongoClient wraps a *mongo.Database to satisfy workflow.MongoClient.
func NewMongoClient(db *mongo.Database) *MongoClient {
	return &MongoClient{db: db}
}

func (c *MongoClient) Find(ctx context.Context, collection string, filter bson.M, opts *options.FindOptionsBuilder) (*mongo.Cursor, error) {
	return c.db.Collection(collection).Find(ctx, filter, opts)
}

func (c *MongoClient) FindOneAndUpdate(ctx context.Context, collection string, filter, update bson.M, opts *options.FindOneAndUpdateOptionsBuilder) (bson.M, error) {
	res := c.db.Collection(collection).FindOneAndUpdate(ctx, filter, update, opts)
	return decodeSingleResult(res)
}

func (c *MongoClient) FindOneAndReplace(ctx context.Context, collection string, filter, replacement bson.M, opts *options.FindOneAndReplaceOptionsBuilder) (bson.M, error) {
	res := c.db.Collection(collection).FindOneAndReplace(ctx, filter, replacement, opts)
	return decodeSingleResult(res)
}

func (c *MongoClient) InsertOne(ctx context.Context, collection string, doc bson.M) (any, error) {
	res, err := c.db.Collection(collection).InsertOne(ctx, doc)
	if err != nil {
		return nil, err
	}
	return res.InsertedID, nil
}

func (c *MongoClient) InsertMany(ctx context.Context, collection string, docs []bson.M, opts *options.InsertManyOptionsBuilder) ([]any, error) {
	anyDocs := make([]any, len(docs))
	for i, d := range docs {
		anyDocs[i] = d
	}
	res, err := c.db.Collection(collection).InsertMany(ctx, anyDocs, opts)
	if err != nil {
		return nil, err
	}
	return res.InsertedIDs, nil
}

func (c *MongoClient) UpdateMany(ctx context.Context, collection string, filter, update bson.M, opts *options.UpdateManyOptionsBuilder) (matched, modified, upserted int64, err error) {
	res, err := c.db.Collection(collection).UpdateMany(ctx, filter, update, opts)
	if err != nil {
		return 0, 0, 0, err
	}
	return res.MatchedCount, res.ModifiedCount, res.UpsertedCount, nil
}

func (c *MongoClient) DeleteOne(ctx context.Context, collection string, filter bson.M) (int64, error) {
	res, err := c.db.Collection(collection).DeleteOne(ctx, filter)
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

func (c *MongoClient) DeleteMany(ctx context.Context, collection string, filter bson.M) (int64, error) {
	res, err := c.db.Collection(collection).DeleteMany(ctx, filter)
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

func (c *MongoClient) Aggregate(ctx context.Context, collection string, pipeline []bson.M, opts *options.AggregateOptionsBuilder) (*mongo.Cursor, error) {
	return c.db.Collection(collection).Aggregate(ctx, pipeline, opts)
}

func (c *MongoClient) CountDocuments(ctx context.Context, collection string, filter bson.M, opts *options.CountOptionsBuilder) (int64, error) {
	return c.db.Collection(collection).CountDocuments(ctx, filter, opts)
}

func (c *MongoClient) Distinct(ctx context.Context, collection, field string, filter bson.M) ([]any, error) {
	res := c.db.Collection(collection).Distinct(ctx, field, filter)
	if err := res.Err(); err != nil {
		return nil, err
	}
	var values []any
	if err := res.Decode(&values); err != nil {
		return nil, err
	}
	return values, nil
}

// decodeSingleResult turns a *mongo.SingleResult into a bson.M, returning
// (nil, nil) when no document was matched (the find-and-* ops on a missed
// filter without upsert).
func decodeSingleResult(res *mongo.SingleResult) (bson.M, error) {
	if err := res.Err(); err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	var out bson.M
	if err := res.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

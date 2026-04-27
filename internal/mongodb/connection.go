package mongodb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// ConnectionRepository persists workflow connections in MongoDB.
type ConnectionRepository struct {
	col    *mongo.Collection
	encKey []byte // nil → no encryption
}

func NewConnectionRepository(ctx context.Context, db *mongo.Database, encKey []byte) (*ConnectionRepository, error) {
	col := db.Collection("workflow_connections")
	_, err := col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "name", Value: 1}}},
		{Keys: bson.D{{Key: "type", Value: 1}}},
	})
	if err != nil {
		return nil, err
	}
	return &ConnectionRepository{col: col, encKey: encKey}, nil
}

// List returns all stored connections.
func (r *ConnectionRepository) List(ctx context.Context) ([]workflow.Connection, error) {
	cur, err := r.col.Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx) //nolint:errcheck
	var results []workflow.Connection
	if err := cur.All(ctx, &results); err != nil {
		return nil, err
	}
	if r.encKey != nil {
		for i, c := range results {
			dec, err := workflow.DecryptConfigMap(c.Config, r.encKey)
			if err != nil {
				return nil, fmt.Errorf("decrypt connection %s: %w", c.ID, err)
			}
			results[i].Config = dec
		}
	}
	return results, nil
}

// GetByID returns the connection with the given ID.
func (r *ConnectionRepository) GetByID(ctx context.Context, id string) (workflow.Connection, error) {
	var conn workflow.Connection
	err := r.col.FindOne(ctx, bson.M{"_id": id}).Decode(&conn)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return workflow.Connection{}, mongo.ErrNoDocuments
	}
	if err != nil {
		return workflow.Connection{}, err
	}
	if r.encKey != nil {
		dec, err := workflow.DecryptConfigMap(conn.Config, r.encKey)
		if err != nil {
			return workflow.Connection{}, fmt.Errorf("decrypt connection %s: %w", conn.ID, err)
		}
		conn.Config = dec
	}
	return conn, nil
}

// Upsert saves or updates a connection by its ID.
func (r *ConnectionRepository) Upsert(ctx context.Context, conn workflow.Connection) (workflow.Connection, error) {
	now := time.Now().UTC()
	if conn.CreatedAt.IsZero() {
		conn.CreatedAt = now
	}
	conn.UpdatedAt = now

	cfgToStore := conn.Config
	if r.encKey != nil {
		enc, err := workflow.EncryptConfigMap(conn.Config, r.encKey)
		if err != nil {
			return workflow.Connection{}, fmt.Errorf("encrypt connection %s: %w", conn.ID, err)
		}
		cfgToStore = enc
	}

	_, err := r.col.UpdateOne(
		ctx,
		bson.M{"_id": conn.ID},
		bson.M{
			"$set": bson.M{
				"name":       conn.Name,
				"type":       conn.Type,
				"config":     cfgToStore,
				"updated_at": conn.UpdatedAt,
			},
			"$setOnInsert": bson.M{
				"created_at": conn.CreatedAt,
			},
		},
		options.UpdateOne().SetUpsert(true),
	)
	return conn, err
}

// Delete removes the connection with the given ID.
func (r *ConnectionRepository) Delete(ctx context.Context, id string) error {
	_, err := r.col.DeleteOne(ctx, bson.M{"_id": id})
	return err
}

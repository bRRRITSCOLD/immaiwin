package mongodb

import (
	"context"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/options"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	mongoOptions "go.mongodb.org/mongo-driver/v2/mongo/options"
)

type OptionsWatchlistRepository struct {
	col *mongo.Collection
}

func NewOptionsWatchlistRepository(db *mongo.Database) *OptionsWatchlistRepository {
	return &OptionsWatchlistRepository{col: db.Collection("options_watchlist")}
}

// List returns all watched underlying symbols.
func (r *OptionsWatchlistRepository) List(ctx context.Context) ([]options.WatchlistItem, error) {
	cursor, err := r.col.Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	var items []options.WatchlistItem
	if err := cursor.All(ctx, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// Sync replaces the watchlist with the given symbols. Items not in the list are deleted.
func (r *OptionsWatchlistRepository) Sync(ctx context.Context, symbols []string) error {
	var deleteFilter bson.M
	if len(symbols) == 0 {
		deleteFilter = bson.M{}
	} else {
		deleteFilter = bson.M{"symbol": bson.M{"$nin": symbols}}
	}
	if _, err := r.col.DeleteMany(ctx, deleteFilter); err != nil {
		return err
	}

	now := time.Now().UTC()
	for _, sym := range symbols {
		_, err := r.col.UpdateOne(
			ctx,
			bson.M{"symbol": sym},
			bson.M{
				"$setOnInsert": bson.M{
					"symbol":   sym,
					"added_at": now,
				},
			},
			mongoOptions.UpdateOne().SetUpsert(true),
		)
		if err != nil {
			return err
		}
	}
	return nil
}

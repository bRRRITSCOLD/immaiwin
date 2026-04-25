package mongodb

import (
	"context"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/futures"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	mongoOptions "go.mongodb.org/mongo-driver/v2/mongo/options"
)

type FuturesWatchlistRepository struct {
	col *mongo.Collection
}

func NewFuturesWatchlistRepository(db *mongo.Database) *FuturesWatchlistRepository {
	return &FuturesWatchlistRepository{col: db.Collection("futures_watchlist")}
}

// List returns all watched futures root symbols.
func (r *FuturesWatchlistRepository) List(ctx context.Context) ([]futures.WatchlistItem, error) {
	cursor, err := r.col.Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	var items []futures.WatchlistItem
	if err := cursor.All(ctx, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// Sync replaces the watchlist with the given symbols. Items not in the list are deleted.
func (r *FuturesWatchlistRepository) Sync(ctx context.Context, symbols []string) error {
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

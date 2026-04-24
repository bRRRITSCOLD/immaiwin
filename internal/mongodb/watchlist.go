package mongodb

import (
	"context"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/watchlist"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type WatchlistRepository struct {
	col *mongo.Collection
}

func NewWatchlistRepository(db *mongo.Database) *WatchlistRepository {
	return &WatchlistRepository{col: db.Collection("watchlist")}
}

// List returns all watchlisted items.
func (r *WatchlistRepository) List(ctx context.Context) ([]watchlist.WatchlistItem, error) {
	cursor, err := r.col.Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	var items []watchlist.WatchlistItem
	if err := cursor.All(ctx, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// Sync replaces the watchlist with the given items.
// Items not in the list are deleted; new items are inserted; existing items have
// question and slug refreshed.
func (r *WatchlistRepository) Sync(ctx context.Context, items []watchlist.WatchlistItem) error {
	incomingIDs := make([]string, len(items))
	for i, item := range items {
		incomingIDs[i] = item.MarketID
	}

	var deleteFilter bson.M
	if len(incomingIDs) == 0 {
		deleteFilter = bson.M{}
	} else {
		deleteFilter = bson.M{"market_id": bson.M{"$nin": incomingIDs}}
	}
	if _, err := r.col.DeleteMany(ctx, deleteFilter); err != nil {
		return err
	}

	now := time.Now()
	for _, item := range items {
		_, err := r.col.UpdateOne(
			ctx,
			bson.M{"market_id": item.MarketID},
			bson.M{
				"$set": bson.M{
					"question": item.Question,
					"slug":     item.Slug,
				},
				"$setOnInsert": bson.M{
					"market_id": item.MarketID,
					"added_at":  now,
				},
			},
			options.UpdateOne().SetUpsert(true),
		)
		if err != nil {
			return err
		}
	}
	return nil
}

package mongodb

import (
	"context"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/trade"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type TradeRepository struct {
	col *mongo.Collection
}

func NewTradeRepository(db *mongo.Database) *TradeRepository {
	return &TradeRepository{col: db.Collection("trades")}
}

func (r *TradeRepository) InsertOne(ctx context.Context, t *trade.Trade) (*mongo.InsertOneResult, error) {
	return r.col.InsertOne(ctx, t)
}

func (r *TradeRepository) InsertMany(ctx context.Context, t *trade.Trade) (*mongo.InsertManyResult, error) {
	return r.col.InsertMany(ctx, t)
}

// ListMissingQuestion returns trades where market_question is empty.
func (r *TradeRepository) ListMissingQuestion(ctx context.Context) ([]trade.Trade, error) {
	filter := bson.D{{Key: "market_question", Value: bson.D{{Key: "$in", Value: bson.A{"", nil}}}}}
	cur, err := r.col.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var results []trade.Trade
	if err := cur.All(ctx, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// UpdateMarketQuestion sets market_question for all trades with the given asset_id.
func (r *TradeRepository) UpdateMarketQuestion(ctx context.Context, assetID, question string) error {
	filter := bson.D{{Key: "asset_id", Value: assetID}}
	update := bson.D{{Key: "$set", Value: bson.D{{Key: "market_question", Value: question}}}}
	_, err := r.col.UpdateMany(ctx, filter, update)
	return err
}

// UpdateTradeMetadata sets market_question and token_outcome for all trades with the given asset_id.
func (r *TradeRepository) UpdateTradeMetadata(ctx context.Context, assetID, question, outcome string) error {
	filter := bson.D{{Key: "asset_id", Value: assetID}}
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "market_question", Value: question},
		{Key: "token_outcome", Value: outcome},
	}}}
	_, err := r.col.UpdateMany(ctx, filter, update)
	return err
}

// ListMissingOutcome returns trades where token_outcome is empty.
func (r *TradeRepository) ListMissingOutcome(ctx context.Context) ([]trade.Trade, error) {
	filter := bson.D{{Key: "token_outcome", Value: bson.D{{Key: "$in", Value: bson.A{"", nil}}}}}
	cur, err := r.col.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var results []trade.Trade
	if err := cur.All(ctx, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// List returns the most recent trades sorted by detected_at descending.
func (r *TradeRepository) List(ctx context.Context, limit int) ([]trade.Trade, error) {
	opts := options.Find().
		SetSort(bson.D{{Key: "detected_at", Value: -1}}).
		SetLimit(int64(limit))
	cur, err := r.col.Find(ctx, bson.D{}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var results []trade.Trade
	if err := cur.All(ctx, &results); err != nil {
		return nil, err
	}
	return results, nil
}

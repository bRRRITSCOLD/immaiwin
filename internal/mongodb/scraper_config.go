package mongodb

import (
	"context"
	"errors"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/news"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// ScraperConfigRepository persists news scraper configs in MongoDB.
type ScraperConfigRepository struct {
	col *mongo.Collection
}

func NewScraperConfigRepository(ctx context.Context, db *mongo.Database) (*ScraperConfigRepository, error) {
	col := db.Collection("news_scraper_configs")
	_, err := col.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "source", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		return nil, err
	}
	return &ScraperConfigRepository{col: col}, nil
}

// List returns all stored scraper configs.
func (r *ScraperConfigRepository) List(ctx context.Context) ([]news.ScraperConfig, error) {
	cur, err := r.col.Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx) //nolint:errcheck
	var results []news.ScraperConfig
	if err := cur.All(ctx, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// GetOrDefault returns the stored config for source, or a default config
// (feedURL, no script) if none has been saved.
func (r *ScraperConfigRepository) GetOrDefault(ctx context.Context, source, defaultFeedURL string) (news.ScraperConfig, error) {
	var cfg news.ScraperConfig
	err := r.col.FindOne(ctx, bson.M{"source": source}).Decode(&cfg)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return news.ScraperConfig{Source: source, FeedURL: defaultFeedURL}, nil
	}
	if err != nil {
		return news.ScraperConfig{}, err
	}
	return cfg, nil
}

// Upsert saves (or updates) a scraper config identified by Source.
func (r *ScraperConfigRepository) Upsert(ctx context.Context, cfg news.ScraperConfig) error {
	cfg.UpdatedAt = time.Now().UTC()
	_, err := r.col.UpdateOne(
		ctx,
		bson.M{"source": cfg.Source},
		bson.M{"$set": cfg},
		options.UpdateOne().SetUpsert(true),
	)
	return err
}

// ClearScript removes the custom script for source, reverting to the default parser.
func (r *ScraperConfigRepository) ClearScript(ctx context.Context, source string) error {
	_, err := r.col.UpdateOne(
		ctx,
		bson.M{"source": source},
		bson.M{"$unset": bson.M{"script": ""}, "$set": bson.M{"updated_at": time.Now().UTC()}},
	)
	return err
}

// Delete removes a scraper config entirely.
func (r *ScraperConfigRepository) Delete(ctx context.Context, source string) error {
	_, err := r.col.DeleteOne(ctx, bson.M{"source": source})
	return err
}

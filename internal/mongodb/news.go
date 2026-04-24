package mongodb

import (
	"context"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/news"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type NewsRepository struct {
	col *mongo.Collection
}

func NewNewsRepository(ctx context.Context, db *mongo.Database) (*NewsRepository, error) {
	col := db.Collection("news_articles")
	for _, model := range []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "url", Value: 1}},
			Options: options.Index().SetUnique(true),
		},
		{
			Keys: bson.D{{Key: "scraped_at", Value: -1}},
		},
	} {
		if _, err := col.Indexes().CreateOne(ctx, model); err != nil {
			return nil, err
		}
	}
	return &NewsRepository{col: col}, nil
}

// List returns articles with scraped_at >= since, sorted newest-first, capped at limit.
func (r *NewsRepository) List(ctx context.Context, since time.Time, limit int) ([]news.Article, error) {
	opts := options.Find().
		SetSort(bson.D{{Key: "scraped_at", Value: -1}}).
		SetLimit(int64(limit))
	cur, err := r.col.Find(ctx, bson.M{"scraped_at": bson.M{"$gte": since}}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var results []news.Article
	if err := cur.All(ctx, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// Upsert inserts the article if its URL has not been seen before.
// Returns true if the article was newly inserted (dedup: false = already existed).
func (r *NewsRepository) Upsert(ctx context.Context, a *news.Article) (bool, error) {
	res, err := r.col.UpdateOne(
		ctx,
		bson.M{"url": a.URL},
		bson.M{"$setOnInsert": bson.M{
			"platform":   a.Platform,
			"url":        a.URL,
			"title":      a.Title,
			"scraped_at": a.ScrapedAt,
			"metadata":   a.Metadata,
		}},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return false, err
	}
	return res.UpsertedCount > 0, nil
}

// UpdateContent sets the body text and raw HTML for an article identified by URL.
func (r *NewsRepository) UpdateContent(ctx context.Context, url, body, rawHTML string) error {
	_, err := r.col.UpdateOne(
		ctx,
		bson.M{"url": url},
		bson.M{"$set": bson.M{"body": body, "raw_html": rawHTML}},
	)
	return err
}

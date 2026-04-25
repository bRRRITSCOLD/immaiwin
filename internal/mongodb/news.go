package mongodb

import (
	"context"
	"log/slog"
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
	defer func() {
		if err := cur.Close(ctx); err != nil {
			slog.Error("mongodb-news: close cursor", "err", err)
		}
	}()
	var results []news.Article
	if err := cur.All(ctx, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// Upsert inserts the article if its URL has not been seen before.
// Returns true if the article was newly inserted (dedup: false = already existed).
// Non-empty optional fields (Body, RawHTML, RawXML, Metadata) are included in $setOnInsert when present.
func (r *NewsRepository) Upsert(ctx context.Context, a *news.Article) (bool, error) {
	doc := bson.M{
		"platform":   a.Platform,
		"url":        a.URL,
		"title":      a.Title,
		"scraped_at": a.ScrapedAt,
	}
	if a.Metadata != nil {
		doc["metadata"] = a.Metadata
	}
	if a.Body != "" {
		doc["body"] = a.Body
	}
	if a.RawHTML != "" {
		doc["raw_html"] = a.RawHTML
	}
	if a.RawXML != "" {
		doc["raw_xml"] = a.RawXML
	}

	res, err := r.col.UpdateOne(
		ctx,
		bson.M{"url": a.URL},
		bson.M{"$setOnInsert": doc},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return false, err
	}
	return res.UpsertedCount > 0, nil
}

// ListEmptyBody returns articles for a platform where body is absent or empty.
func (r *NewsRepository) ListEmptyBody(ctx context.Context, platform string) ([]news.Article, error) {
	filter := bson.M{
		"platform": platform,
		"$or": bson.A{
			bson.M{"body": bson.M{"$exists": false}},
			bson.M{"body": ""},
		},
	}
	cur, err := r.col.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := cur.Close(ctx); err != nil {
			slog.Error("mongodb-news: close cursor", "err", err)
		}
	}()
	var results []news.Article
	if err := cur.All(ctx, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// GetByURL returns the article with the given URL.
func (r *NewsRepository) GetByURL(ctx context.Context, url string) (*news.Article, error) {
	var a news.Article
	if err := r.col.FindOne(ctx, bson.M{"url": url}).Decode(&a); err != nil {
		return nil, err
	}
	return &a, nil
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

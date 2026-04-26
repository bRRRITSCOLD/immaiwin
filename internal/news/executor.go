package news

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// ArticleStore upserts scraped articles and reports whether each was new.
type ArticleStore interface {
	Upsert(ctx context.Context, a *Article) (bool, error)
}

// Publisher broadcasts serialised payloads to a named channel.
type Publisher interface {
	Publish(ctx context.Context, channel string, payload []byte) error
}

// NewsChannel is the Redis pub/sub channel used to broadcast new articles.
const NewsChannel = "immaiwin:news:articles"

var executorHTTPClient = &http.Client{Timeout: 30 * time.Second}

// Executor runs a single scraper config end-to-end: fetch → parse → upsert → publish.
type Executor struct {
	Store      ArticleStore
	Pub        Publisher
	PubChannel string // defaults to NewsChannel if empty
}

// Execute scrapes the source described by cfg and returns the count of new articles.
func (e *Executor) Execute(ctx context.Context, cfg ScraperConfig) (int, error) {
	ch := e.PubChannel
	if ch == "" {
		ch = NewsChannel
	}

	parser := NewParser(cfg, &GenericRSSParser{})

	res, err := executorHTTPGet(cfg.FeedURL)
	if err != nil {
		return 0, fmt.Errorf("fetch %s: %w", cfg.FeedURL, err)
	}
	body, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", cfg.FeedURL, err)
	}

	slog.Info("executor: fetched feed", "source", cfg.Source, "bytes", len(body))

	articles, err := parser.Parse(ctx, ParseInput{
		Source:  cfg.Source,
		FeedURL: cfg.FeedURL,
		Raw:     string(body),
	})
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", cfg.Source, err)
	}

	newCount := 0
	for _, a := range articles {
		scrapedAt := a.ScrapedAt
		if scrapedAt.IsZero() {
			scrapedAt = time.Now().UTC()
		}
		article := &Article{
			Platform:  cfg.Source,
			URL:       a.URL,
			Title:     a.Title,
			Body:      a.Body,
			RawHTML:   a.RawHTML,
			RawXML:    a.RawXML,
			ScrapedAt: scrapedAt,
			Metadata:  a.Metadata,
		}

		inserted, err := e.Store.Upsert(ctx, article)
		if err != nil {
			slog.Error("executor: upsert", "source", cfg.Source, "url", a.URL, "err", err)
			continue
		}
		if inserted {
			newCount++
			slog.Info("executor: new article", "source", cfg.Source, "title", a.Title)
			if payload, err := json.Marshal(article); err == nil {
				if err := e.Pub.Publish(ctx, ch, payload); err != nil {
					slog.Warn("executor: publish", "source", cfg.Source, "url", a.URL, "err", err)
				}
			}
		}
	}

	slog.Info("executor: done", "source", cfg.Source, "new", newCount, "total", len(articles))
	return newCount, nil
}

func executorHTTPGet(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; immaiwin-scraper/1.0)")
	return executorHTTPClient.Do(req)
}

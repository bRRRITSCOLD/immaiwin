package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/news"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
)

const (
	bloombergFeedURL        = "https://feeds.bloomberg.com/markets/news.rss"
	bloombergPlatform       = "bloomberg"
	bloombergScrapeInterval = 10 * time.Minute
)

var BloombergRSSWorker = &bloombergRSSWorker{}

type bloombergRSSWorker struct{}

func (w *bloombergRSSWorker) Name() string { return "bloomberg-rss" }

func (w *bloombergRSSWorker) Run(ctx context.Context) error {
	cfg, err := config.Load(config.WithDotEnv(".env"))
	if err != nil {
		return fmt.Errorf("bloomberg-rss: load config: %w", err)
	}

	rc := rediss.New(cfg.Redis)
	defer func() {
		if err := rc.Close(); err != nil {
			slog.Error("bloomberg-rss: close redis client", "err", err)
		}
	}()

	mc, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		return fmt.Errorf("bloomberg-rss: connect mongodb: %w", err)
	}
	defer func() {
		if err := mc.Disconnect(ctx); err != nil {
			slog.Error("bloomberg-rss: disconnect mongodb", "err", err)
		}
	}()

	repo, err := mongodb.NewNewsRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("bloomberg-rss: create news repo: %w", err)
	}

	scraperRepo, err := mongodb.NewScraperConfigRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("bloomberg-rss: create scraper config repo: %w", err)
	}

	scrape := func() {
		if err := scrapeBloombergRSS(ctx, repo, scraperRepo, rc); err != nil {
			slog.Error("bloomberg-rss: scrape failed", "err", err)
		}
	}

	scrape()

	ticker := time.NewTicker(bloombergScrapeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			scrape()
		}
	}
}

func scrapeBloombergRSS(ctx context.Context, repo *mongodb.NewsRepository, scraperRepo *mongodb.ScraperConfigRepository, rc *rediss.Client) error {
	scrapercfg, err := scraperRepo.GetOrDefault(ctx, bloombergPlatform, bloombergFeedURL)
	if err != nil {
		return fmt.Errorf("load scraper config: %w", err)
	}

	parser := news.NewParser(scrapercfg, &news.DefaultBloombergParser{})

	res, err := httpGet(scrapercfg.FeedURL)
	if err != nil {
		return fmt.Errorf("fetch feed: %w", err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			slog.Error("bloomberg-rss: close feed body", "err", err)
		}
	}()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("read feed body: %w", err)
	}

	articles, err := parser.Parse(ctx, news.ParseInput{
		Source:  bloombergPlatform,
		FeedURL: scrapercfg.FeedURL,
		Raw:     string(body),
	})
	if err != nil {
		return fmt.Errorf("parse feed: %w", err)
	}

	for _, a := range articles {
		scrapedAt := a.ScrapedAt
		if scrapedAt.IsZero() {
			scrapedAt = time.Now().UTC()
		}
		article := &news.Article{
			Platform:  bloombergPlatform,
			URL:       a.URL,
			Title:     a.Title,
			Body:      a.Body,
			RawXML:    a.RawXML,
			ScrapedAt: scrapedAt,
			Metadata:  a.Metadata,
		}

		inserted, err := repo.Upsert(ctx, article)
		if err != nil {
			slog.Error("bloomberg-rss: upsert article", "url", a.URL, "err", err)
			continue
		}
		if inserted {
			slog.Info("bloomberg-rss: new article", "title", a.Title, "url", a.URL)
			if payload, err := json.Marshal(article); err == nil {
				if err := rc.Publish(ctx, rediss.NewsChannel, payload); err != nil {
					slog.Warn("bloomberg-rss: publish article", "url", a.URL, "err", err)
				}
			}
		}
	}

	return nil
}

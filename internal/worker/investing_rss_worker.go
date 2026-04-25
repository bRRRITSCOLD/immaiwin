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
	investingFeedURL        = "https://www.investing.com/rss/news_301.rss"
	investingPlatform       = "investing"
	investingScrapeInterval = 10 * time.Minute
)

var InvestingRSSWorker = &investingRSSWorker{}

type investingRSSWorker struct{}

func (w *investingRSSWorker) Name() string { return "investing-rss" }

func (w *investingRSSWorker) Run(ctx context.Context) error {
	cfg, err := config.Load(config.WithDotEnv(".env"))
	if err != nil {
		return fmt.Errorf("investing-rss: load config: %w", err)
	}

	rc := rediss.New(cfg.Redis)
	defer func() {
		if err := rc.Close(); err != nil {
			slog.Error("investing-rss: close redis client", "err", err)
		}
	}()

	mc, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		return fmt.Errorf("investing-rss: connect mongodb: %w", err)
	}
	defer func() {
		if err := mc.Disconnect(ctx); err != nil {
			slog.Error("investing-rss: disconnect mongodb", "err", err)
		}
	}()

	repo, err := mongodb.NewNewsRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("investing-rss: create news repo: %w", err)
	}

	scraperRepo, err := mongodb.NewScraperConfigRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("investing-rss: create scraper config repo: %w", err)
	}

	scrape := func() {
		if err := scrapeInvestingRSS(ctx, repo, scraperRepo, rc); err != nil {
			slog.Error("investing-rss: scrape failed", "err", err)
		}
	}

	scrape()

	ticker := time.NewTicker(investingScrapeInterval)
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

func scrapeInvestingRSS(ctx context.Context, repo *mongodb.NewsRepository, scraperRepo *mongodb.ScraperConfigRepository, rc *rediss.Client) error {
	scrapercfg, err := scraperRepo.GetOrDefault(ctx, investingPlatform, investingFeedURL)
	if err != nil {
		return fmt.Errorf("load scraper config: %w", err)
	}

	parser := news.NewParser(scrapercfg, &news.DefaultInvestingParser{})

	res, err := httpGet(scrapercfg.FeedURL)
	if err != nil {
		return fmt.Errorf("fetch feed: %w", err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			slog.Error("investing-rss: close feed body", "err", err)
		}
	}()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("read feed body: %w", err)
	}

	articles, err := parser.Parse(ctx, news.ParseInput{
		Source:  investingPlatform,
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
			Platform:  investingPlatform,
			URL:       a.URL,
			Title:     a.Title,
			RawXML:    a.RawXML,
			ScrapedAt: scrapedAt,
			Metadata:  a.Metadata,
		}

		inserted, err := repo.Upsert(ctx, article)
		if err != nil {
			slog.Error("investing-rss: upsert article", "url", a.URL, "err", err)
			continue
		}
		if inserted {
			slog.Info("investing-rss: new article", "title", a.Title, "url", a.URL)
			if payload, err := json.Marshal(article); err == nil {
				if err := rc.Publish(ctx, rediss.NewsChannel, payload); err != nil {
					slog.Warn("investing-rss: publish article", "url", a.URL, "err", err)
				}
			}
		}
	}

	return nil
}

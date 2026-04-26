package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/news"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
)

const newsScrapeInterval = 10 * time.Minute

var NewsScraperWorker = &newsScraperWorker{}

type newsScraperWorker struct{}

func (w *newsScraperWorker) Name() string { return "news-scraper" }

func (w *newsScraperWorker) Run(ctx context.Context) error {
	cfg, err := config.Load(config.WithDotEnv(".env"))
	if err != nil {
		return fmt.Errorf("news-scraper: load config: %w", err)
	}

	rc := rediss.New(cfg.Redis)
	defer func() {
		if err := rc.Close(); err != nil {
			slog.Error("news-scraper: close redis", "err", err)
		}
	}()

	mc, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		return fmt.Errorf("news-scraper: connect mongodb: %w", err)
	}
	defer func() {
		if err := mc.Disconnect(ctx); err != nil {
			slog.Error("news-scraper: disconnect mongodb", "err", err)
		}
	}()

	repo, err := mongodb.NewNewsRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("news-scraper: create news repo: %w", err)
	}

	scraperRepo, err := mongodb.NewScraperConfigRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("news-scraper: create scraper config repo: %w", err)
	}

	executor := &news.Executor{Store: repo, Pub: rc}

	scrape := func() {
		slog.Info("news-scraper: starting scrape cycle")
		start := time.Now()
		if err := scrapeAllSources(ctx, scraperRepo, executor); err != nil {
			slog.Error("news-scraper: scrape cycle failed", "err", err)
		} else {
			slog.Info("news-scraper: scrape cycle complete", "elapsed", time.Since(start).Round(time.Millisecond))
		}
	}

	scrape()

	ticker := time.NewTicker(newsScrapeInterval)
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

func scrapeAllSources(ctx context.Context, scraperRepo *mongodb.ScraperConfigRepository, executor *news.Executor) error {
	configs, err := scraperRepo.List(ctx)
	if err != nil {
		return fmt.Errorf("list scraper configs: %w", err)
	}

	slog.Info("news-scraper: loaded configs", "count", len(configs))

	for _, cfg := range configs {
		slog.Info("news-scraper: scraping source", "source", cfg.Source, "feed_url", cfg.FeedURL)
		start := time.Now()
		if _, err := executor.Execute(ctx, cfg); err != nil {
			slog.Error("news-scraper: source failed", "source", cfg.Source, "elapsed", time.Since(start).Round(time.Millisecond), "err", err)
		} else {
			slog.Info("news-scraper: source done", "source", cfg.Source, "elapsed", time.Since(start).Round(time.Millisecond))
		}
	}
	return nil
}

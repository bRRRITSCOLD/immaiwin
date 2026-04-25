package worker

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/news"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
)

const (
	investingFeedURL      = "https://www.investing.com/rss/news_301.rss"
	investingPlatform     = "investing"
	investingScrapeInterval = 10 * time.Minute

	// Investing.com pubDate format: "2006-01-02 15:04:05"
	investingPubDateFormat = "2006-01-02 15:04:05"
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

	scrape := func() {
		if err := scrapeInvestingRSS(ctx, repo, rc); err != nil {
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

type investingItem struct {
	XMLName   xml.Name `xml:"item"`
	Title     string   `xml:"title"`
	Link      string   `xml:"link"`
	PubDate   string   `xml:"pubDate"`
	Author    string   `xml:"author"`
	Enclosure struct {
		URL string `xml:"url,attr"`
	} `xml:"enclosure"`
}

type investingFeed struct {
	Items []investingItem `xml:"channel>item"`
}

func scrapeInvestingRSS(ctx context.Context, repo *mongodb.NewsRepository, rc *rediss.Client) error {
	res, err := httpGet(investingFeedURL)
	if err != nil {
		return fmt.Errorf("fetch feed: %w", err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			slog.Error("investing-rss: close feed body", "err", err)
		}
	}()

	var feed investingFeed
	if err := xml.NewDecoder(res.Body).Decode(&feed); err != nil {
		return fmt.Errorf("parse feed: %w", err)
	}

	for _, item := range feed.Items {
		url := strings.TrimSpace(item.Link)
		if url == "" || !strings.HasPrefix(url, "http") {
			continue
		}

		title := strings.TrimSpace(item.Title)
		if title == "" {
			continue
		}

		scrapedAt := time.Now().UTC()
		if t, err := time.Parse(investingPubDateFormat, strings.TrimSpace(item.PubDate)); err == nil {
			scrapedAt = t.UTC()
		}

		var rawXML string
		if b, err := xml.MarshalIndent(item, "", "  "); err == nil {
			rawXML = string(b)
		}

		meta := map[string]any{}
		if item.Author != "" {
			meta["author"] = strings.TrimSpace(item.Author)
		}
		if item.Enclosure.URL != "" {
			meta["image_url"] = item.Enclosure.URL
		}

		article := &news.Article{
			Platform:  investingPlatform,
			URL:       url,
			Title:     title,
			RawXML:    rawXML,
			ScrapedAt: scrapedAt,
			Metadata:  meta,
		}

		inserted, err := repo.Upsert(ctx, article)
		if err != nil {
			slog.Error("investing-rss: upsert article", "url", url, "err", err)
			continue
		}
		if inserted {
			slog.Info("investing-rss: new article", "title", title, "url", url)
			if payload, err := json.Marshal(article); err == nil {
				if err := rc.Publish(ctx, rediss.NewsChannel, payload); err != nil {
					slog.Warn("investing-rss: publish article", "url", url, "err", err)
				}
			}
		}
	}

	return nil
}

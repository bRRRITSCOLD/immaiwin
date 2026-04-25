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
	bloombergFeedURL      = "https://feeds.bloomberg.com/markets/news.rss"
	bloombergPlatform     = "bloomberg"
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

	scrape := func() {
		if err := scrapeBloombergRSS(ctx, repo, rc); err != nil {
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

// rssItem maps relevant fields from a Bloomberg RSS <item>.
type rssItem struct {
	XMLName      xml.Name `xml:"item"`
	Title        string   `xml:"title"`
	Link         string   `xml:"link"`
	GUID         string   `xml:"guid"`
	Description  string   `xml:"description"`
	PubDate      string   `xml:"pubDate"`
	Creator      string   `xml:"http://purl.org/dc/elements/1.1/ creator"`
	Categories   []string `xml:"category"`
	MediaContent struct {
		URL string `xml:"url,attr"`
	} `xml:"http://search.yahoo.com/mrss/ content"`
}

type rssFeed struct {
	Items []rssItem `xml:"channel>item"`
}

func scrapeBloombergRSS(ctx context.Context, repo *mongodb.NewsRepository, rc *rediss.Client) error {
	res, err := httpGet(bloombergFeedURL)
	if err != nil {
		return fmt.Errorf("fetch feed: %w", err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			slog.Error("bloomberg-rss: close feed body", "err", err)
		}
	}()

	var feed rssFeed
	if err := xml.NewDecoder(res.Body).Decode(&feed); err != nil {
		return fmt.Errorf("parse feed: %w", err)
	}

	for _, item := range feed.Items {
		url := strings.TrimSpace(item.Link)
		if url == "" {
			url = strings.TrimSpace(item.GUID)
		}
		if url == "" || !strings.HasPrefix(url, "http") {
			continue
		}

		title := strings.TrimSpace(item.Title)
		if title == "" {
			continue
		}

		scrapedAt := time.Now().UTC()
		if t, err := time.Parse(time.RFC1123, strings.TrimSpace(item.PubDate)); err == nil {
			scrapedAt = t.UTC()
		} else if t, err := time.Parse(time.RFC1123Z, strings.TrimSpace(item.PubDate)); err == nil {
			scrapedAt = t.UTC()
		}

		body := strings.TrimSpace(item.Description)

		var rawXML string
		if b, err := xml.MarshalIndent(item, "", "  "); err == nil {
			rawXML = string(b)
		}

		meta := map[string]any{}
		if item.Creator != "" {
			meta["author"] = strings.TrimSpace(item.Creator)
		}
		if item.MediaContent.URL != "" {
			meta["image_url"] = item.MediaContent.URL
		}
		if len(item.Categories) > 0 {
			symbols := make([]string, 0, len(item.Categories))
			for _, c := range item.Categories {
				if s := strings.TrimSpace(c); s != "" {
					symbols = append(symbols, s)
				}
			}
			if len(symbols) > 0 {
				meta["symbols"] = symbols
			}
		}

		article := &news.Article{
			Platform:  bloombergPlatform,
			URL:       url,
			Title:     title,
			Body:      body,
			RawXML:    rawXML,
			ScrapedAt: scrapedAt,
			Metadata:  meta,
		}

		inserted, err := repo.Upsert(ctx, article)
		if err != nil {
			slog.Error("bloomberg-rss: upsert article", "url", url, "err", err)
			continue
		}
		if inserted {
			slog.Info("bloomberg-rss: new article", "title", title, "url", url)
			if payload, err := json.Marshal(article); err == nil {
				if err := rc.Publish(ctx, rediss.NewsChannel, payload); err != nil {
					slog.Warn("bloomberg-rss: publish article", "url", url, "err", err)
				}
			}
		}
	}

	return nil
}

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/news"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
)

const (
	aljazeeraPlatform       = "aljazeera"
	aljazeeraScrapeInterval = 5 * time.Minute
)

var AljazeeraScraperWorker = &aljazeeraScraperWorker{}

type aljazeeraScraperWorker struct{}

func (w *aljazeeraScraperWorker) Name() string { return "aljazeera-scraper" }

func (w *aljazeeraScraperWorker) Run(ctx context.Context) error {
	cfg, err := config.Load(config.WithDotEnv(".env"))
	if err != nil {
		return fmt.Errorf("aljazeera-scraper: load config: %w", err)
	}

	rc := rediss.New(cfg.Redis)
	defer func() {
		if err := rc.Close(); err != nil {
			slog.Error("aljazeera-scraper: close redis client", "err", err)
		}
	}()

	mc, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		return fmt.Errorf("aljazeera-scraper: connect mongodb: %w", err)
	}
	defer func() {
		if err := mc.Disconnect(ctx); err != nil {
			slog.Error("aljazeera-scraper: disconnect mongodb", "err", err)
		}
	}()

	repo, err := mongodb.NewNewsRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("aljazeera-scraper: create news repo: %w", err)
	}

	scraperRepo, err := mongodb.NewScraperConfigRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("aljazeera-scraper: create scraper config repo: %w", err)
	}

	scrape := func() {
		if err := scrapeAlJazeera(ctx, repo, scraperRepo, rc); err != nil {
			slog.Error("aljazeera-scraper: scrape failed", "err", err)
		}
	}

	scrape()

	ticker := time.NewTicker(aljazeeraScrapeInterval)
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

// scrapeAlJazeera fetches the Al Jazeera homepage, extracts article links and titles,
// upserts new articles to MongoDB, then fetches body text for newly discovered articles.
func scrapeAlJazeera(ctx context.Context, repo *mongodb.NewsRepository, scraperRepo *mongodb.ScraperConfigRepository, rc *rediss.Client) error {
	scrapercfg, err := scraperRepo.GetOrDefault(ctx, aljazeeraPlatform, news.AljazeeraBaseURL+"/")
	if err != nil {
		return fmt.Errorf("load scraper config: %w", err)
	}

	parser := news.NewParser(scrapercfg, &news.DefaultAljazeeraParser{})

	res, err := httpGet(scrapercfg.FeedURL)
	if err != nil {
		return fmt.Errorf("fetch homepage: %w", err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			slog.Error("aljazeera-scraper: close homepage body", "err", err)
		}
	}()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("read homepage body: %w", err)
	}

	articles, err := parser.Parse(ctx, news.ParseInput{
		Source:  aljazeeraPlatform,
		FeedURL: scrapercfg.FeedURL,
		Raw:     string(body),
	})
	if err != nil {
		return fmt.Errorf("parse homepage: %w", err)
	}

	var newURLs []string
	for _, a := range articles {
		scrapedAt := a.ScrapedAt
		if scrapedAt.IsZero() {
			scrapedAt = time.Now().UTC()
		}
		article := &news.Article{
			Platform:  aljazeeraPlatform,
			URL:       a.URL,
			Title:     a.Title,
			ScrapedAt: scrapedAt,
		}

		inserted, err := repo.Upsert(ctx, article)
		if err != nil {
			slog.Error("aljazeera-scraper: upsert article", "url", a.URL, "err", err)
			continue
		}
		if inserted {
			slog.Info("aljazeera-scraper: new article", "title", a.Title, "url", a.URL)
			newURLs = append(newURLs, a.URL)
		}
	}

	// Fetch body for newly inserted articles, then publish (body-first).
	for _, url := range newURLs {
		body, rawHTML, err := fetchArticleContent(url)
		if err != nil {
			slog.Warn("aljazeera-scraper: fetch content failed", "url", url, "err", err)
		}
		if body != "" || rawHTML != "" {
			if err := repo.UpdateContent(ctx, url, body, rawHTML); err != nil {
				slog.Error("aljazeera-scraper: update content", "url", url, "err", err)
			}
		}
		// Publish after body is saved so SSE clients receive complete article.
		article, err := repo.GetByURL(ctx, url)
		if err != nil {
			slog.Warn("aljazeera-scraper: fetch article for publish", "url", url, "err", err)
			continue
		}
		if payload, err := json.Marshal(article); err == nil {
			if err := rc.Publish(ctx, rediss.NewsChannel, payload); err != nil {
				slog.Warn("aljazeera-scraper: publish article", "url", url, "err", err)
			}
		}
	}

	return nil
}

// fetchArticleContent fetches an article page and returns:
//   - body: plain text extracted from all p and li elements within the matched container
//   - rawHTML: inner HTML of the matched container (for future backfilling)
func fetchArticleContent(url string) (body, rawHTML string, err error) {
	res, err := httpGet(url)
	if err != nil {
		return "", "", err
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			slog.Error("aljazeera-scraper: close article body", "url", url, "err", err)
		}
	}()

	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		return "", "", err
	}

	// Container selectors tried in priority order.
	containerSelectors := []string{
		"[data-testid='ArticleBodyParagraph']",
		"[data-testid='LiveBlogCard']",
		".wysiwyg--all-content",
		".article-p-wrapper",
		"article",
	}

	for _, sel := range containerSelectors {
		container := doc.Find(sel).First()
		if container.Length() == 0 {
			continue
		}

		var parts []string
		container.Find("p, li").Each(func(_ int, s *goquery.Selection) {
			if t := strings.TrimSpace(s.Text()); t != "" {
				parts = append(parts, t)
			}
		})
		if len(parts) == 0 {
			continue
		}

		html, _ := container.Html()
		return strings.Join(parts, "\n\n"), html, nil
	}

	return "", "", nil
}

func httpGet(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; immaiwin-scraper/1.0)")
	return http.DefaultClient.Do(req)
}

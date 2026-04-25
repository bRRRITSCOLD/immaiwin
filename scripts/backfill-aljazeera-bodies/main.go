//go:build ignore

// backfill-aljazeera-bodies re-fetches body content for Al Jazeera articles
// that have an empty body field, using the current extraction logic (p + li).
//
// Usage:
//
//	go run ./scripts/backfill-aljazeera-bodies
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(config.WithDotEnv(".env"))
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	mc, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		slog.Error("connect mongodb", "err", err)
		os.Exit(1)
	}
	defer mc.Disconnect(ctx) //nolint:errcheck

	repo, err := mongodb.NewNewsRepository(ctx, mc.DB())
	if err != nil {
		slog.Error("init news repo", "err", err)
		os.Exit(1)
	}

	articles, err := repo.ListEmptyBody(ctx, "aljazeera")
	if err != nil {
		slog.Error("list empty-body articles", "err", err)
		os.Exit(1)
	}

	slog.Info("found articles to backfill", "count", len(articles))

	updated, failed, skipped := 0, 0, 0

	for _, a := range articles {
		select {
		case <-ctx.Done():
			slog.Info("interrupted")
			goto done
		default:
		}

		body, rawHTML, err := fetchArticleContent(a.URL)
		if err != nil {
			slog.Warn("fetch failed", "url", a.URL, "err", err)
			failed++
			continue
		}
		if body == "" {
			slog.Info("no body extracted, skipping", "url", a.URL)
			skipped++
			continue
		}

		if err := repo.UpdateContent(ctx, a.URL, body, rawHTML); err != nil {
			slog.Error("update content", "url", a.URL, "err", err)
			failed++
			continue
		}

		slog.Info("updated", "url", a.URL, "body_len", len(body))
		updated++

		// Polite delay to avoid hammering Al Jazeera.
		time.Sleep(500 * time.Millisecond)
	}

done:
	slog.Info("backfill complete", "updated", updated, "failed", failed, "skipped", skipped)
}

// fetchArticleContent fetches an article page and returns:
//   - body: plain text from all p and li elements within the matched container
//   - rawHTML: inner HTML of the matched container
func fetchArticleContent(url string) (body, rawHTML string, err error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; immaiwin-scraper/1.0)")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close() //nolint:errcheck

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

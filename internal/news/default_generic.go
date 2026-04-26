package news

import (
	"context"
	"strings"
	"time"
)

// GenericRSSParser is a fallback parser for RSS/Atom feeds with no custom script.
// It delegates to the same parseGenericRSS helper used by the goja runtime binding.
type GenericRSSParser struct{}

func (p *GenericRSSParser) Parse(_ context.Context, in ParseInput) ([]ParsedArticle, error) {
	items := parseGenericRSS(in.Raw)
	now := time.Now().UTC()
	articles := make([]ParsedArticle, 0, len(items))
	for _, item := range items {
		url := stringField(item, "link")
		if url == "" {
			url = stringField(item, "guid")
		}
		if url == "" || !strings.HasPrefix(url, "http") {
			continue
		}
		title := stringField(item, "title")
		if title == "" {
			continue
		}
		scrapedAt := now
		if ds := stringField(item, "pubDate"); ds != "" {
			if t := tryParseDate(ds); t != "" {
				if parsed, err := time.Parse(time.RFC3339, t); err == nil {
					scrapedAt = parsed.UTC()
				}
			}
		}
		articles = append(articles, ParsedArticle{
			URL:       url,
			Title:     title,
			Body:      stringField(item, "description"),
			ScrapedAt: scrapedAt,
		})
	}
	return articles, nil
}

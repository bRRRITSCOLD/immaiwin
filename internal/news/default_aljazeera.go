package news

import (
	"context"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const AljazeeraBaseURL = "https://www.aljazeera.com"

// DefaultAljazeeraParser is the built-in parser for Al Jazeera's homepage HTML.
// It extracts article links and titles from h3 elements.
// Body content is NOT extracted here — the worker handles secondary fetches.
type DefaultAljazeeraParser struct{}

func (p *DefaultAljazeeraParser) Parse(_ context.Context, in ParseInput) ([]ParsedArticle, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(in.Raw))
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	var articles []ParsedArticle

	doc.Find("h3").Each(func(_ int, s *goquery.Selection) {
		title := strings.TrimSpace(s.Text())
		if title == "" {
			return
		}

		link := ""
		if a := s.Find("a"); a.Length() > 0 {
			link, _ = a.Attr("href")
		} else if s.Parent().Is("a") {
			link, _ = s.Parent().Attr("href")
		}
		if link == "" {
			return
		}
		if strings.HasPrefix(link, "/") {
			link = AljazeeraBaseURL + link
		}
		if !strings.HasPrefix(link, "http") {
			return
		}

		articles = append(articles, ParsedArticle{
			URL:       link,
			Title:     title,
			ScrapedAt: now,
		})
	})

	return articles, nil
}

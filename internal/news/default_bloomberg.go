package news

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

// DefaultBloombergParser is the built-in parser for Bloomberg RSS feeds.
type DefaultBloombergParser struct{}

type bloombergRSSItem struct {
	Title       string   `xml:"title"`
	Link        string   `xml:"link"`
	GUID        string   `xml:"guid"`
	Description string   `xml:"description"`
	PubDate     string   `xml:"pubDate"`
	Creator     string   `xml:"http://purl.org/dc/elements/1.1/ creator"`
	Categories  []string `xml:"category"`
	Media       struct {
		URL string `xml:"url,attr"`
	} `xml:"http://search.yahoo.com/mrss/ content"`
}

type bloombergRSSFeed struct {
	Items []bloombergRSSItem `xml:"channel>item"`
}

func (p *DefaultBloombergParser) Parse(_ context.Context, in ParseInput) ([]ParsedArticle, error) {
	var feed bloombergRSSFeed
	if err := xml.NewDecoder(strings.NewReader(in.Raw)).Decode(&feed); err != nil {
		return nil, fmt.Errorf("bloomberg parser: decode XML: %w", err)
	}

	articles := make([]ParsedArticle, 0, len(feed.Items))
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
		for _, f := range []string{time.RFC1123, time.RFC1123Z} {
			if t, err := time.Parse(f, strings.TrimSpace(item.PubDate)); err == nil {
				scrapedAt = t.UTC()
				break
			}
		}

		rawXMLBytes, _ := xml.MarshalIndent(item, "", "  ")

		meta := map[string]any{}
		if item.Creator != "" {
			meta["author"] = strings.TrimSpace(item.Creator)
		}
		if item.Media.URL != "" {
			meta["image_url"] = item.Media.URL
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

		articles = append(articles, ParsedArticle{
			URL:       url,
			Title:     title,
			Body:      strings.TrimSpace(item.Description),
			RawXML:    string(rawXMLBytes),
			ScrapedAt: scrapedAt,
			Metadata:  meta,
		})
	}
	return articles, nil
}

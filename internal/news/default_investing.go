package news

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

const investingPubDateFormat = "2006-01-02 15:04:05"

// DefaultInvestingParser is the built-in parser for Investing.com RSS feeds.
type DefaultInvestingParser struct{}

type investingRSSItem struct {
	Title     string `xml:"title"`
	Link      string `xml:"link"`
	PubDate   string `xml:"pubDate"`
	Author    string `xml:"author"`
	Enclosure struct {
		URL string `xml:"url,attr"`
	} `xml:"enclosure"`
}

type investingRSSFeed struct {
	Items []investingRSSItem `xml:"channel>item"`
}

func (p *DefaultInvestingParser) Parse(_ context.Context, in ParseInput) ([]ParsedArticle, error) {
	var feed investingRSSFeed
	if err := xml.NewDecoder(strings.NewReader(in.Raw)).Decode(&feed); err != nil {
		return nil, fmt.Errorf("investing parser: decode XML: %w", err)
	}

	articles := make([]ParsedArticle, 0, len(feed.Items))
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

		rawXMLBytes, _ := xml.MarshalIndent(item, "", "  ")

		meta := map[string]any{}
		if item.Author != "" {
			meta["author"] = strings.TrimSpace(item.Author)
		}
		if item.Enclosure.URL != "" {
			meta["image_url"] = item.Enclosure.URL
		}

		articles = append(articles, ParsedArticle{
			URL:       url,
			Title:     title,
			RawXML:    string(rawXMLBytes),
			ScrapedAt: scrapedAt,
			Metadata:  meta,
		})
	}
	return articles, nil
}

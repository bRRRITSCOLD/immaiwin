//go:build ignore

// Seed default scraper configs into MongoDB.
// Upserts feed_url + default JS parser script for each known source.
// Re-running WILL overwrite existing scripts with the seeded defaults.
//
// Usage:
//   go run ./scripts/seed/scrapers/main.go

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type seedEntry struct {
	Source  string
	FeedURL string
	Script  string
}

// bloombergScript mirrors DefaultBloombergParser in Go.
// parseRSS items: {title, link, guid, description, pubDate, author, categories:[]}
const bloombergScript = `// Bloomberg RSS parser — mirrors built-in Go default.
// parseRSS returns items with: title, link, guid, description, pubDate, author, categories
function parse(raw) {
  var items = parseRSS(raw)
  var out = []
  for (var i = 0; i < items.length; i++) {
    var item = items[i]
    var url = (item.link || item.guid || '').trim()
    if (!url || url.indexOf('http') !== 0) continue
    var title = (item.title || '').trim()
    if (!title) continue
    var meta = {}
    if (item.author) meta.author = item.author.trim()
    var cats = item.categories || []
    var syms = []
    for (var j = 0; j < cats.length; j++) {
      var s = (cats[j] || '').trim()
      if (s) syms.push(s)
    }
    if (syms.length) meta.symbols = syms
    out.push({
      url: url,
      title: title,
      body: (item.description || '').trim(),
      scraped_at: item.pubDate ? parseDate(item.pubDate) : now(),
      metadata: meta
    })
  }
  return out
}`

// investingScript mirrors DefaultInvestingParser in Go.
const investingScript = `// Investing.com RSS parser — mirrors built-in Go default.
// parseRSS returns items with: title, link, pubDate, author
function parse(raw) {
  var items = parseRSS(raw)
  var out = []
  for (var i = 0; i < items.length; i++) {
    var item = items[i]
    var url = (item.link || '').trim()
    if (!url || url.indexOf('http') !== 0) continue
    var title = (item.title || '').trim()
    if (!title) continue
    var meta = {}
    if (item.author) meta.author = item.author.trim()
    out.push({
      url: url,
      title: title,
      scraped_at: item.pubDate ? parseDate(item.pubDate) : now(),
      metadata: meta
    })
  }
  return out
}`

// aljazeeraScript extracts article links from the Al Jazeera homepage,
// then fetches each article page via httpGet() to enrich with body text.
const aljazeeraScript = `// Al Jazeera HTML parser — extracts titles/links from homepage, fetches body per article.
// each(fn) callback: fn(index, element)
function parse(raw) {
  var articles = []
  $(raw).find('h3').each(function(i, el) {
    var title = el.text().trim()
    if (!title) return
    var a = el.find('a')
    var link = a.length ? (a.attr('href') || '') : ''
    if (!link) return
    if (link.indexOf('/') === 0) link = 'https://www.aljazeera.com' + link
    if (link.indexOf('http') !== 0) return

    var body = ''
    var res = httpGet(link)
    if (res.ok) {
      var selectors = [
        '[data-testid="ArticleBodyParagraph"]',
        '.wysiwyg--all-content',
        '.article-p-wrapper',
        'article',
      ]
      for (var s = 0; s < selectors.length; s++) {
        var container = $(res.body).find(selectors[s]).first()
        if (container.length > 0) {
          body = container.text().trim()
          break
        }
      }
    }

    articles.push({ url: link, title: title, body: body, scraped_at: now() })
  })
  return articles
}`

var seeds = []seedEntry{
	{
		Source:  "bloomberg",
		FeedURL: "https://feeds.bloomberg.com/markets/news.rss",
		Script:  bloombergScript,
	},
	{
		Source:  "investing",
		FeedURL: "https://www.investing.com/rss/news_301.rss",
		Script:  investingScript,
	},
	{
		Source:  "aljazeera",
		FeedURL: "https://www.aljazeera.com/",
		Script:  aljazeeraScript,
	},
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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
	defer func() {
		if err := mc.Disconnect(ctx); err != nil {
			slog.Error("disconnect mongodb", "err", err)
		}
	}()

	col := mc.DB().Collection("news_scraper_configs")

	for _, seed := range seeds {
		_, err := col.UpdateOne(
			ctx,
			bson.M{"source": seed.Source},
			bson.M{
				"$set": bson.M{
					"source":     seed.Source,
					"feed_url":   seed.FeedURL,
					"script":     seed.Script,
					"updated_at": time.Now().UTC(),
				},
			},
			options.UpdateOne().SetUpsert(true),
		)
		if err != nil {
			slog.Error("upsert config", "source", seed.Source, "err", err)
			continue
		}
		fmt.Printf("seeded: %s\n", seed.Source)
	}
}

//go:build ignore

// Seed default scraper configs into MongoDB.
// Inserts feed_url + default JS parser script for each known source.
// Safe to re-run: only updates feed_url on existing docs; never overwrites a
// custom script a user has already saved.
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

// aljazeeraScript mirrors DefaultAljazeeraParser in Go.
// Note: each(fn) callback signature is fn(index, element) — element is the second arg.
const aljazeeraScript = `// Al Jazeera HTML parser — mirrors built-in Go default.
// Extracts h3 article links from homepage HTML.
// Note: body text is NOT extracted here; the worker fetches each article page separately.
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
    articles.push({ url: link, title: title, scraped_at: now() })
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
		// $set: always update feed_url
		// $setOnInsert: only set source + script when inserting a new doc
		//   → existing docs with custom scripts are NOT overwritten
		_, err := col.UpdateOne(
			ctx,
			bson.M{"source": seed.Source},
			bson.M{
				"$set": bson.M{
					"feed_url":   seed.FeedURL,
					"updated_at": time.Now().UTC(),
				},
				"$setOnInsert": bson.M{
					"source": seed.Source,
					"script": seed.Script,
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

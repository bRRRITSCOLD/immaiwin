//go:build ignore

// Backfill/refresh workflows — creates one Trigger→HTTPFetch→JSTransform→MongoUpsert workflow
// per scraper config. Deletes any existing workflow with the same name first (idempotent refresh).
//
// Usage:
//
//	go run ./scripts/seed/workflows/main.go

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/news"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

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

	db := mc.DB()

	// load all scraper configs
	scraperCol := db.Collection("news_scraper_configs")
	cur, err := scraperCol.Find(ctx, bson.M{})
	if err != nil {
		slog.Error("list scraper configs", "err", err)
		os.Exit(1)
	}
	var scrapers []news.ScraperConfig
	if err := cur.All(ctx, &scrapers); err != nil {
		slog.Error("decode scraper configs", "err", err)
		os.Exit(1)
	}
	if len(scrapers) == 0 {
		fmt.Println("no scraper configs found — run scripts/seed/scrapers/main.go first")
		os.Exit(0)
	}

	wfCol := db.Collection("workflows")
	now := time.Now().UTC()
	seeded := 0

	for _, sc := range scrapers {
		// delete existing workflow with same name so we get fresh node structure
		_, _ = wfCol.DeleteOne(ctx, bson.M{"name": sc.Source})

		id := newID()
		nodes, edges, params := buildNodes(sc)

		_, err := wfCol.UpdateOne(
			ctx,
			bson.M{"_id": id},
			bson.M{
				"$set": bson.M{
					"name":       sc.Source,
					"params":     params,
					"nodes":      nodes,
					"edges":      edges,
					"updated_at": now,
				},
				"$setOnInsert": bson.M{
					"created_at": now,
				},
			},
			options.UpdateOne().SetUpsert(true),
		)
		if err != nil {
			slog.Error("upsert workflow", "source", sc.Source, "err", err)
			continue
		}
		fmt.Printf("seeded: %s (id=%s)\n", sc.Source, id)
		seeded++
	}

	fmt.Printf("done: %d seeded\n", seeded)
}

// buildNodes returns source-specific nodes, edges, and params for a workflow.
// AlJazeera: two-step fetch (homepage → per-article body).
// RSS sources: single fetch + parseRSS transform.
func buildNodes(sc news.ScraperConfig) ([]workflow.Node, []workflow.Edge, map[string]string) {
	switch sc.Source {
	case "aljazeera":
		return buildAljazeeraNodes(sc)
	default:
		return buildRSSNodes(sc)
	}
}

// buildRSSNodes: trigger → http_fetch → js_transform(parseRSS) → for_each → mongo_upsert → redis_publish
func buildRSSNodes(sc news.ScraperConfig) ([]workflow.Node, []workflow.Edge, map[string]string) {
	const (
		triggerID   = "trigger-1"
		fetchID     = "http_fetch-1"
		transformID = "js_transform-1"
		forEachID   = "for_each-1"
		upsertID    = "mongo_upsert-1"
		publishID   = "redis_publish-1"
		notifyID    = "notify-1"
	)

	params := map[string]string{
		"feedURL":    sc.FeedURL,
		"platform":   sc.Source,
		"collection": "news_articles",
		"channel":    "immaiwin:news:articles",
	}

	script := `// context.fetchFeed.output = { ok, status, body } from HTTP Fetch
// Return array — ForEach iterates it item by item into Mongo Upsert
var items = parseRSS(context.fetchFeed.output.body);
return items.map(function(item) {
  return {
    url:        item.link || item.guid,
    title:      item.title,
    body:       item.description,
    scraped_at: item.pubDate ? parseDate(item.pubDate) : now(),
    platform:   params.platform,
    metadata:   { categories: item.categories }
  };
});`

	nodes := []workflow.Node{
		{ID: triggerID,   Type: workflow.NodeTypeTrigger,     Position: workflow.Position{X: 0,    Y: 60},  Data: map[string]any{}},
		{ID: fetchID,     Type: workflow.NodeTypeHTTPFetch,    Position: workflow.Position{X: 220,  Y: 40},  Data: map[string]any{"url": "{{params.feedURL}}", "name": "fetchFeed"}},
		{ID: transformID, Type: workflow.NodeTypeJSTransform,  Position: workflow.Position{X: 500,  Y: 20},  Data: map[string]any{"script": script}},
		{ID: forEachID,   Type: workflow.NodeTypeForEach,      Position: workflow.Position{X: 780,  Y: 30},  Data: map[string]any{}},
		{ID: upsertID,    Type: workflow.NodeTypeMongoUpsert,  Position: workflow.Position{X: 1020, Y: 10},  Data: map[string]any{"collection": "{{params.collection}}", "filter_field": "url"}},
		{ID: publishID,   Type: workflow.NodeTypeRedisPublish, Position: workflow.Position{X: 1280, Y: 10},  Data: map[string]any{"channel": "{{params.channel}}"}},
		{ID: notifyID,    Type: workflow.NodeTypeNotify,       Position: workflow.Position{X: 780,  Y: 240}, Data: map[string]any{"message": "{{params.platform}}: pipeline error"}},
	}
	edges := []workflow.Edge{
		{ID: "e1", Source: triggerID,   Target: fetchID},
		{ID: "e2", Source: fetchID,     Target: transformID, SourceHandle: "success"},
		{ID: "e3", Source: fetchID,     Target: notifyID,    SourceHandle: "error", TargetHandle: "in-top"},
		{ID: "e4", Source: transformID, Target: forEachID,   SourceHandle: "success"},
		{ID: "e5", Source: transformID, Target: notifyID,    SourceHandle: "error", TargetHandle: "in-left"},
		{ID: "e6", Source: forEachID,   Target: upsertID,    SourceHandle: "item"},
		{ID: "e7", Source: forEachID,   Target: notifyID,    SourceHandle: "error", TargetHandle: "in-bottom"},
		{ID: "e8", Source: upsertID,    Target: publishID,   SourceHandle: "success"},
	}
	return nodes, edges, params
}

// buildAljazeeraNodes: two-step fetch.
//
//	trigger → http_fetch(homepage) → js_transform-1(extract links [{url,title}])
//	→ for_each
//	   item → http_fetch-2({{input.url}}, name="fetchArticle")
//	            → js_transform-2(context.fetchArticle.input={url,title}, $(input.body)=article HTML)
//	            → mongo_upsert → redis_publish
//	   error → notify
func buildAljazeeraNodes(sc news.ScraperConfig) ([]workflow.Node, []workflow.Edge, map[string]string) {
	const (
		triggerID    = "trigger-1"
		fetch1ID     = "http_fetch-1"
		transform1ID = "js_transform-1"
		forEachID    = "for_each-1"
		fetch2ID     = "http_fetch-2"
		transform2ID = "js_transform-2"
		upsertID     = "mongo_upsert-1"
		publishID    = "redis_publish-1"
		notifyID     = "notify-1"
	)

	params := map[string]string{
		"feedURL":    sc.FeedURL,
		"platform":   "aljazeera",
		"baseURL":    "https://www.aljazeera.com",
		"collection": "news_articles",
		"channel":    "immaiwin:news:articles",
	}

	// Step 1: parse homepage HTML → extract article links
	// http_fetch-1 named "fetchHomepage" → context.fetchHomepage.output.body = raw HTML
	parseLinksScript := `// context.fetchHomepage.output = { ok, status, body } — raw homepage HTML
// Extract article links from h3 headings → return [{url, title}]
var articles = [];
$(context.fetchHomepage.output.body).find('h3').each(function(i, el) {
  var title = el.text().trim();
  if (!title) return;
  var a = el.find('a');
  var link = a.length ? (a.attr('href') || '') : '';
  if (!link) return;
  if (link.indexOf('/') === 0) link = params.baseURL + link;
  if (link.indexOf('http') !== 0) return;
  articles.push({ url: link, title: title });
});
return articles;`

	// Step 2: merge article metadata with fetched body.
	// http_fetch-2 named "fetchArticle":
	//   context.fetchArticle.input  = {url, title} — what http_fetch-2 received (for_each item)
	//   context.fetchArticle.output = {ok, status, body} — article page response
	mergeBodyScript := `// context.fetchArticle.input  = { url, title } — for_each item passed into http_fetch-2
// context.fetchArticle.output = { ok, status, body } — article page response
var meta = context.fetchArticle.input;
var body = '';
var selectors = [
  '[data-testid="ArticleBodyParagraph"]',
  '.wysiwyg--all-content',
  '.article-p-wrapper',
  'article',
];
for (var s = 0; s < selectors.length; s++) {
  var container = $(context.fetchArticle.output.body).find(selectors[s]).first();
  if (container.length > 0) {
    body = container.text().trim();
    break;
  }
}
return {
  url:        meta.url,
  title:      meta.title,
  body:       body,
  scraped_at: now(),
  platform:   params.platform,
};`

	nodes := []workflow.Node{
		{ID: triggerID,    Type: workflow.NodeTypeTrigger,     Position: workflow.Position{X: 0,    Y: 80},  Data: map[string]any{}},
		{ID: fetch1ID,     Type: workflow.NodeTypeHTTPFetch,    Position: workflow.Position{X: 220,  Y: 60},  Data: map[string]any{"url": "{{params.feedURL}}", "name": "fetchHomepage"}},
		{ID: transform1ID, Type: workflow.NodeTypeJSTransform,  Position: workflow.Position{X: 480,  Y: 40},  Data: map[string]any{"script": parseLinksScript}},
		{ID: forEachID,    Type: workflow.NodeTypeForEach,      Position: workflow.Position{X: 760,  Y: 50},  Data: map[string]any{"name": "forEachArticle"}},
		{ID: fetch2ID,     Type: workflow.NodeTypeHTTPFetch,    Position: workflow.Position{X: 1000, Y: 20},  Data: map[string]any{"url": "{{context.forEachArticle.item.url}}", "name": "fetchArticle"}},
		{ID: transform2ID, Type: workflow.NodeTypeJSTransform,  Position: workflow.Position{X: 1260, Y: 20},  Data: map[string]any{"script": mergeBodyScript}},
		{ID: upsertID,     Type: workflow.NodeTypeMongoUpsert,  Position: workflow.Position{X: 1520, Y: 20},  Data: map[string]any{"collection": "{{params.collection}}", "filter_field": "url"}},
		{ID: publishID,    Type: workflow.NodeTypeRedisPublish, Position: workflow.Position{X: 1780, Y: 20},  Data: map[string]any{"channel": "{{params.channel}}"}},
		{ID: notifyID,     Type: workflow.NodeTypeNotify,       Position: workflow.Position{X: 760,  Y: 260}, Data: map[string]any{"message": "{{params.platform}}: pipeline error"}},
	}
	edges := []workflow.Edge{
		{ID: "e1", Source: triggerID,    Target: fetch1ID},
		{ID: "e2", Source: fetch1ID,     Target: transform1ID, SourceHandle: "success"},
		{ID: "e3", Source: fetch1ID,     Target: notifyID,     SourceHandle: "error", TargetHandle: "in-top"},
		{ID: "e4", Source: transform1ID, Target: forEachID,    SourceHandle: "success"},
		{ID: "e5", Source: transform1ID, Target: notifyID,     SourceHandle: "error", TargetHandle: "in-left"},
		// for_each body: fetch each article, transform, upsert, publish
		{ID: "e6", Source: forEachID,    Target: fetch2ID,     SourceHandle: "item"},
		{ID: "e7", Source: forEachID,    Target: notifyID,     SourceHandle: "error", TargetHandle: "in-bottom"},
		{ID: "e8", Source: fetch2ID,     Target: transform2ID, SourceHandle: "success"},
		{ID: "e9", Source: transform2ID, Target: upsertID,     SourceHandle: "success"},
		{ID: "e10", Source: upsertID,    Target: publishID,    SourceHandle: "success"},
	}
	return nodes, edges, params
}

// newID generates a short unique string ID from current nanoseconds.
func newID() string {
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

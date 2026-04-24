package news

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Article is a generic scraped news article.
// Platform identifies the source (e.g. "aljazeera").
// Metadata holds platform-specific or extensible fields.
type Article struct {
	ID        bson.ObjectID  `bson:"_id,omitempty"         json:"id"`
	Platform  string         `bson:"platform"              json:"platform"`
	URL       string         `bson:"url"                   json:"url"`
	Title     string         `bson:"title"                 json:"title"`
	Body      string         `bson:"body,omitempty"        json:"body,omitempty"`
	RawHTML   string         `bson:"raw_html,omitempty"    json:"-"`
	RawXML    string         `bson:"raw_xml,omitempty"     json:"-"`
	ScrapedAt time.Time      `bson:"scraped_at"            json:"scraped_at"`
	Metadata  map[string]any `bson:"metadata,omitempty"    json:"metadata,omitempty"`
}

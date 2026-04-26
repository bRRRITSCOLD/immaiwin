package news

import (
	"context"
	"time"
)

// ParseInput is the raw feed content passed to a Parser.
type ParseInput struct {
	Source  string
	FeedURL string
	Raw     string // raw HTML or XML string
}

// ParsedArticle is the structured output from a Parser.
type ParsedArticle struct {
	URL       string
	Title     string
	Body      string
	RawHTML   string
	RawXML    string
	ScrapedAt time.Time      // zero → caller uses time.Now()
	Metadata  map[string]any
}

// Parser transforms raw feed content into articles.
type Parser interface {
	Parse(ctx context.Context, in ParseInput) ([]ParsedArticle, error)
}

// ScraperConfig holds configuration for a news scraper source.
type ScraperConfig struct {
	Source    string    `bson:"source"           json:"source"`
	FeedURL   string    `bson:"feed_url"         json:"feed_url"`
	Script    string    `bson:"script,omitempty" json:"script,omitempty"`
	UpdatedAt time.Time `bson:"updated_at"       json:"updated_at"`
}

// NewParser returns a ScriptParser when cfg.Script is set, otherwise defaultParser.
func NewParser(cfg ScraperConfig, defaultParser Parser) Parser {
	if cfg.Script == "" {
		return defaultParser
	}
	return &ScriptParser{script: cfg.Script}
}

// CompileScript validates that a JS script is syntactically valid and defines
// a top-level parse(raw) function. Returns an error describing the problem.
func CompileScript(script string) error {
	return validateScript(script)
}

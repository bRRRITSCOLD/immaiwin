package news

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/dop251/goja"
)

// ScriptParser runs a user-supplied goja (JS) script to parse raw feed content.
type ScriptParser struct {
	script string
}

func (p *ScriptParser) Parse(_ context.Context, in ParseInput) ([]ParsedArticle, error) {
	js, err := transpileTS(p.script)
	if err != nil {
		return nil, fmt.Errorf("transpile: %w", err)
	}
	vm, err := newScriptVM(js)
	if err != nil {
		return nil, fmt.Errorf("script compile: %w", err)
	}

	parseFn, ok := goja.AssertFunction(vm.Get("parse"))
	if !ok {
		return nil, errors.New("script must define a parse(raw) function")
	}

	result, err := parseFn(goja.Undefined(), vm.ToValue(in.Raw))
	if err != nil {
		return nil, fmt.Errorf("script runtime: %w", err)
	}

	return jsValueToArticles(result, in.Source)
}

// validateScript transpiles TS→JS then compiles in goja, checking for parse().
func validateScript(script string) error {
	js, err := transpileTS(script)
	if err != nil {
		return err
	}
	vm, err := newScriptVM(js)
	if err != nil {
		return err
	}
	if _, ok := goja.AssertFunction(vm.Get("parse")); !ok {
		return errors.New("script must define a parse(raw) function")
	}
	return nil
}

// SetTransformBindings installs sync-only helpers into vm (no HTTP).
// Used by both scraper scripts and workflow JS-transform nodes.
//
//   - $(html[, selector]) → jQuery-like Selection
//   - parseRSS(xmlStr)    → []object
//   - now()               → ISO-8601 UTC string
//   - parseDate(str)      → ISO-8601 UTC string or ""
func SetTransformBindings(vm *goja.Runtime) error {
	// $(html) → Selection; $(html, selector) → Selection.Find(selector)
	if err := vm.Set("$", func(call goja.FunctionCall) goja.Value {
		html := call.Argument(0).String()
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
		if err != nil {
			return goja.Undefined()
		}
		sel := doc.Selection
		if len(call.Arguments) > 1 {
			sel = doc.Find(call.Argument(1).String())
		}
		return wrapSel(vm, sel)
	}); err != nil {
		return fmt.Errorf("vm.Set $: %w", err)
	}

	// parseRSS(xmlStr) → []object
	if err := vm.Set("parseRSS", func(call goja.FunctionCall) goja.Value {
		xmlStr := call.Argument(0).String()
		items := parseGenericRSS(xmlStr)
		return vm.ToValue(items)
	}); err != nil {
		return fmt.Errorf("vm.Set parseRSS: %w", err)
	}

	// now() → ISO-8601 UTC string
	if err := vm.Set("now", func() string {
		return time.Now().UTC().Format(time.RFC3339)
	}); err != nil {
		return fmt.Errorf("vm.Set now: %w", err)
	}

	// parseDate(str) → ISO-8601 UTC string or ""
	if err := vm.Set("parseDate", func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(tryParseDate(call.Argument(0).String()))
	}); err != nil {
		return fmt.Errorf("vm.Set parseDate: %w", err)
	}

	return nil
}

// newScriptVM creates a sandboxed goja VM with the script already executed.
// Includes all sync helpers plus httpGet for scraper scripts.
func newScriptVM(script string) (*goja.Runtime, error) {
	vm := goja.New()

	if err := SetTransformBindings(vm); err != nil {
		return nil, err
	}

	// httpGet(url) → {ok: bool, status: int, body: string}
	// Each call has a 15-second timeout to prevent hanging on slow/blocked URLs.
	httpClient := &http.Client{Timeout: 15 * time.Second}
	if err := vm.Set("httpGet", func(call goja.FunctionCall) goja.Value {
		urlStr := call.Argument(0).String()
		obj := vm.NewObject()
		_ = obj.Set("ok", false)
		_ = obj.Set("status", 0)
		_ = obj.Set("body", "")
		req, err := http.NewRequest(http.MethodGet, urlStr, nil)
		if err != nil {
			return obj
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; immaiwin-scraper/1.0)")
		resp, err := httpClient.Do(req)
		if err != nil {
			return obj
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		_ = obj.Set("ok", resp.StatusCode >= 200 && resp.StatusCode < 300)
		_ = obj.Set("status", resp.StatusCode)
		_ = obj.Set("body", string(body))
		return obj
	}); err != nil {
		return nil, fmt.Errorf("vm.Set httpGet: %w", err)
	}

	if _, err := vm.RunString(script); err != nil {
		return nil, err
	}
	return vm, nil
}

// wrapSel wraps a goquery.Selection as a goja object exposing a jQuery-like API.
func wrapSel(vm *goja.Runtime, sel *goquery.Selection) *goja.Object {
	obj := vm.NewObject()

	_ = obj.Set("find", func(call goja.FunctionCall) goja.Value {
		return wrapSel(vm, sel.Find(call.Argument(0).String()))
	})

	_ = obj.Set("first", func() *goja.Object {
		return wrapSel(vm, sel.First())
	})

	_ = obj.Set("eq", func(call goja.FunctionCall) *goja.Object {
		return wrapSel(vm, sel.Eq(int(call.Argument(0).ToInteger())))
	})

	_ = obj.Set("text", func() string {
		return strings.TrimSpace(sel.Text())
	})

	_ = obj.Set("attr", func(call goja.FunctionCall) goja.Value {
		val, exists := sel.Attr(call.Argument(0).String())
		if !exists {
			return goja.Undefined()
		}
		return vm.ToValue(val)
	})

	_ = obj.Set("html", func() string {
		h, _ := sel.Html()
		return h
	})

	_ = obj.Set("length", sel.Length())

	_ = obj.Set("each", func(call goja.FunctionCall) goja.Value {
		fn, ok := goja.AssertFunction(call.Argument(0))
		if !ok {
			return goja.Undefined()
		}
		sel.Each(func(i int, s *goquery.Selection) {
			_, _ = fn(goja.Undefined(), vm.ToValue(i), wrapSel(vm, s))
		})
		return goja.Undefined()
	})

	return obj
}

// genericRSSItem captures common RSS item fields for the parseRSS() binding.
type genericRSSItem struct {
	Title       string   `xml:"title"`
	Link        string   `xml:"link"`
	GUID        string   `xml:"guid"`
	Description string   `xml:"description"`
	PubDate     string   `xml:"pubDate"`
	Author      string   `xml:"author"`
	Creator     string   `xml:"http://purl.org/dc/elements/1.1/ creator"`
	Categories  []string `xml:"category"`
}

type genericRSSFeed struct {
	Items []genericRSSItem `xml:"channel>item"`
}

func parseGenericRSS(xmlStr string) []map[string]any {
	var feed genericRSSFeed
	if err := xml.NewDecoder(strings.NewReader(xmlStr)).Decode(&feed); err != nil {
		return nil
	}
	items := make([]map[string]any, 0, len(feed.Items))
	for _, item := range feed.Items {
		author := strings.TrimSpace(item.Creator)
		if author == "" {
			author = strings.TrimSpace(item.Author)
		}
		m := map[string]any{
			"title":       strings.TrimSpace(item.Title),
			"link":        strings.TrimSpace(item.Link),
			"guid":        strings.TrimSpace(item.GUID),
			"description": strings.TrimSpace(item.Description),
			"pubDate":     strings.TrimSpace(item.PubDate),
			"author":      author,
			"categories":  item.Categories,
		}
		items = append(items, m)
	}
	return items
}

var dateFmts = []string{
	time.RFC1123Z,
	time.RFC1123,
	time.RFC3339,
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func tryParseDate(s string) string {
	s = strings.TrimSpace(s)
	for _, f := range dateFmts {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// jsValueToArticles converts the JS return value from parse(raw) into []ParsedArticle.
func jsValueToArticles(v goja.Value, source string) ([]ParsedArticle, error) {
	exported := v.Export()
	raw, ok := exported.([]interface{})
	if !ok {
		return nil, errors.New("parse() must return an array")
	}

	now := time.Now().UTC()
	articles := make([]ParsedArticle, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		url := stringField(m, "url")
		title := stringField(m, "title")
		if url == "" || title == "" {
			continue
		}

		scrapedAt := now
		if ds := stringField(m, "scraped_at"); ds != "" {
			if t, err := time.Parse(time.RFC3339, ds); err == nil {
				scrapedAt = t.UTC()
			}
		}

		var metadata map[string]any
		if meta, ok := m["metadata"].(map[string]interface{}); ok {
			metadata = meta
		}

		articles = append(articles, ParsedArticle{
			URL:       url,
			Title:     title,
			Body:      stringField(m, "body"),
			RawHTML:   stringField(m, "raw_html"),
			RawXML:    stringField(m, "raw_xml"),
			ScrapedAt: scrapedAt,
			Metadata:  metadata,
		})
	}
	return articles, nil
}

func stringField(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

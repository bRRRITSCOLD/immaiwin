package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	polymarketsdk "github.com/GoPolymarket/polymarket-go-sdk"
	"github.com/GoPolymarket/polymarket-go-sdk/pkg/clob/ws"
	"github.com/GoPolymarket/polymarket-go-sdk/pkg/gamma"
)

const gammaBaseURL = "https://gamma-api.polymarket.com"

type ClientConfig struct{}

// Client wraps the Polymarket SDK WebSocket client for trade monitoring.
type Client struct {
	pgs *polymarketsdk.Client
}

// New creates a Polymarket client and validates the SDK WebSocket initialised correctly.
func New(cfg ClientConfig) (*Client, error) {
	pgs := polymarketsdk.NewClient()
	if pgs.CLOBWS == nil {
		return nil, fmt.Errorf("polymarket: WebSocket client failed to initialise")
	}

	if pgs.Gamma == nil {
		return nil, fmt.Errorf("polymarket: Gamma client failed to initialise")
	}

	return &Client{pgs: pgs}, nil
}

// WatchTrades subscribes to last-trade-price events for all configured token IDs.
// The returned channel is closed when ctx is cancelled or the connection drops.
func (c *Client) WatchTrades(ctx context.Context, tokenIDs []string) (<-chan ws.LastTradePriceEvent, error) {
	return c.pgs.CLOBWS.SubscribeLastTradePrices(ctx, tokenIDs)
}

func (c *Client) GetMarkets(ctx context.Context, req *gamma.MarketsRequest) ([]gamma.Market, error) {
	return c.pgs.Gamma.GetMarkets(ctx, req)
}

func (c *Client) GetEvents(ctx context.Context, req *gamma.EventsRequest) ([]gamma.Event, error) {
	return c.pgs.Gamma.Events(ctx, req)
}

func (c *Client) SearchEvents(ctx context.Context, req *gamma.PublicSearchRequest) ([]gamma.Event, error) {
	results, err := c.pgs.Gamma.PublicSearch(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("polymarket: search events: %w", err)
	}
	return results.Events, nil
}

func (c *Client) GetMarket(ctx context.Context, id string) (*gamma.Market, error) {
	return c.pgs.Gamma.GetMarket(ctx, id)
}

// SearchMarkets calls GET /markets directly, bypassing the SDK's buildMarketsQuery which omits
// slug_contains. All other filters (active, closed, order, limit, offset) are passed server-side.
func (c *Client) SearchMarkets(ctx context.Context, req *gamma.MarketsRequest) ([]gamma.Market, error) {
	q := url.Values{}
	if req != nil {
		if req.SlugContains != "" {
			q.Set("slug_contains", req.SlugContains)
		}
		if req.Active != nil {
			q.Set("active", strconv.FormatBool(*req.Active))
		}
		if req.Closed != nil {
			q.Set("closed", strconv.FormatBool(*req.Closed))
		}
		if req.Order != "" {
			q.Set("order", req.Order)
		}
		if req.Ascending != nil {
			q.Set("ascending", strconv.FormatBool(*req.Ascending))
		}
		if req.Limit != nil {
			q.Set("limit", strconv.Itoa(*req.Limit))
		}
		if req.Offset != nil {
			q.Set("offset", strconv.Itoa(*req.Offset))
		}
		if req.EndDateMin != "" {
			q.Set("end_date_min", req.EndDateMin)
		}
	}
	endpoint := gammaBaseURL + "/markets"
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("polymarket: build search request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("polymarket: search markets: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("polymarket: search markets: status %d", resp.StatusCode)
	}
	var markets []gamma.Market
	if err := json.NewDecoder(resp.Body).Decode(&markets); err != nil {
		return nil, fmt.Errorf("polymarket: decode markets: %w", err)
	}
	return markets, nil
}

func (c *Client) Close() error {
	err := c.pgs.CLOBWS.Close()
	if err != nil {
		return err
	}

	return nil
}

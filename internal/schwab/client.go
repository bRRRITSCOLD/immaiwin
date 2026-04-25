package schwab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/options"
)

const marketDataBase = "https://api.schwabapi.com/marketdata/v1"

// Client implements options.Provider using the Schwab Market Data API.
type Client struct {
	tokens *TokenManager
	http   *http.Client
}

func NewClient(tokens *TokenManager) *Client {
	return &Client{
		tokens: tokens,
		http:   &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) Close() error { return nil }

// GetExpirations returns unique expiration dates (YYYY-MM-DD) for an underlying.
func (c *Client) GetExpirations(ctx context.Context, symbol string) ([]string, error) {
	// Fetch a small chain to get all available expirations.
	url := fmt.Sprintf("%s/chains?symbol=%s&contractType=ALL&strikeCount=1", marketDataBase, symbol)
	var resp chainResponse
	if err := c.get(ctx, url, &resp); err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var dates []string
	for key := range resp.CallExpDateMap {
		// key format: "YYYY-MM-DD:N" where N = days to expiration
		date := strings.SplitN(key, ":", 2)[0]
		if _, ok := seen[date]; !ok {
			seen[date] = struct{}{}
			dates = append(dates, date)
		}
	}
	return dates, nil
}

// GetChain returns all contracts for a symbol within a date range.
// expiration is YYYY-MM-DD; pass empty string to use today+30d window.
func (c *Client) GetChain(ctx context.Context, symbol, expiration string) ([]options.Contract, error) {
	from := expiration
	to := expiration
	if expiration == "" {
		from = time.Now().UTC().Format("2006-01-02")
		to = time.Now().UTC().AddDate(0, 1, 0).Format("2006-01-02")
	}
	url := fmt.Sprintf(
		"%s/chains?symbol=%s&contractType=ALL&includeUnderlyingQuote=false&strikeCount=10&fromDate=%s&toDate=%s",
		marketDataBase, symbol, from, to,
	)
	var resp chainResponse
	if err := c.get(ctx, url, &resp); err != nil {
		return nil, err
	}
	return flattenChain(resp), nil
}

// GetUserPreferences returns the Schwab streamer connection info.
func (c *Client) GetUserPreferences(ctx context.Context) (*preferencesResponse, error) {
	var resp preferencesResponse
	if err := c.get(ctx, "https://api.schwabapi.com/trader/v1/userPreference", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) get(ctx context.Context, url string, out any) error {
	tok, err := c.tokens.AccessToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			slog.Error("schwab: close response body", "url", url, "err", err)
		}
	}()

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("schwab GET %s: HTTP %d", url, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// ResolveFuturesSymbols maps root symbols (e.g. "/CL") to their active front-month contract symbol.
// Falls back to the root symbol itself when the API does not return an active contract.
func (c *Client) ResolveFuturesSymbols(ctx context.Context, roots []string) (map[string]string, error) {
	if len(roots) == 0 {
		return map[string]string{}, nil
	}
	// URL-encode each symbol individually then join — /CL must become %2FCL.
	encoded := make([]string, len(roots))
	for i, r := range roots {
		encoded[i] = strings.ReplaceAll(r, "/", "%2F")
	}
	rawURL := fmt.Sprintf("%s/quotes?symbols=%s", marketDataBase, strings.Join(encoded, ","))

	// Use raw bytes to log the response for debugging.
	raw, err := c.getRaw(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	slog.Debug("schwab futures quotes raw", "url", rawURL, "body", string(raw))

	var resp quotesResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("schwab futures quotes: decode: %w", err)
	}
	// Map key = active contract symbol; root = entry.Reference.Product.
	result := make(map[string]string, len(roots))
	for contractSym, entry := range resp {
		if entry.Reference != nil && entry.Reference.Product != "" {
			result[entry.Reference.Product] = contractSym
		}
	}
	slog.Info("schwab futures quotes resolved", "roots", roots, "result", result)
	return result, nil
}

func (c *Client) getRaw(ctx context.Context, url string) ([]byte, error) {
	tok, err := c.tokens.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			slog.Error("schwab: close response body raw", "url", url, "err", err)
		}
	}()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("schwab GET %s: HTTP %d: %s", url, res.StatusCode, body)
	}
	return body, nil
}

// flattenChain converts the nested call/put exp-date maps into a flat slice.
func flattenChain(resp chainResponse) []options.Contract {
	var contracts []options.Contract
	process := func(m map[string]map[string][]chainOptionRaw, putCall string) {
		for _, strikes := range m {
			for _, opts := range strikes {
				for _, o := range opts {
					exp, _ := time.Parse("2006-01-02", o.ExpirationDate)
					contracts = append(contracts, options.Contract{
						Symbol:     o.Symbol,
						Underlying: resp.Symbol,
						Strike:     o.StrikePrice,
						Expiration: exp,
						Type:       strings.ToLower(putCall),
						Bid:        o.Bid,
						Ask:        o.Ask,
						Last:       o.Last,
						Volume:     o.TotalVolume,
						OI:         o.OpenInterest,
						IV:         o.Volatility / 100.0, // Schwab returns 0–100
					})
				}
			}
		}
	}
	process(resp.CallExpDateMap, "call")
	process(resp.PutExpDateMap, "put")
	return contracts
}

// ParseSchwabSymbol parses a Schwab option symbol like "SPY_020725C400".
// Format: {underlying}_{MMDDYY}{C/P}{strike}
func ParseSchwabSymbol(sym string) (underlying string, expiry time.Time, optType string, strike float64, err error) {
	idx := strings.LastIndex(sym, "_")
	if idx < 0 || idx+7 > len(sym) {
		return "", time.Time{}, "", 0, fmt.Errorf("invalid schwab symbol: %s", sym)
	}
	underlying = sym[:idx]
	rest := sym[idx+1:]
	if len(rest) < 7 {
		return "", time.Time{}, "", 0, fmt.Errorf("invalid schwab symbol tail: %s", sym)
	}
	dateStr := rest[:6] // MMDDYY
	expiry, err = time.Parse("010206", dateStr)
	if err != nil {
		return "", time.Time{}, "", 0, fmt.Errorf("invalid date in %s: %w", sym, err)
	}
	switch rest[6] {
	case 'C', 'c':
		optType = "call"
	case 'P', 'p':
		optType = "put"
	default:
		return "", time.Time{}, "", 0, fmt.Errorf("invalid type in %s", sym)
	}
	_, err = fmt.Sscanf(rest[7:], "%f", &strike)
	if err != nil {
		return "", time.Time{}, "", 0, fmt.Errorf("invalid strike in %s: %w", sym, err)
	}
	return underlying, expiry, optType, strike, nil
}

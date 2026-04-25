package schwab

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/futures"
	"github.com/gorilla/websocket"
)

// StreamFutures implements futures streaming using the Schwab Streamer LEVELONE_FUTURES service.
// contractSymbols are active contract symbols, e.g. "/CLM25".
func (c *Client) StreamFutures(ctx context.Context, contractSymbols []string) (<-chan futures.Trade, error) {
	ch, _, err := c.streamFutures(ctx, contractSymbols)
	return ch, err
}

// StreamFuturesEx returns the trade channel plus a close-reason channel.
func (c *Client) StreamFuturesEx(ctx context.Context, contractSymbols []string) (<-chan futures.Trade, <-chan error, error) {
	return c.streamFutures(ctx, contractSymbols)
}

func (c *Client) streamFutures(ctx context.Context, contractSymbols []string) (<-chan futures.Trade, <-chan error, error) {
	prefs, err := c.GetUserPreferences(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("schwab futures stream: get preferences: %w", err)
	}
	if len(prefs.StreamerInfo) == 0 {
		return nil, nil, fmt.Errorf("schwab futures stream: no streamer info in preferences")
	}
	info := prefs.StreamerInfo[0]

	tok, err := c.tokens.AccessToken(ctx)
	if err != nil {
		return nil, nil, err
	}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, info.StreamerSocketURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("schwab futures stream: dial %s: %w", info.StreamerSocketURL, err)
	}

	// LOGIN
	loginReq := streamerRequest{Requests: []streamerCommand{{
		Service:    "ADMIN",
		Command:    "LOGIN",
		RequestID:  nextID(),
		CustomerID: info.CustomerID,
		CorrelID:   info.CorrelID,
		Parameters: map[string]any{
			"Authorization":          "Bearer " + tok,
			"SchwabClientChannel":    info.Channel,
			"SchwabClientFunctionId": info.FunctionID,
		},
	}}}
	if err := conn.WriteJSON(loginReq); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("schwab futures stream: login: %w", err)
	}

	_, msg, err := conn.ReadMessage()
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("schwab futures stream: login response: %w", err)
	}
	var loginResp streamerResponse
	if err := json.Unmarshal(msg, &loginResp); err == nil {
		for _, r := range loginResp.Response {
			if r.Content.Code != 0 {
				_ = conn.Close()
				return nil, nil, fmt.Errorf("schwab futures stream: login failed: %s", r.Content.Message)
			}
		}
	}

	// Subscribe to LEVELONE_FUTURES.
	// fields: 0=key, 3=LAST_PRICE, 8=TOTAL_VOLUME, 9=LAST_SIZE, 23=OPEN_INTEREST
	const fields = "0,3,8,9,23"
	keys := strings.Join(contractSymbols, ",")
	subReq := streamerRequest{Requests: []streamerCommand{{
		Service:    "LEVELONE_FUTURES",
		Command:    "SUBS",
		RequestID:  nextID(),
		CustomerID: info.CustomerID,
		CorrelID:   info.CorrelID,
		Parameters: map[string]any{
			"keys":   keys,
			"fields": fields,
		},
	}}}
	if err := conn.WriteJSON(subReq); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("schwab futures stream: subscribe: %w", err)
	}

	ch := make(chan futures.Trade, 256)
	closeCh := make(chan error, 1)

	go func() {
		var closeErr error
		defer close(ch)
		defer func() {
			if err := conn.Close(); err != nil {
				slog.Error("schwab futures stream: close connection", "err", err)
			}
		}()
		defer func() {
			closeCh <- closeErr
			close(closeCh)
		}()

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			_, msg, err := conn.ReadMessage()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				closeErr = err
				slog.Error("schwab futures stream: read", "err", err)
				return
			}

			var resp streamerResponse
			if err := json.Unmarshal(msg, &resp); err != nil {
				continue
			}

			for _, d := range resp.Data {
				if d.Service != "LEVELONE_FUTURES" {
					continue
				}
				for _, item := range d.Content {
					trade, err := contentToFuturesTrade(item, d.Timestamp)
					if err != nil {
						slog.Warn("schwab futures stream: parse trade", "err", err)
						continue
					}
					select {
					case ch <- trade:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()

	return ch, closeCh, nil
}

func contentToFuturesTrade(m map[string]interface{}, tsMs int64) (futures.Trade, error) {
	sym, _ := m["key"].(string)
	if sym == "" {
		return futures.Trade{}, fmt.Errorf("missing key")
	}

	// Schwab may return named keys (LAST_PRICE) or numeric string keys ("3").
	price := firstFloat(m, "LAST_PRICE", "3")
	size := int64(firstFloat(m, "LAST_SIZE", "9"))
	volume := int64(firstFloat(m, "TOTAL_VOLUME", "8"))
	oi := int64(firstFloat(m, "OPEN_INTEREST", "23"))

	var ts time.Time
	if tsMs > 0 {
		ts = time.UnixMilli(tsMs).UTC()
	} else {
		ts = time.Now().UTC()
	}

	return futures.Trade{
		Symbol:    sym,
		Root:      futuresRoot(sym),
		Price:     price,
		Size:      size,
		Volume:    volume,
		OI:        oi,
		Timestamp: ts,
	}, nil
}

// firstFloat returns the float64 value of the first key found in m.
func firstFloat(m map[string]interface{}, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if f := toFloat(v); f != 0 {
				return f
			}
		}
	}
	return 0
}

// futuresRoot strips the contract month+year suffix from a futures symbol.
// e.g. "/CLM25" → "/CL", "/ESH26" → "/ES".
func futuresRoot(sym string) string {
	const months = "FGHJKMNQUVXZ"
	if len(sym) >= 4 {
		tail := sym[len(sym)-3:]
		if strings.ContainsRune(months, rune(tail[0])) &&
			tail[1] >= '0' && tail[1] <= '9' &&
			tail[2] >= '0' && tail[2] <= '9' {
			return sym[:len(sym)-3]
		}
	}
	return sym
}

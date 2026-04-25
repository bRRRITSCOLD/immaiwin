package schwab

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/options"
	"github.com/gorilla/websocket"
)

var reqID int

func nextID() int {
	reqID++
	return reqID
}

// StreamTrades implements options.Provider.StreamTrades using the Schwab Streamer.
// optionSymbols are Schwab-format option keys (e.g. "SPY_020725C400").
func (c *Client) StreamTrades(ctx context.Context, optionSymbols []string) (<-chan options.Trade, error) {
	ch, _, err := c.streamTrades(ctx, optionSymbols)
	return ch, err
}

// StreamTradesEx returns the trade channel plus a close-reason channel that
// receives one value (the WS close error or nil) when the stream ends.
func (c *Client) StreamTradesEx(ctx context.Context, optionSymbols []string) (<-chan options.Trade, <-chan error, error) {
	return c.streamTrades(ctx, optionSymbols)
}

func (c *Client) streamTrades(ctx context.Context, optionSymbols []string) (<-chan options.Trade, <-chan error, error) {
	prefs, err := c.GetUserPreferences(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("schwab stream: get preferences: %w", err)
	}
	if len(prefs.StreamerInfo) == 0 {
		return nil, nil, fmt.Errorf("schwab stream: no streamer info in preferences")
	}
	info := prefs.StreamerInfo[0]

	tok, err := c.tokens.AccessToken(ctx)
	if err != nil {
		return nil, nil, err
	}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, info.StreamerSocketURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("schwab stream: dial %s: %w", info.StreamerSocketURL, err)
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
		return nil, nil, fmt.Errorf("schwab stream: login: %w", err)
	}

	// Read login response.
	_, msg, err := conn.ReadMessage()
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("schwab stream: login response: %w", err)
	}
	var loginResp streamerResponse
	if err := json.Unmarshal(msg, &loginResp); err == nil {
		for _, r := range loginResp.Response {
			if r.Content.Code != 0 {
				_ = conn.Close()
				return nil, nil, fmt.Errorf("schwab stream: login failed: %s", r.Content.Message)
			}
		}
	}

	// Subscribe to LEVELONE_OPTIONS in chunks of 500 (streamer key limit).
	const subChunkSize = 500
	const fields = "0,1,2,3,9,11,24,29,30,38" // key,bid,ask,last,volume,OI,IV,bidSz,askSz,lastSz
	for i := 0; i < len(optionSymbols); i += subChunkSize {
		end := i + subChunkSize
		if end > len(optionSymbols) {
			end = len(optionSymbols)
		}
		chunk := optionSymbols[i:end]
		keys := ""
		for j, s := range chunk {
			if j > 0 {
				keys += ","
			}
			keys += s
		}
		cmd := "SUBS"
		if i > 0 {
			cmd = "ADD"
		}
		subReq := streamerRequest{Requests: []streamerCommand{{
			Service:    "LEVELONE_OPTIONS",
			Command:    cmd,
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
			return nil, nil, fmt.Errorf("schwab stream: subscribe chunk %d: %w", i/subChunkSize, err)
		}
	}

	ch := make(chan options.Trade, 256)
	closeCh := make(chan error, 1)

	go func() {
		var closeErr error
		defer close(ch)
		defer func() {
			if err := conn.Close(); err != nil {
				slog.Error("schwab stream: close connection", "err", err)
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
				slog.Error("schwab stream: read", "err", err)
				return
			}

			var resp streamerResponse
			if err := json.Unmarshal(msg, &resp); err != nil {
				continue
			}

			for _, d := range resp.Data {
				if d.Service != "LEVELONE_OPTIONS" {
					continue
				}
				for _, item := range d.Content {
					trade, err := contentToTrade(item, d.Timestamp)
					if err != nil {
						slog.Warn("schwab stream: parse trade", "err", err)
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

func contentToTrade(m map[string]interface{}, tsMs int64) (options.Trade, error) {
	sym, _ := m["key"].(string)
	if sym == "" {
		return options.Trade{}, fmt.Errorf("missing key")
	}

	underlying, expiry, optType, strike, err := ParseSchwabSymbol(sym)
	if err != nil {
		return options.Trade{}, err
	}

	price := toFloat(m["last"])
	size := int64(toFloat(m["lastSize"]))

	var ts time.Time
	if tsMs > 0 {
		ts = time.UnixMilli(tsMs).UTC()
	} else {
		ts = time.Now().UTC()
	}

	return options.Trade{
		Symbol:     sym,
		Underlying: underlying,
		Strike:     strike,
		Expiration: expiry,
		Type:       optType,
		Price:      price,
		Size:       size,
		Timestamp:  ts,
	}, nil
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

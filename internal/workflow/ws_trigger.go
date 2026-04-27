package workflow

import (
	"strings"

	"github.com/GoPolymarket/polymarket-go-sdk/pkg/clob/ws"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/futures"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/options"
)

// WSTriggerInfo holds the websocket trigger config extracted from a workflow trigger node.
type WSTriggerInfo struct {
	TriggerType  string   // "polymarket_ws", "schwab_ws"
	ConnectionID string
	AssetIDs     []string // polymarket_ws: CLOB token IDs
	Symbols      []string // schwab_ws: option keys or futures contracts
	StreamType   string   // schwab_ws: "options" or "futures"
}

// WSInfoFromWorkflow extracts WS trigger info from the first matching trigger node.
func WSInfoFromWorkflow(wf Workflow) (WSTriggerInfo, bool) {
	for _, n := range wf.Nodes {
		if n.Type != NodeTypeTrigger {
			continue
		}
		tt, _ := n.Data["trigger_type"].(string)
		switch tt {
		case "polymarket_ws":
			raw, _ := n.Data["asset_ids"].(string)
			var ids []string
			for _, line := range strings.Split(raw, "\n") {
				id := strings.TrimSpace(line)
				if id != "" {
					ids = append(ids, id)
				}
			}
			if len(ids) == 0 {
				continue
			}
			connID, _ := n.Data["connection_id"].(string)
			return WSTriggerInfo{
				TriggerType:  tt,
				ConnectionID: connID,
				AssetIDs:     ids,
			}, true
		case "schwab_ws":
			raw, _ := n.Data["symbols"].(string)
			var syms []string
			for _, line := range strings.Split(raw, "\n") {
				s := strings.TrimSpace(line)
				if s != "" {
					syms = append(syms, s)
				}
			}
			if len(syms) == 0 {
				continue
			}
			connID, _ := n.Data["connection_id"].(string)
			streamType, _ := n.Data["stream_type"].(string)
			return WSTriggerInfo{
				TriggerType:  tt,
				ConnectionID: connID,
				Symbols:      syms,
				StreamType:   streamType,
			}, true
		default:
			continue
		}
	}
	return WSTriggerInfo{}, false
}

// PolymarketTradeInput converts a Polymarket trade event to a workflow input map.
func PolymarketTradeInput(ev ws.LastTradePriceEvent) map[string]any {
	return map[string]any{
		"asset_id":     ev.AssetID,
		"market":       ev.Market,
		"price":        ev.Price,
		"size":         ev.Size,
		"side":         ev.Side,
		"fee_rate_bps": ev.FeeRateBps,
		"timestamp":    ev.Timestamp,
	}
}

// SchwabOptionsTradeInput converts a Schwab options trade to a workflow input map.
func SchwabOptionsTradeInput(t options.Trade) map[string]any {
	return map[string]any{
		"symbol":     t.Symbol,
		"underlying": t.Underlying,
		"strike":     t.Strike,
		"expiration": t.Expiration,
		"type":       t.Type,
		"price":      t.Price,
		"size":       t.Size,
		"exchange":   t.Exchange,
		"timestamp":  t.Timestamp,
	}
}

// SchwabFuturesTradeInput converts a Schwab futures trade to a workflow input map.
func SchwabFuturesTradeInput(t futures.Trade) map[string]any {
	return map[string]any{
		"symbol":    t.Symbol,
		"root":      t.Root,
		"price":     t.Price,
		"size":      t.Size,
		"volume":    t.Volume,
		"oi":        t.OI,
		"timestamp": t.Timestamp,
	}
}

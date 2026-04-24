package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/GoPolymarket/polymarket-go-sdk/pkg/clob/ws"
	"github.com/GoPolymarket/polymarket-go-sdk/pkg/gamma"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/polymarket"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/trade"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/watchlist"
)

// PolymarketWatcherWorker monitors markets in the watchlist collection for unusual trades.
var PolymarketWatcherWorker = &polymarketWatcher{}

type polymarketWatcher struct{}

func (w *polymarketWatcher) Name() string { return "polymarket-watcher" }

func (w *polymarketWatcher) Run(ctx context.Context) error {
	cfg, err := config.Load(config.WithDotEnv(".env"))
	if err != nil {
		return fmt.Errorf("polymarket-watcher: load config: %w", err)
	}

	rc := rediss.New(cfg.Redis)
	defer func() {
		if err := rc.Close(); err != nil {
			slog.Error("polymarket-watcher: close redis client", "err", err)
		}
	}()

	detCfg, err := loadDetectorEnv()
	if err != nil {
		return fmt.Errorf("polymarket-watcher: %w", err)
	}

	client, err := polymarket.New(polymarket.ClientConfig{})
	if err != nil {
		return fmt.Errorf("polymarket-watcher: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			slog.Error("polymarket-watcher: close polymarket client", "err", err)
		}
	}()

	mc, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		return fmt.Errorf("polymarket-watcher: connect mongodb: %w", err)
	}
	defer func() {
		if err := mc.Disconnect(ctx); err != nil {
			slog.Error("polymarket-watcher: disconnect mongodb", "err", err)
		}
	}()

	wlRepo := mongodb.NewWatchlistRepository(mc.DB())

	det := polymarket.NewDetector(polymarket.DetectorConfig{
		WindowSize:      detCfg.windowSize,
		SizeMultiplier:  detCfg.sizeMultiplier,
		MinAbsoluteSize: detCfg.minAbsoluteSize,
	})

	pollInterval := loadPollInterval()
	pollTicker := time.NewTicker(pollInterval)
	defer pollTicker.Stop()

	// tradeCh is nil when idle (no watchlist items or subscribe failed).
	// A nil channel in a select case is never selected — safe idle behaviour.
	var (
		subCtx            context.Context
		cancelSub         context.CancelFunc
		tradeCh           <-chan ws.LastTradePriceEvent
		activeTokenIDs    []string
		activeExprs       map[string]string                   // tokenID → expr string, for change detection
		activeWindowSizes map[string]int                      // tokenID → window size, for change detection
		questionByToken   map[string]string                   // assetID → market question
		outcomeByToken    map[string]string                   // assetID → outcome label (e.g. "Yes"/"No")
		compiledByToken   map[string]*polymarket.CompiledExpr // assetID → compiled expr (nil = use default detector)
	)

	fetchWatchlist := func() ([]string, map[string]string, map[string]string, map[string]string, map[string]int) {
		ids, qmap, omap, exprs, windows, err := clobTokenIDsFromWatchlist(ctx, wlRepo, client)
		if err != nil {
			slog.Error("polymarket watcher: fetch watchlist token IDs", "err", err)
			return nil, nil, nil, nil, nil
		}
		return ids, qmap, omap, exprs, windows
	}

	restartSub := func(tokenIDs []string, qmap map[string]string, omap map[string]string, exprByToken map[string]string, windowByToken map[string]int) {
		if cancelSub != nil {
			cancelSub()
			cancelSub = nil
		}
		tradeCh = nil
		activeTokenIDs = nil
		activeExprs = exprByToken
		activeWindowSizes = windowByToken

		// Merge new maps with old — preserve non-empty known values so a
		// reconnect where ParsedTokens returns empty outcomes doesn't clobber
		// previously resolved labels.
		mergedQ := make(map[string]string)
		for k, v := range questionByToken {
			mergedQ[k] = v
		}
		for k, v := range qmap {
			if v != "" {
				mergedQ[k] = v
			}
		}
		mergedO := make(map[string]string)
		for k, v := range outcomeByToken {
			mergedO[k] = v
		}
		for k, v := range omap {
			if v != "" {
				mergedO[k] = v
			}
		}
		questionByToken = mergedQ
		outcomeByToken = mergedO

		// Apply per-market window sizes. SetWindowSize no-ops if size unchanged (preserves history).
		for tokenID, n := range windowByToken {
			if n > 0 {
				det.SetWindowSize(tokenID, n)
			}
		}

		// Compile per-market expressions. Tokens with no expr use the default detector.
		compiled := make(map[string]*polymarket.CompiledExpr)
		for tokenID, exprStr := range exprByToken {
			if exprStr == "" {
				continue
			}
			prog, err := polymarket.CompileExpr(exprStr)
			if err != nil {
				slog.Error("polymarket watcher: compile expr, falling back to default detector",
					"token_id", tokenID,
					"err", err,
				)
				continue
			}
			compiled[tokenID] = prog
		}
		compiledByToken = compiled

		if len(tokenIDs) == 0 {
			slog.Info("polymarket watcher: watchlist empty, idle")
			return
		}

		subCtx, cancelSub = context.WithCancel(ctx)
		ch, err := client.WatchTrades(subCtx, tokenIDs)
		if err != nil {
			cancelSub()
			cancelSub = nil
			slog.Error("polymarket watcher: subscribe failed", "err", err)
			return
		}
		tradeCh = ch
		activeTokenIDs = tokenIDs
		slog.Info("polymarket watcher: subscribed", "token_ids", tokenIDs)
	}

	// Initial subscription from current watchlist.
	restartSub(fetchWatchlist())

	for {
		select {
		case <-ctx.Done():
			if cancelSub != nil {
				cancelSub()
			}
			slog.Info("polymarket watcher stopped")
			return nil

		case <-pollTicker.C:
			newIDs, newQmap, newOmap, newExprs, newWindows := fetchWatchlist()
			if !sameStringSlice(activeTokenIDs, newIDs) || !sameMaps(activeExprs, newExprs) || !sameMapsInt(activeWindowSizes, newWindows) {
				slog.Info("polymarket watcher: watchlist changed, restarting subscription",
					"old_count", len(activeTokenIDs),
					"new_count", len(newIDs),
				)
				restartSub(newIDs, newQmap, newOmap, newExprs, newWindows)
			}

		case event, ok := <-tradeCh: // nil when idle — never fires
			if !ok {
				slog.Warn("polymarket watcher: trade stream closed, reconnecting immediately")
				restartSub(fetchWatchlist())
				continue
			}

			var (
				detected bool
				unusual  *polymarket.UnusualTrade
			)

			if prog, hasExpr := compiledByToken[event.AssetID]; hasExpr {
				// Per-market custom expression: evaluate and always update rolling window.
				size, _ := strconv.ParseFloat(event.Size, 64)
				price, _ := strconv.ParseFloat(event.Price, 64)
				avg := det.RollingAvg(event.AssetID)
				windowFull := det.IsWindowFull(event.AssetID)
				det.UpdateWindow(event.AssetID, size)

				env := polymarket.TradeEnv{
					Size:       size,
					Price:      price,
					Side:       event.Side,
					Avg:        avg,
					WindowFull: windowFull,
					AssetID:    event.AssetID,
					Market:     event.Market,
				}
				match, err := prog.Eval(env)
				if err != nil {
					slog.Error("polymarket watcher: eval expr", "asset_id", event.AssetID, "err", err)
				} else if match {
					detected = true
					unusual = &polymarket.UnusualTrade{
						LastTradePriceEvent: event,
						RollingAvgSize:      avg,
						Reason:              "custom expression",
					}
				}
			} else {
				unusual, detected = det.Process(event)
			}

			tradeExpr := activeExprs[event.AssetID]
			if tradeExpr == "" {
				tradeExpr = watchlist.DefaultExpr
			}

			tradeEvent := trade.TradeEvent{
				AssetID:        event.AssetID,
				Market:         event.Market,
				MarketQuestion: questionByToken[event.AssetID],
				TokenOutcome:   outcomeByToken[event.AssetID],
				Price:          event.Price,
				Size:           event.Size,
				Side:           event.Side,
				FeeRateBps:     event.FeeRateBps,
				Timestamp:      event.Timestamp,
				Unusual:        detected,
				Expr:           tradeExpr,
				DetectedAt:     time.Now().UTC(),
			}
			if detected {
				tradeEvent.RollingAvgSize = unusual.RollingAvgSize
				tradeEvent.Reason = unusual.Reason
				slog.Warn("unusual trade detected",
					"asset_id", event.AssetID,
					"market", event.Market,
					"price", event.Price,
					"size", event.Size,
					"side", event.Side,
					"rolling_avg_size", fmt.Sprintf("%.2f", unusual.RollingAvgSize),
					"reason", unusual.Reason,
				)
			}

			payload, err := json.Marshal(tradeEvent)
			if err != nil {
				slog.Error("polymarket-watcher: marshal trade event", "err", err)
			} else if err := rc.Publish(ctx, rediss.TradesChannel, payload); err != nil {
				slog.Error("polymarket-watcher: publish trade event", "err", err)
			}
		}
	}
}

// clobTokenIDsFromWatchlist fetches watchlist market IDs, looks up their CLOB token IDs
// via the Polymarket API, and returns:
//   - deduplicated sorted slice of token IDs for WatchTrades
//   - map of assetID → market question
//   - map of assetID → outcome label (e.g. "Yes", "No")
//   - map of assetID → custom unusual-trade expression (empty string = use default detector)
//   - map of assetID → rolling window size (0 = use global default)
func clobTokenIDsFromWatchlist(ctx context.Context, wlRepo *mongodb.WatchlistRepository, client *polymarket.Client) ([]string, map[string]string, map[string]string, map[string]string, map[string]int, error) {
	items, err := wlRepo.List(ctx)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("list watchlist: %w", err)
	}
	if len(items) == 0 {
		return nil, nil, nil, nil, nil, nil
	}

	// Build marketID → config maps from watchlist items.
	itemExpr := make(map[string]string, len(items))
	itemWindow := make(map[string]int, len(items))
	marketIDs := make([]string, len(items))
	for i, item := range items {
		marketIDs[i] = item.MarketID
		itemExpr[item.MarketID] = item.UnusualExpr
		itemWindow[item.MarketID] = item.WindowSize
	}

	markets, err := client.GetMarkets(ctx, &gamma.MarketsRequest{IDs: marketIDs})
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("get markets: %w", err)
	}

	seen := make(map[string]struct{})
	questionByToken := make(map[string]string)
	outcomeByToken := make(map[string]string)
	exprByToken := make(map[string]string)
	windowByToken := make(map[string]int)
	for _, m := range markets {
		for _, tok := range m.ParsedTokens() {
			if tok.TokenID == "" {
				continue
			}
			seen[tok.TokenID] = struct{}{}
			questionByToken[tok.TokenID] = m.Question
			outcomeByToken[tok.TokenID] = tok.Outcome
			exprByToken[tok.TokenID] = itemExpr[m.ID]
			windowByToken[tok.TokenID] = itemWindow[m.ID]
		}
	}

	result := make([]string, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	sort.Strings(result)
	return result, questionByToken, outcomeByToken, exprByToken, windowByToken, nil
}

// sameStringSlice returns true when two pre-sorted slices are identical.
func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameMaps returns true when two string→string maps have identical key-value pairs.
func sameMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// sameMapsInt returns true when two string→int maps have identical key-value pairs.
func sameMapsInt(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

type detectorEnv struct {
	windowSize      int
	sizeMultiplier  float64
	minAbsoluteSize float64
}

func loadDetectorEnv() (*detectorEnv, error) {
	cfg := &detectorEnv{
		windowSize:      20,
		sizeMultiplier:  3.0,
		minAbsoluteSize: 10000,
	}

	if v := os.Getenv("POLYMARKET_WINDOW_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("POLYMARKET_WINDOW_SIZE must be a positive integer, got %q", v)
		}
		cfg.windowSize = n
	}

	if v := os.Getenv("POLYMARKET_SIZE_MULTIPLIER"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 {
			return nil, fmt.Errorf("POLYMARKET_SIZE_MULTIPLIER must be a positive number, got %q", v)
		}
		cfg.sizeMultiplier = f
	}

	if v := os.Getenv("POLYMARKET_MIN_ABSOLUTE_SIZE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 {
			return nil, fmt.Errorf("POLYMARKET_MIN_ABSOLUTE_SIZE must be a positive number, got %q", v)
		}
		cfg.minAbsoluteSize = f
	}

	return cfg, nil
}

func loadPollInterval() time.Duration {
	if v := os.Getenv("POLYMARKET_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil && d > 0 {
			return d
		}
	}
	return 30 * time.Second
}

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
	defer rc.Close()

	detCfg, err := loadDetectorEnv()
	if err != nil {
		return fmt.Errorf("polymarket-watcher: %w", err)
	}

	client, err := polymarket.New(polymarket.ClientConfig{})
	if err != nil {
		return fmt.Errorf("polymarket-watcher: %w", err)
	}
	defer client.Close()

	mc, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		return fmt.Errorf("polymarket-watcher: connect mongodb: %w", err)
	}
	defer mc.Disconnect(ctx)

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
		subCtx          context.Context
		cancelSub       context.CancelFunc
		tradeCh         <-chan ws.LastTradePriceEvent
		activeTokenIDs  []string
		questionByToken map[string]string // assetID → market question
		outcomeByToken  map[string]string // assetID → outcome label (e.g. "Yes"/"No")
	)

	fetchWatchlist := func() ([]string, map[string]string, map[string]string) {
		ids, qmap, omap, err := clobTokenIDsFromWatchlist(ctx, wlRepo, client)
		if err != nil {
			slog.Error("polymarket watcher: fetch watchlist token IDs", "err", err)
			return nil, nil, nil
		}
		return ids, qmap, omap
	}

	restartSub := func(tokenIDs []string, qmap map[string]string, omap map[string]string) {
		if cancelSub != nil {
			cancelSub()
			cancelSub = nil
		}
		tradeCh = nil
		activeTokenIDs = nil

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
			newIDs, newQmap, newOmap := fetchWatchlist()
			if !sameStringSlice(activeTokenIDs, newIDs) {
				slog.Info("polymarket watcher: watchlist changed, restarting subscription",
					"old_count", len(activeTokenIDs),
					"new_count", len(newIDs),
				)
				restartSub(newIDs, newQmap, newOmap)
			}

		case event, ok := <-tradeCh: // nil when idle — never fires
			if !ok {
				slog.Warn("polymarket watcher: trade stream closed, reconnecting immediately")
				restartSub(fetchWatchlist())
				continue
			}

			unusual, detected := det.Process(event)

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
func clobTokenIDsFromWatchlist(ctx context.Context, wlRepo *mongodb.WatchlistRepository, client *polymarket.Client) ([]string, map[string]string, map[string]string, error) {
	items, err := wlRepo.List(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list watchlist: %w", err)
	}
	if len(items) == 0 {
		return nil, nil, nil, nil
	}

	marketIDs := make([]string, len(items))
	for i, item := range items {
		marketIDs[i] = item.MarketID
	}

	markets, err := client.GetMarkets(ctx, &gamma.MarketsRequest{IDs: marketIDs})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get markets: %w", err)
	}

	seen := make(map[string]struct{})
	questionByToken := make(map[string]string)
	outcomeByToken := make(map[string]string)
	for _, m := range markets {
		for _, tok := range m.ParsedTokens() {
			if tok.TokenID == "" {
				continue
			}
			seen[tok.TokenID] = struct{}{}
			questionByToken[tok.TokenID] = m.Question
			outcomeByToken[tok.TokenID] = tok.Outcome
		}
	}

	result := make([]string, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	sort.Strings(result)
	return result, questionByToken, outcomeByToken, nil
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

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/options"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/schwab"
)

// SchwabWatcherWorker monitors option chains for symbols in the options watchlist
// and flags unusual prints via the options.Detector.
var SchwabWatcherWorker = &schwabWatcher{}

type schwabWatcher struct{}

func (w *schwabWatcher) Name() string { return "schwab-watcher" }

func (w *schwabWatcher) Run(ctx context.Context) error {
	cfg, err := config.Load(config.WithDotEnv(".env"))
	if err != nil {
		return fmt.Errorf("schwab-watcher: load config: %w", err)
	}

	rc := rediss.New(cfg.Redis)
	defer func() {
		if err := rc.Close(); err != nil {
			slog.Error("schwab-watcher: close redis", "err", err)
		}
	}()

	mc, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		return fmt.Errorf("schwab-watcher: connect mongodb: %w", err)
	}
	defer func() {
		if err := mc.Disconnect(ctx); err != nil {
			slog.Error("schwab-watcher: disconnect mongodb", "err", err)
		}
	}()

	tokens := schwab.NewTokenManager(cfg.Schwab, mc.DB())
	if err := tokens.Load(ctx); err != nil {
		slog.Warn("schwab-watcher: load tokens failed (not yet authorized?)", "err", err)
	}
	tokens.RunRefresher(ctx)

	client := schwab.NewClient(tokens)
	wlRepo := mongodb.NewOptionsWatchlistRepository(mc.DB())
	det := options.NewDetector()

	// Reset session volumes daily at market open (9:30 ET).
	go runDailyReset(ctx, det)

	pollInterval := 5 * time.Minute
	pollTicker := time.NewTicker(pollInterval)
	defer pollTicker.Stop()

	var (
		subCtx      context.Context
		cancelSub   context.CancelFunc
		tradeCh     <-chan options.Trade
		closeCh     <-chan error
		reconnectCh <-chan time.Time
		oiBySymbol  map[string]int64
	)
	bo := &StreamBackoff{}

	buildAndSubscribe := func() {
		if cancelSub != nil {
			cancelSub()
			cancelSub = nil
		}
		tradeCh = nil
		closeCh = nil
		oiBySymbol = nil

		if !isMarketHours() {
			slog.Info("schwab-watcher: outside market hours, idle until next poll")
			return
		}

		items, err := wlRepo.List(ctx)
		if err != nil {
			slog.Error("schwab-watcher: list watchlist", "err", err)
			return
		}
		if len(items) == 0 {
			slog.Info("schwab-watcher: watchlist empty, idle")
			return
		}

		// Fetch chains for all watched underlyings.
		newOI := make(map[string]int64)
		var contractSymbols []string
		for _, item := range items {
			contracts, err := client.GetChain(ctx, item.Symbol, "")
			if err != nil {
				slog.Error("schwab-watcher: get chain", "symbol", item.Symbol, "err", err)
				continue
			}
			for _, c := range contracts {
				if c.Symbol == "" {
					continue
				}
				contractSymbols = append(contractSymbols, c.Symbol)
				newOI[c.Symbol] = c.OI
			}
		}

		if len(contractSymbols) == 0 {
			slog.Info("schwab-watcher: no contracts found")
			return
		}

		sort.Strings(contractSymbols)
		oiBySymbol = newOI

		subCtx, cancelSub = context.WithCancel(ctx)
		ch, closeC, err := client.StreamTradesEx(subCtx, contractSymbols)
		if err != nil {
			cancelSub()
			cancelSub = nil
			slog.Error("schwab-watcher: stream trades", "err", err)
			return
		}
		tradeCh = ch
		closeCh = closeC
		slog.Info("schwab-watcher: subscribed", "contracts", len(contractSymbols))
	}

	buildAndSubscribe()

	for {
		select {
		case <-ctx.Done():
			if cancelSub != nil {
				cancelSub()
			}
			slog.Info("schwab-watcher: stopped")
			return nil

		case <-pollTicker.C:
			buildAndSubscribe()

		case <-reconnectCh: // backoff-delayed reconnect
			reconnectCh = nil
			buildAndSubscribe()

		case trade, ok := <-tradeCh: // nil channel never fires when idle
			if !ok {
				var closeErr error
				if closeCh != nil {
					select {
					case closeErr = <-closeCh:
					default:
					}
				}
				tradeCh = nil
				closeCh = nil
				bd := bo.Next(closeErr)
				slog.Info("schwab-watcher: stream closed", "backoff", bd, "reason", closeErr, "attempt", bo.consecutive)
				reconnectCh = time.After(bd)
				continue
			}
			bo.Reset()

			oi := oiBySymbol[trade.Symbol]
			result := det.Process(trade, oi)

			event := options.OptionsEvent{
				Symbol:        result.Trade.Symbol,
				Underlying:    result.Trade.Underlying,
				Strike:        result.Trade.Strike,
				Expiration:    result.Trade.Expiration,
				Type:          result.Trade.Type,
				Price:         result.Trade.Price,
				Size:          result.Trade.Size,
				Unusual:       result.Unusual,
				Reason:        result.Reason,
				VolumeOIRatio: result.VolumeOIRatio,
				DetectedAt:    time.Now().UTC(),
			}

			if result.Unusual {
				slog.Warn("schwab-watcher: unusual options print",
					"symbol", event.Symbol,
					"underlying", event.Underlying,
					"type", event.Type,
					"strike", event.Strike,
					"price", event.Price,
					"size", event.Size,
					"reason", event.Reason,
				)
			}

			payload, err := json.Marshal(event)
			if err != nil {
				slog.Error("schwab-watcher: marshal event", "err", err)
				continue
			}
			if err := rc.Publish(ctx, rediss.OptionsChannel, payload); err != nil {
				slog.Error("schwab-watcher: publish event", "err", err)
			}
		}
	}
}

// isMarketHours returns true if current time is within regular US equity market hours
// (Mon–Fri 9:30–16:00 ET). Does not account for holidays.
func isMarketHours() bool {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return true // fail open
	}
	now := time.Now().In(loc)
	wd := now.Weekday()
	if wd == time.Saturday || wd == time.Sunday {
		return false
	}
	open := time.Date(now.Year(), now.Month(), now.Day(), 9, 30, 0, 0, loc)
	close := time.Date(now.Year(), now.Month(), now.Day(), 16, 0, 0, 0, loc)
	return now.After(open) && now.Before(close)
}

// runDailyReset resets cumulative volume at 9:30am ET each trading day.
func runDailyReset(ctx context.Context, det *options.Detector) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		slog.Error("schwab-watcher: load ET timezone", "err", err)
		return
	}
	for {
		now := time.Now().In(loc)
		next := time.Date(now.Year(), now.Month(), now.Day(), 9, 30, 0, 0, loc)
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
			det.ResetVolumes()
			slog.Info("schwab-watcher: reset session volumes at market open")
		}
	}
}

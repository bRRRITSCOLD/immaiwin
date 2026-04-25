package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/futures"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/schwab"
)

// SchwabFuturesWatcherWorker monitors futures contracts for symbols in the futures watchlist.
var SchwabFuturesWatcherWorker = &schwabFuturesWatcher{}

type schwabFuturesWatcher struct{}

func (w *schwabFuturesWatcher) Name() string { return "schwab-futures-watcher" }

func (w *schwabFuturesWatcher) Run(ctx context.Context) error {
	cfg, err := config.Load(config.WithDotEnv(".env"))
	if err != nil {
		return fmt.Errorf("schwab-futures-watcher: load config: %w", err)
	}

	rc := rediss.New(cfg.Redis)
	defer func() {
		if err := rc.Close(); err != nil {
			slog.Error("schwab-futures-watcher: close redis", "err", err)
		}
	}()

	mc, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		return fmt.Errorf("schwab-futures-watcher: connect mongodb: %w", err)
	}
	defer func() {
		if err := mc.Disconnect(ctx); err != nil {
			slog.Error("schwab-futures-watcher: disconnect mongodb", "err", err)
		}
	}()

	tokens := schwab.NewTokenManager(cfg.Schwab, mc.DB())
	if err := tokens.Load(ctx); err != nil {
		slog.Warn("schwab-futures-watcher: load tokens failed (not yet authorized?)", "err", err)
	}
	tokens.RunRefresher(ctx)

	client := schwab.NewClient(tokens)
	wlRepo := mongodb.NewFuturesWatchlistRepository(mc.DB())

	pollInterval := 5 * time.Minute
	pollTicker := time.NewTicker(pollInterval)
	defer pollTicker.Stop()

	var (
		subCtx      context.Context
		cancelSub   context.CancelFunc
		tradeCh     <-chan futures.Trade
		closeCh     <-chan error
		reconnectCh <-chan time.Time
	)
	bo := &StreamBackoff{}

	buildAndSubscribe := func() {
		if cancelSub != nil {
			cancelSub()
			cancelSub = nil
		}
		tradeCh = nil
		closeCh = nil

		items, err := wlRepo.List(ctx)
		if err != nil {
			slog.Error("schwab-futures-watcher: list watchlist", "err", err)
			return
		}
		if len(items) == 0 {
			slog.Info("schwab-futures-watcher: watchlist empty, idle")
			return
		}

		roots := make([]string, len(items))
		for i, item := range items {
			roots[i] = item.Symbol
		}

		// Resolve root symbols to active front-month contracts.
		resolved, err := client.ResolveFuturesSymbols(ctx, roots)
		if err != nil {
			slog.Error("schwab-futures-watcher: resolve symbols", "err", err)
			return
		}

		var contractSymbols []string
		for _, root := range roots {
			if sym, ok := resolved[root]; ok {
				slog.Info("schwab-futures-watcher: resolved", "root", root, "contract", sym)
				contractSymbols = append(contractSymbols, sym)
			} else {
				// Streamer may accept root symbol directly; fall back.
				slog.Warn("schwab-futures-watcher: resolution failed, using root", "root", root)
				contractSymbols = append(contractSymbols, root)
			}
		}

		if len(contractSymbols) == 0 {
			slog.Info("schwab-futures-watcher: no contracts to subscribe")
			return
		}

		subCtx, cancelSub = context.WithCancel(ctx)
		ch, closeC, err := client.StreamFuturesEx(subCtx, contractSymbols)
		if err != nil {
			cancelSub()
			cancelSub = nil
			slog.Error("schwab-futures-watcher: stream futures", "err", err)
			return
		}
		tradeCh = ch
		closeCh = closeC
		slog.Info("schwab-futures-watcher: subscribed", "contracts", contractSymbols)
	}

	buildAndSubscribe()

	for {
		select {
		case <-ctx.Done():
			if cancelSub != nil {
				cancelSub()
			}
			slog.Info("schwab-futures-watcher: stopped")
			return nil

		case <-pollTicker.C:
			buildAndSubscribe()

		case <-reconnectCh:
			reconnectCh = nil
			buildAndSubscribe()

		case trade, ok := <-tradeCh:
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
				slog.Info("schwab-futures-watcher: stream closed", "backoff", bd, "reason", closeErr, "attempt", bo.consecutive)
				reconnectCh = time.After(bd)
				continue
			}
			bo.Reset()

			event := futures.FuturesEvent{
				Symbol:     trade.Symbol,
				Root:       trade.Root,
				Price:      trade.Price,
				Size:       trade.Size,
				Volume:     trade.Volume,
				OI:         trade.OI,
				DetectedAt: time.Now().UTC(),
			}

			payload, err := json.Marshal(event)
			if err != nil {
				slog.Error("schwab-futures-watcher: marshal event", "err", err)
				continue
			}
			if err := rc.Publish(ctx, rediss.FuturesChannel, payload); err != nil {
				slog.Error("schwab-futures-watcher: publish event", "err", err)
			}
		}
	}
}


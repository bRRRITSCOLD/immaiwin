// backfill-trade-questions resolves market_question and token_outcome for trades missing them.
//
// Fetches all trades from MongoDB where these fields are empty, batches their asset_ids
// to the Polymarket Gamma API using clob_token_ids, then updates the records in place.
//
// Usage:
//
//	go run ./scripts/backfill-trade-questions
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/GoPolymarket/polymarket-go-sdk/pkg/gamma"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/polymarket"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(config.WithDotEnv(".env"))
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	mc, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		slog.Error("connect mongodb", "err", err)
		os.Exit(1)
	}
	defer mc.Disconnect(ctx)

	repo := mongodb.NewTradeRepository(mc.DB())

	pm, err := polymarket.New(polymarket.ClientConfig{})
	if err != nil {
		slog.Error("create polymarket client", "err", err)
		os.Exit(1)
	}
	defer pm.Close()

	// Collect all trades missing question OR outcome
	missingQ, err := repo.ListMissingQuestion(ctx)
	if err != nil {
		slog.Error("list missing question trades", "err", err)
		os.Exit(1)
	}
	missingO, err := repo.ListMissingOutcome(ctx)
	if err != nil {
		slog.Error("list missing outcome trades", "err", err)
		os.Exit(1)
	}

	// Union of unique asset_ids needing any backfill
	seen := make(map[string]struct{})
	var assetIDs []string
	for _, t := range append(missingQ, missingO...) {
		if _, ok := seen[t.AssetID]; !ok {
			seen[t.AssetID] = struct{}{}
			assetIDs = append(assetIDs, t.AssetID)
		}
	}

	if len(assetIDs) == 0 {
		slog.Info("no trades need backfill")
		return
	}
	slog.Info("unique asset_ids to resolve", "count", len(assetIDs))

	// Build tokenID → {question, outcome} via Polymarket, 100 token IDs per batch
	type meta struct{ question, outcome string }
	tokenMeta := make(map[string]meta)

	const batchSize = 100
	for i := 0; i < len(assetIDs); i += batchSize {
		end := i + batchSize
		if end > len(assetIDs) {
			end = len(assetIDs)
		}
		batch := assetIDs[i:end]

		markets, err := pm.GetMarkets(ctx, &gamma.MarketsRequest{
			ClobTokenIDs: batch,
		})
		if err != nil {
			slog.Error("get markets batch", "start", i, "err", err)
			continue
		}

		for _, m := range markets {
			if m.Question == "" {
				continue
			}
			for _, tok := range m.ParsedTokens() {
				if tok.TokenID != "" {
					tokenMeta[tok.TokenID] = meta{question: m.Question, outcome: tok.Outcome}
				}
			}
		}
		slog.Info("batch resolved", "batch_start", i, "markets_returned", len(markets))
	}

	// Update MongoDB
	updated, skipped := 0, 0
	for _, assetID := range assetIDs {
		m, ok := tokenMeta[assetID]
		if !ok {
			slog.Warn("no market found for asset_id", "asset_id", assetID)
			skipped++
			continue
		}
		if err := repo.UpdateTradeMetadata(ctx, assetID, m.question, m.outcome); err != nil {
			slog.Error("update trade metadata", "asset_id", assetID, "err", err)
			continue
		}
		slog.Info("updated", "asset_id", assetID, "question", m.question, "outcome", m.outcome)
		updated++
	}

	slog.Info("backfill complete", "updated", updated, "skipped", skipped)
}

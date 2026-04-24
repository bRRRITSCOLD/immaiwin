// backfill-watchlist-defaults sets unusual_expr and window_size on watchlist items
// that are missing them (added before these fields were introduced).
//
// Safe to run multiple times — only updates documents where fields are missing/zero.
//
// Usage:
//
//	go run ./scripts/backfill-watchlist-defaults
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/watchlist"
	"go.mongodb.org/mongo-driver/v2/bson"
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

	col := mc.DB().Collection("watchlist")

	// Only touch documents missing unusual_expr or window_size.
	filter := bson.M{
		"$or": bson.A{
			bson.M{"unusual_expr": bson.M{"$exists": false}},
			bson.M{"unusual_expr": ""},
			bson.M{"window_size": bson.M{"$exists": false}},
			bson.M{"window_size": 0},
		},
	}

	res, err := col.UpdateMany(
		ctx,
		filter,
		bson.M{"$set": bson.M{
			"unusual_expr": watchlist.DefaultExpr,
			"window_size":  watchlist.DefaultWindowSize,
		}},
	)
	if err != nil {
		slog.Error("update watchlist items", "err", err)
		os.Exit(1)
	}

	slog.Info("backfill complete",
		"matched", res.MatchedCount,
		"modified", res.ModifiedCount,
		"unusual_expr", watchlist.DefaultExpr,
		"window_size", watchlist.DefaultWindowSize,
	)
}

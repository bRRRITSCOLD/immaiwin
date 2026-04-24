package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/rediss"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/trade"
)

// MongoDBWriterWorker subscribes to the Redis trades channel and persists each event to MongoDB.
var MongoDBWriterWorker = &mongoDBWriter{}

type mongoDBWriter struct{}

func (w *mongoDBWriter) Name() string { return "mongodb-writer" }

func (w *mongoDBWriter) Run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("mongodb-writer: load config: %w", err)
	}

	rc := rediss.New(cfg.Redis)
	defer rc.Close()

	mongoClient, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		return fmt.Errorf("mongodb-writer: connect mongodb: %w", err)
	}
	defer mongoClient.Disconnect(ctx)

	repo := mongodb.NewTradeRepository(mongoClient.DB())

	sub := rc.Subscribe(ctx, rediss.TradesChannel)
	defer sub.Close()

	slog.Info("mongodb writer started")

	for {
		select {
		case <-ctx.Done():
			slog.Info("mongodb writer stopped")
			return nil
		case msg, ok := <-sub.Channel():
			if !ok {
				return fmt.Errorf("mongodb-writer: redis subscription closed")
			}
			var event trade.TradeEvent
			if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
				slog.Error("mongodb-writer: parse trade event", "err", err)
				continue
			}
			if !event.Unusual {
				continue
			}
			t := &trade.Trade{
				AssetID:        event.AssetID,
				Market:         event.Market,
				MarketQuestion: event.MarketQuestion,
				TokenOutcome:   event.TokenOutcome,
				Price:          event.Price,
				Size:           event.Size,
				Side:           event.Side,
				FeeRateBps:     event.FeeRateBps,
				Timestamp:      event.Timestamp,
				RollingAvgSize: event.RollingAvgSize,
				Reason:         event.Reason,
				DetectedAt:     event.DetectedAt,
			}
			if _, err := repo.InsertOne(ctx, t); err != nil {
				slog.Error("mongodb-writer: insert trade", "asset_id", event.AssetID, "err", err)
			} else {
				slog.Info("trade persisted", "asset_id", event.AssetID, "reason", event.Reason)
			}
		}
	}
}

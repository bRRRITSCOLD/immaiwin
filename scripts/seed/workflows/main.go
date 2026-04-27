//go:build ignore

// Seed workflows from immaiwin.workflows.json — idempotent (deletes + re-inserts by name).
//
// Usage:
//
//	go run ./scripts/seed/workflows/main.go

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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
	defer func() {
		if err := mc.Disconnect(ctx); err != nil {
			slog.Error("disconnect mongodb", "err", err)
		}
	}()

	// Read JSON file (MongoDB extended JSON format)
	jsonData, err := os.ReadFile("immaiwin.workflows.json")
	if err != nil {
		slog.Error("read workflows json", "err", err)
		os.Exit(1)
	}

	// Parse top-level array, then unmarshal each element as extended JSON
	var rawDocs []json.RawMessage
	if err := json.Unmarshal(jsonData, &rawDocs); err != nil {
		slog.Error("parse json array", "err", err)
		os.Exit(1)
	}

	if len(rawDocs) == 0 {
		fmt.Println("no workflows in JSON file")
		os.Exit(0)
	}

	wfCol := mc.DB().Collection("workflows")
	seeded := 0

	for _, raw := range rawDocs {
		var doc bson.M
		if err := bson.UnmarshalExtJSON(raw, false, &doc); err != nil {
			slog.Error("unmarshal ext json", "err", err)
			continue
		}

		name, _ := doc["name"].(string)
		if name == "" {
			slog.Warn("skipping workflow with empty name")
			continue
		}

		// Delete existing by name (idempotent refresh)
		_, _ = wfCol.DeleteOne(ctx, bson.M{"name": name})

		// Refresh timestamps
		now := time.Now().UTC()
		doc["created_at"] = now
		doc["updated_at"] = now

		_, err := wfCol.InsertOne(ctx, doc)
		if err != nil {
			slog.Error("insert workflow", "name", name, "err", err)
			continue
		}
		fmt.Printf("seeded: %s (id=%v)\n", name, doc["_id"])
		seeded++
	}

	fmt.Printf("done: %d seeded\n", seeded)
}

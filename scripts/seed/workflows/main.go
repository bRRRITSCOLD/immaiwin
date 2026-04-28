//go:build ignore

// Seed workflows from every *.json file in the same directory as this script.
// Each file may hold either a single workflow document (object) or an array of
// workflow documents. Idempotent — deletes existing docs by name before insert.
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
	"path/filepath"
	"runtime"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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

	// Resolve the directory this script lives in. Seeds *.json files in
	// that directory and nowhere else, regardless of where `go run` is
	// invoked from.
	scriptDir, err := scriptDirectory()
	if err != nil {
		slog.Error("resolve script directory", "err", err)
		os.Exit(1)
	}

	jsonFiles, err := filepath.Glob(filepath.Join(scriptDir, "*.json"))
	if err != nil {
		slog.Error("glob json files", "dir", scriptDir, "err", err)
		os.Exit(1)
	}
	if len(jsonFiles) == 0 {
		fmt.Printf("no *.json files in %s — nothing to seed\n", scriptDir)
		return
	}

	wfCol := mc.DB().Collection("workflows")
	totalSeeded := 0

	for _, path := range jsonFiles {
		seeded, err := seedFile(ctx, wfCol, path)
		if err != nil {
			slog.Error("seed file", "path", path, "err", err)
			continue
		}
		fmt.Printf("==> %s — seeded %d\n", filepath.Base(path), seeded)
		totalSeeded += seeded
	}

	fmt.Printf("done: %d workflow(s) across %d file(s)\n", totalSeeded, len(jsonFiles))
}

// scriptDirectory returns the directory containing this main.go via the Go
// runtime caller info. Avoids relying on the user's working directory.
func scriptDirectory() (string, error) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller(0) unavailable")
	}
	return filepath.Dir(filename), nil
}

// seedFile reads one JSON file and seeds every workflow it contains. The file
// may hold a top-level array OR a single object.
func seedFile(ctx context.Context, wfCol *mongo.Collection, path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read: %w", err)
	}

	rawDocs, err := splitTopLevel(data)
	if err != nil {
		return 0, fmt.Errorf("parse: %w", err)
	}

	seeded := 0
	for i, raw := range rawDocs {
		// Parse as plain JSON first so nested objects decode as
		// map[string]any (bson.UnmarshalExtJSON would give bson.D for
		// nested objects, breaking the envelope peel below).
		var top map[string]any
		if err := json.Unmarshal(raw, &top); err != nil {
			slog.Warn("unmarshal json", "path", path, "index", i, "err", err)
			continue
		}

		// Peel a `{ "version": N, "workflow": {...} }` envelope if present.
		if inner, ok := top["workflow"].(map[string]any); ok {
			top = inner
		}

		// API-style export uses `id` for the workflow ID; Mongo stores it
		// at `_id`. Translate so seeding preserves stable IDs across runs.
		if id, ok := top["id"].(string); ok && id != "" {
			top["_id"] = id
			delete(top, "id")
		}

		doc := bson.M(top)

		name, _ := doc["name"].(string)
		if name == "" {
			slog.Warn("skipping workflow with empty name", "path", path, "index", i)
			continue
		}

		if _, err := wfCol.DeleteOne(ctx, bson.M{"name": name}); err != nil {
			slog.Warn("delete existing", "name", name, "err", err)
		}

		now := time.Now().UTC()
		doc["created_at"] = now
		doc["updated_at"] = now

		if _, err := wfCol.InsertOne(ctx, doc); err != nil {
			slog.Warn("insert workflow", "name", name, "err", err)
			continue
		}
		fmt.Printf("    seeded: %s (id=%v)\n", name, doc["_id"])
		seeded++
	}
	return seeded, nil
}

// splitTopLevel returns one json.RawMessage per workflow document, regardless
// of whether the file's top-level value is an array or a single object.
func splitTopLevel(data []byte) ([]json.RawMessage, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err == nil {
		return arr, nil
	}
	var obj json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("not an array or object: %w", err)
	}
	return []json.RawMessage{obj}, nil
}


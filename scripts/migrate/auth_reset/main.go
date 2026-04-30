//go:build ignore

// Auth + multi-tenancy reset migration.
//
// Phase B introduced tenant_id on Workflow / Connection / WorkflowRun /
// Eval. Existing dev data was created without those fields and would
// be inaccessible (every store filters by tenant_id when authed).
//
// This script drops user-scope collections so the next dev signup
// starts fresh under the new schema. Bundled assets (templates via
// go:embed, skill registry as global platform resource) are NOT
// dropped — every new tenant inherits them out of the gate.
//
// Usage:
//
//	go run ./scripts/migrate/auth_reset/main.go
//
// Confirms before destructive ops; pass --yes to skip the prompt.

package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/config"
	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
)

// dropCollections lists every per-user collection that should be
// reset. Skill registry (`skill_registry`) intentionally NOT here —
// platform-global. Templates aren't in Mongo at all (embedded JSON).
var dropCollections = []string{
	"workflows",
	"workflow_runs",
	"workflow_connections",
	"evals",
	"eval_runs",
	"chat_memory",
	"users",
	"tenants",
	"tenant_members",
	"api_keys",
}

func main() {
	skipConfirm := flag.Bool("yes", false, "skip confirmation prompt")
	flag.Parse()

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
	defer func() { _ = mc.Disconnect(ctx) }()

	db := mc.DB()

	fmt.Printf("Database: %s\n", db.Name())
	fmt.Println("Collections to DROP:")
	for _, c := range dropCollections {
		fmt.Printf("  - %s\n", c)
	}
	fmt.Println()
	fmt.Println("Preserved (platform-global):")
	fmt.Println("  - skill_registry")
	fmt.Println()

	if !*skipConfirm {
		fmt.Print("Type 'YES' to confirm: ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		if strings.TrimSpace(line) != "YES" {
			fmt.Println("Aborted.")
			return
		}
	}

	for _, name := range dropCollections {
		if err := db.Collection(name).Drop(ctx); err != nil {
			slog.Warn("drop failed (may not exist)", "col", name, "err", err)
			continue
		}
		fmt.Printf("dropped %s\n", name)
	}
	fmt.Println("done. Restart api + register a fresh user.")
}

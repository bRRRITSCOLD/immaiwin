//go:build ignore

package main

import (
	"log/slog"
	"os"
	"os/exec"
)

func main() {
	if len(os.Args) < 2 {
		slog.Error("missing command", "usage", "go run scripts/docker-compose/main.go [up|down]")
		os.Exit(1)
	}

	command := os.Args[1]
	var args []string

	switch command {
	case "up":
		args = []string{"up", "-d"}
	case "down":
		args = []string{"down"}
	default:
		slog.Error("unknown command", "command", command, "usage", "go run scripts/docker-compose/main.go [up|down]")
		os.Exit(1)
	}

	cmd := exec.Command("docker-compose", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	slog.Info("running docker-compose", "args", args)
	if err := cmd.Run(); err != nil {
		slog.Error("docker-compose command failed", "err", err)
		os.Exit(1)
	}
}

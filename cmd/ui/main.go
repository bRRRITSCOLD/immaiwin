package main

import (
	"os"
	"os/exec"
)

func main() {
	args := []string{"internal", "&&", "cd", "ui", "&&", "pnpm", "run", "dev"}

	cmd := exec.Command("cd", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
}

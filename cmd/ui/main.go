package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	dev := flag.Bool("dev", false, "run the dev server")
	flag.Parse()

	if *dev {
		cmd := exec.Command("pnpm", "run", "dev")
		cmd.Dir = filepath.Join("internal", "ui")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			os.Exit(1)
		}
	}
}

//go:build ignore

// Test runner entry point. Default = unit only (no tier build tag).
// Pass `-tier=integration` or `-tier=e2e` to compile + run the tagged
// suites. `-tier=all` runs unit, integration, and e2e in sequence.
//
// Tiered suites are gated by Go build tags:
//   *_test.go              — no tag, default build (unit)
//   *_integration_test.go  — //go:build integration
//   *_e2e_test.go          — //go:build e2e
//
// Integration / e2e suites fail loud if their service deps are
// unreachable. There is no skip path. Run docker compose up before
// invoking the tagged tiers.

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

func main() {
	tier := flag.String("tier", "unit", "test tier to run: unit | integration | e2e | all")
	flag.Parse()

	tiers := []string{*tier}
	if *tier == "all" {
		tiers = []string{"unit", "integration", "e2e"}
	}

	for _, t := range tiers {
		args := []string{"test", "-count=1"}
		if runtime.GOOS != "windows" {
			args = append(args, "-race")
		}
		switch t {
		case "unit":
			// no extra tag
		case "integration":
			args = append(args, "-tags=integration")
		case "e2e":
			args = append(args, "-tags=e2e")
		default:
			fmt.Fprintf(os.Stderr, "unknown tier %q (expected unit | integration | e2e | all)\n", t)
			os.Exit(2)
		}
		args = append(args, "./...")

		fmt.Fprintf(os.Stderr, "==> tier=%s args=%v\n", t, args)
		cmd := exec.Command("go", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			os.Exit(1)
		}
	}
}

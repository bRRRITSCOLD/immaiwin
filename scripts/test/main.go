//go:build ignore

package main

import (
	"os"
	"os/exec"
	"runtime"
)

func main() {
	args := []string{"test", "-count=1"}
	if runtime.GOOS != "windows" {
		args = append(args, "-race")
	}
	args = append(args, "./...")

	cmd := exec.Command("go", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
}

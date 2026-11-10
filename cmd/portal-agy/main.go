package main

import (
	"cliproxy-portal/internal/agy"
	"log/slog"
	"os"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--official-tool-policy" {
		if agy.RunOfficialToolPolicy(os.Stdin, os.Stdout) != nil {
			os.Exit(1)
		}
		return
	}
	if err := agy.RunService(); err != nil {
		slog.Error("AGY service stopped", "error", err)
		os.Exit(1)
	}
}

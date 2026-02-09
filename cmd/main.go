package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/drewfead/pdx-watcher/internal/root"
)

func main() {
	ctx := context.Background()

	rootCmd, err := root.Root(ctx)
	if err != nil {
		slog.Error("failed to create root command", "error", err)
		os.Exit(137)
	}

	if err := rootCmd.Run(ctx, os.Args); err != nil {
		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}

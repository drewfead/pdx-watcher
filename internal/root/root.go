package root

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/drewfead/pdx-watcher/internal"
	"github.com/drewfead/pdx-watcher/internal/enrichment"
	"github.com/drewfead/pdx-watcher/internal/scraper"
	"github.com/drewfead/pdx-watcher/internal/services"
	"github.com/drewfead/pdx-watcher/proto"
	protocli "github.com/drewfead/proto-cli"
	"github.com/urfave/cli/v3"
	protobuf "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// syncWriter wraps an *os.File and calls Sync after each Write so streamed output
// (e.g. list-showtimes to stdout) appears immediately on Windows.
type syncWriter struct {
	f *os.File
}

func (w *syncWriter) Write(p []byte) (n int, err error) {
	n, err = w.f.Write(p)
	if err != nil {
		return n, err
	}
	_ = w.f.Sync()
	return n, nil
}

// RootOption configures the root command (e.g. for tests).
type RootOption func(*rootConfig)

type rootConfig struct {
	registry scraper.Registry
}

// WithRegistry sets the scraper registry. Use in tests to inject a registry that uses
// golden HTTP servers or mocks instead of the default (Rod) scraper.
func WithRegistry(registry scraper.Registry) RootOption {
	return func(c *rootConfig) {
		c.registry = registry
	}
}

func Root(ctx context.Context, opts ...RootOption) (*cli.Command, error) {
	cfg := &rootConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	// Create timestamp deserializer for all timestamp fields
	timestampDeserializer := func(ctx context.Context, flags protocli.FlagContainer) (protobuf.Message, error) {
		timeStr := flags.String()
		// If no timestamp provided, return empty timestamp (mapper will apply defaults)
		if timeStr == "" {
			return &timestamppb.Timestamp{}, nil
		}
		t, err := time.Parse(time.RFC3339, timeStr)
		if err != nil {
			return nil, fmt.Errorf("invalid timestamp format (expected RFC3339): %w", err)
		}
		return timestamppb.New(t), nil
	}

	registry := cfg.registry
	if registry == nil {
		registry = scraper.NewRegistry(
			scraper.WithScraperForSite(proto.PdxSite_HollywoodTheatre, scraper.HollywoodTheatre(), scraper.Cached(64, 5*time.Minute)),
		)
	}
	// Pass a factory so the CLI can create the service when --config is used (CallFactory expects a function that returns exactly one value).
	factory := func(cfg *proto.ShowtimeConfig) proto.ShowtimeServiceServer {
		var enrichmentProviders []internal.EnrichmentProvider
		if cfg != nil && cfg.Tmdb != nil && cfg.Tmdb.ApiKey != "" {
			tmdbClient, err := enrichment.TMDB(cfg.Tmdb.ApiKey)
			if err != nil {
				slog.Info("TMDB enrichment not configured", "reason", "client init failed", "error", err)
			} else {
				enrichmentProviders = append(enrichmentProviders, tmdbClient)
				slog.Info("TMDB enrichment configured")
			}
		} else {
			slog.Info("TMDB enrichment not configured", "reason", "no api_key or config")
		}
		return services.ShowtimesService(registry, enrichmentProviders...)
	}

	showtimesCLI := proto.ShowtimeServiceCommand(ctx, factory,
		protocli.WithOutputFormats(
			protocli.JSON(),
			protocli.YAML(),
		),
		protocli.WithFlagDeserializer("google.protobuf.Timestamp", timestampDeserializer),
	)

	// Create root command with config support
	rootCmd, err := protocli.RootCommand("pdx-watcher",
		protocli.Service(showtimesCLI, protocli.Hoisted()),
		protocli.WithEnvPrefix("PDX_WATCHER"),
	)
	if err != nil {
		slog.Error("failed to create root command", "error", err)
		return nil, fmt.Errorf("failed to create root command: %w", err)
	}

	// Use a syncing stdout so streamed output (e.g. list-showtimes) appears immediately,
	// especially on Windows where stdout may be buffered.
	rootCmd.Writer = &syncWriter{f: os.Stdout}

	return rootCmd, nil
}

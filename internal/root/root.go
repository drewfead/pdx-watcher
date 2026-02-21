package root

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"text/template"
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

// denseOutputFormat renders ListShowtimesResponse in a compact one-line format.
// It reads --timezone (or --output-timezone) from the command and displays times in that
// IANA timezone, or the CLI's local time if not set.
type denseOutputFormat struct {
	templateStr string
}

func (f *denseOutputFormat) Name() string { return "dense" }

func (f *denseOutputFormat) Format(ctx context.Context, cmd *cli.Command, w io.Writer, msg protobuf.Message) error {
	tzStr := cmd.String("output-timezone")
	if tzStr == "" {
		tzStr = cmd.String("timezone")
	}
	loc := time.Local
	if tzStr != "" {
		var err error
		loc, err = time.LoadLocation(tzStr)
		if err != nil {
			return fmt.Errorf("invalid --timezone %q: %w", tzStr, err)
		}
	}
	funcMap := template.FuncMap{}
	for k, v := range protocli.DefaultTemplateFunctions() {
		funcMap[k] = v
	}
	funcMap["shortTime"] = func(v any) string {
		if v == nil {
			return "-"
		}
		s, ok := v.(string)
		if !ok {
			return "-"
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return s
		}
		return t.In(loc).Format("Jan 02 03:04 PM")
	}
	const siteColumnWidth = 20 // pad so "hollywood-theatre" and "cinemagic" align in mixed output
	funcMap["padSite"] = func(s string) string {
		return fmt.Sprintf("%-*s", siteColumnWidth, s)
	}
	funcMap["orStr"] = func(a any, b string) string {
		if a == nil {
			return b
		}
		if s, ok := a.(string); ok && s != "" {
			return s
		}
		return b
	}
	// siteDisplay converts PdxSite from protoFields (number or enum name string) to CLI display string.
	funcMap["siteDisplay"] = func(v any) string {
		if v == nil {
			return "-"
		}
		switch x := v.(type) {
		case float64:
			switch proto.PdxSite(x) {
			case proto.PdxSite_HollywoodTheatre:
				return "hollywood-theatre"
			case proto.PdxSite_Cinemagic:
				return "cinemagic"
			case proto.PdxSite_Cinema21:
				return "cinema21"
			}
		case string:
			switch strings.ToLower(x) {
			case "hollywoodtheatre", "hollywood_theatre", "hollywood-theatre":
				return "hollywood-theatre"
			case "cinemagic":
				return "cinemagic"
			case "cinema21":
				return "cinema21"
			}
		}
		return "-"
	}
	tmpl, err := template.New("dense").Funcs(funcMap).Parse(f.templateStr)
	if err != nil {
		return fmt.Errorf("dense template: %w", err)
	}
	var buf bytes.Buffer
	data := map[string]any{"Message": msg}
	if err := tmpl.Execute(&buf, data); err != nil {
		return err
	}
	_, err = w.Write(buf.Bytes())
	return err
}

func Root(ctx context.Context, opts ...RootOption) (*cli.Command, error) {
	cfg := &rootConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	registry := cfg.registry
	if registry == nil {
		registry = scraper.NewRegistry(
			scraper.WithScraperForSite(proto.PdxSite_None, scraper.None()),
			scraper.WithScraperForSite(proto.PdxSite_HollywoodTheatre, scraper.HollywoodTheatre(), scraper.Cached(64, 5*time.Minute)),
			scraper.WithScraperForSite(proto.PdxSite_Cinemagic, scraper.Cinemagic(), scraper.Cached(64, 5*time.Minute)),
			scraper.WithScraperForSite(proto.PdxSite_Cinema21, scraper.Cinema21(), scraper.Cached(64, 5*time.Minute)),
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

	denseFormat := &denseOutputFormat{
		templateStr: `{{$f := protoFields .Message}}{{$s := $f.showtime}}{{shortTime $s.startTime}} | {{padSite (siteDisplay $f.site)}} | {{$s.summary}}`,
	}

	showtimesCLI := proto.ShowtimeServiceCommand(ctx, factory,
		protocli.WithOutputFormats(
			denseFormat,
			protocli.JSON(),
			protocli.YAML(),
		),
		protocli.WithFlagDeserializer("google.protobuf.Timestamp", timestampDeserializer),
		protocli.WithFlagDeserializer("showtimes.ListShowtimesRequest", listShowtimesRequestDeserializer),
	)

	// Create root command with config support
	rootCmd, err := protocli.RootCommand("pdx-watcher",
		protocli.Service(showtimesCLI, protocli.Hoisted()),
		protocli.WithEnvPrefix("PDX_WATCHER"),
		protocli.WithConfigManagementCommands(&proto.ShowtimeConfig{}, "pdx-watcher", "showtimeservice"),
	)
	if err != nil {
		slog.Error("failed to create root command", "error", err)
		return nil, fmt.Errorf("failed to create root command: %w", err)
	}

	return rootCmd, nil
}

func timestampDeserializer(ctx context.Context, flags protocli.FlagContainer) (protobuf.Message, error) {
	timeStr := flags.String()
	if timeStr == "" {
		return &timestamppb.Timestamp{}, nil
	}
	t, err := time.Parse(time.RFC3339, timeStr)
	if err != nil {
		return nil, fmt.Errorf("invalid timestamp format (expected RFC3339): %w", err)
	}
	return timestamppb.New(t), nil
}

// listShowtimesRequestDeserializer builds ListShowtimesRequest from flags.
// Supports multiple --from (StringSlice); omitted --from means "all" (handled by service).
func listShowtimesRequestDeserializer(ctx context.Context, flags protocli.FlagContainer) (protobuf.Message, error) {
	req := &proto.ListShowtimesRequest{}
	for _, s := range flags.StringSliceNamed("from") {
		site, err := parsePdxSite(s)
		if err != nil {
			return nil, err
		}
		req.From = append(req.From, site)
	}
	if afterStr := flags.StringNamed("after"); afterStr != "" {
		t, err := time.Parse(time.RFC3339, afterStr)
		if err != nil {
			return nil, fmt.Errorf("invalid --after (expected RFC3339): %w", err)
		}
		req.After = timestamppb.New(t)
	}
	if beforeStr := flags.StringNamed("before"); beforeStr != "" {
		t, err := time.Parse(time.RFC3339, beforeStr)
		if err != nil {
			return nil, fmt.Errorf("invalid --before (expected RFC3339): %w", err)
		}
		req.Before = timestamppb.New(t)
	}
	if flags.IsSetNamed("limit") {
		n := flags.IntNamed("limit")
		req.Limit = ptr(int32(n))
	}
	if anchor := flags.StringNamed("anchor"); anchor != "" {
		req.Anchor = &anchor
	}
	if tz := flags.StringNamed("output-timezone"); tz != "" {
		req.OutputTimezone = &tz
	} else if tz := flags.StringNamed("timezone"); tz != "" {
		req.OutputTimezone = &tz
	}
	return req, nil
}

func parsePdxSite(value string) (proto.PdxSite, error) {
	switch strings.ToLower(value) {
	case "hollywoodtheatre", "hollywood-theatre":
		return proto.PdxSite_HollywoodTheatre, nil
	case "cinemagic":
		return proto.PdxSite_Cinemagic, nil
	case "cinema21":
		return proto.PdxSite_Cinema21, nil
	}
	return 0, fmt.Errorf("invalid site %q (valid: hollywood-theatre, cinemagic, cinema21)", value)
}

func ptr[T any](v T) *T { return &v }

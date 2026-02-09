package acceptance

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/drewfead/pdx-watcher/internal"
	"github.com/drewfead/pdx-watcher/internal/root"
	"github.com/drewfead/pdx-watcher/internal/scraper"
	"github.com/drewfead/pdx-watcher/proto"
	"github.com/stretchr/testify/require"
)

func TestAcceptance_ListShowtimes(t *testing.T) {
	// Use golden HTTP server so we get deterministic output without Rod/live site.
	// Path is relative to the acceptance package directory (where go test runs).
	goldenDir := filepath.Join("..", "internal", "scraper", "golden", "hollywoodtheatre")
	ht := scraper.HollywoodTheatre()
	gs, ok := ht.(internal.GoldenScraper)
	require.True(t, ok, "HollywoodTheatre must implement GoldenScraper")
	handler, err := gs.MountGolden(t.Context(), goldenDir)
	require.NoError(t, err, "MountGolden")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	s := scraper.HollywoodTheatre(scraper.WithBaseURL(server.URL), scraper.WithClient(server.Client()))
	registry := scraper.NewRegistry(scraper.WithScraperForSite(proto.PdxSite_HollywoodTheatre, s))

	outputFile := filepath.Join(t.TempDir(), "output.json")

	rootCmd, err := root.Root(t.Context(), root.WithRegistry(registry))
	require.NoError(t, err, "Root")
	require.NotNil(t, rootCmd, "Root")

	// Golden data is Feb 2026; request that range so we get results.
	err = rootCmd.Run(t.Context(), []string{
		"pdx-watcher", "list-showtimes",
		"--from", "HollywoodTheatre",
		"--after", "2026-02-01T00:00:00Z",
		"--before", "2026-03-01T00:00:00Z",
		"--output", outputFile,
	})
	require.NoError(t, err, "Run")

	outputBytes, err := os.ReadFile(outputFile)
	require.NoError(t, err, "ReadFile")
	require.NotEmpty(t, outputBytes, "output file should contain showtimes from golden data")
	t.Log(string(outputBytes))
}

package scraper

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/drewfead/pdx-watcher/internal"
	"github.com/stretchr/testify/require"
)

var goldenScrapers = map[string]internal.GoldenScraper{
	"hollywoodtheatre": HollywoodTheatre().(internal.GoldenScraper),
}

const goldenDir = "golden"

func TestPrep_PullAllGolden(t *testing.T) {
	if os.Getenv("PREP") != "1" {
		t.Skip("PREP is not set")
	}

	for name, s := range goldenScrapers {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(goldenDir, name)
			err := s.PullGolden(t.Context(), dir)
			require.NoError(t, err, "PullGolden")
			t.Logf("wrote golden files to %s", dir)
		})
	}
}

func MountGoldenTestServer(t *testing.T, scraperName string) *httptest.Server {
	t.Helper()
	dir := filepath.Join(goldenDir, scraperName)
	s := goldenScrapers[scraperName]
	handler, err := s.MountGolden(t.Context(), dir)
	require.NoError(t, err, "MountGolden")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

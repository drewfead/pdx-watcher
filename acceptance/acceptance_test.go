package acceptance

import (
	"net/http"
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
	type siteCase struct {
		goldenDir string
		site      proto.PdxSite
		fromFlag  string
		golden    func() internal.GoldenScraper
		withTest  func(url string, client *http.Client) internal.Scraper
	}

	cases := []siteCase{
		{
			goldenDir: filepath.Join("..", "internal", "scraper", "golden", "hollywoodtheatre"),
			site:      proto.PdxSite_HollywoodTheatre,
			fromFlag:  "HollywoodTheatre",
			golden:    func() internal.GoldenScraper { gs, _ := scraper.HollywoodTheatre().(internal.GoldenScraper)
				return gs },
			withTest: func(url string, client *http.Client) internal.Scraper {
				return scraper.HollywoodTheatre(scraper.WithBaseURL(url), scraper.WithClient(client))
			},
		},
		{
			goldenDir: filepath.Join("..", "internal", "scraper", "golden", "cinemagic"),
			site:      proto.PdxSite_Cinemagic,
			fromFlag:  "Cinemagic",
			golden:    func() internal.GoldenScraper { gs, _ := scraper.Cinemagic().(internal.GoldenScraper)
				return gs },
			withTest: func(url string, client *http.Client) internal.Scraper {
				return scraper.Cinemagic(scraper.CinemagicWithBaseURL(url), scraper.CinemagicWithClient(client))
			},
		},
		{
			goldenDir: filepath.Join("..", "internal", "scraper", "golden", "cinema21"),
			site:      proto.PdxSite_Cinema21,
			fromFlag:  "Cinema21",
			golden: func() internal.GoldenScraper {
				gs, _ := scraper.Cinema21().(internal.GoldenScraper)
				return gs
			},
			withTest: func(url string, client *http.Client) internal.Scraper {
				return scraper.Cinema21(scraper.Cinema21WithBaseURL(url), scraper.Cinema21WithClient(client))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.fromFlag, func(t *testing.T) {
			gs := tc.golden()
			handler, err := gs.MountGolden(t.Context(), tc.goldenDir)
			require.NoError(t, err, "MountGolden")
			server := httptest.NewServer(handler)
			t.Cleanup(server.Close)

			s := tc.withTest(server.URL, server.Client())
			registry := scraper.NewRegistry(scraper.WithScraperForSite(tc.site, s))

			outputFile := filepath.Join(t.TempDir(), "output.json")

			rootCmd, err := root.Root(t.Context(), root.WithRegistry(registry))
			require.NoError(t, err, "Root")
			require.NotNil(t, rootCmd, "Root")

			err = rootCmd.Run(t.Context(), []string{
				"pdx-watcher", "list-showtimes",
				"--from", tc.fromFlag,
				"--after", "2026-02-01T00:00:00Z",
				"--before", "2026-03-01T00:00:00Z",
				"--output", outputFile,
			})
			require.NoError(t, err, "Run")

			outputBytes, err := os.ReadFile(outputFile)
			require.NoError(t, err, "ReadFile")
			require.NotEmpty(t, outputBytes, "output file should contain showtimes from golden data")
			t.Log(string(outputBytes))
		})
	}
}

package internal

import (
	"context"
	"net/http"
)

type Scraper interface {
	// Descriptor returns the site descriptor (e.g. for cache keys and registry lookup).
	Descriptor() string
	ScrapeShowtimes(ctx context.Context, req ListShowtimesRequest) (<-chan ShowtimeListItem, error)
}

// GoldenScraper extends Scraper with the ability to pull and write golden test data.
type GoldenScraper interface {
	Scraper
	PullGolden(ctx context.Context, goldenDir string) error
	MountGolden(ctx context.Context, goldenDir string) (http.Handler, error)
}

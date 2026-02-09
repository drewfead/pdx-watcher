package scraper

import (
	"context"
	"log/slog"

	"github.com/drewfead/pdx-watcher/internal"
	"github.com/drewfead/pdx-watcher/proto"
)

type noneScraper struct{}

func (s *noneScraper) Descriptor() string {
	return proto.PdxSite_None.Descriptor().Syntax().GoString()
}

func (s *noneScraper) ScrapeShowtimes(
	ctx context.Context,
	req internal.ListShowtimesRequest,
) (<-chan internal.ShowtimeListItem, error) {
	slog.Debug("scrape-showtimes", "descriptor", s.Descriptor(), "request", req)
	ch := make(chan internal.ShowtimeListItem)
	close(ch)
	return ch, nil
}

func None() internal.Scraper {
	return &noneScraper{}
}
